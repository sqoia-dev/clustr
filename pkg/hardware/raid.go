package hardware

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// MDArray represents a Linux software RAID array managed by mdadm.
type MDArray struct {
	Name       string   `json:"name"`              // "md0", "md1"
	Path       string   `json:"path"`              // "/dev/md0"
	Level      string   `json:"level"`             // "raid0", "raid1", "raid5", "raid6", "raid10"
	State      string   `json:"state"`             // "active", "degraded", "rebuilding"
	Size       uint64   `json:"size_bytes"`
	ChunkKB    int      `json:"chunk_kb"`
	Members    []string `json:"members"`           // ["/dev/sda1", "/dev/sdb1"]
	Filesystem string   `json:"filesystem,omitempty"`
	MountPoint string   `json:"mountpoint,omitempty"`
	UUID       string   `json:"uuid,omitempty"`
}

// mdstatReader is a package-level variable so tests can substitute a fake reader.
var mdstatReader = func() ([]byte, error) {
	return os.ReadFile("/proc/mdstat")
}

// DiscoverMDArrays discovers Linux software RAID arrays by reading /proc/mdstat
// and cross-referencing with sysfs. Returns an empty slice if there are no arrays.
func DiscoverMDArrays() ([]MDArray, error) {
	return discoverMDArraysFromSources(mdstatReader, lsblkRunner)
}

func discoverMDArraysFromSources(mdstatFn func() ([]byte, error), lsblkFn func() ([]byte, error)) ([]MDArray, error) {
	raw, err := mdstatFn()
	if err != nil {
		if os.IsNotExist(err) {
			return []MDArray{}, nil
		}
		return nil, fmt.Errorf("hardware/raid: read /proc/mdstat: %w", err)
	}

	arrays, err := parseMdstat(raw)
	if err != nil {
		return nil, err
	}

	// Enrich each array with sysfs attributes.
	for i := range arrays {
		enrichFromSysfs(&arrays[i])
	}

	// Cross-reference lsblk for filesystem and mountpoint information.
	lsblkRaw, err := lsblkFn()
	if err == nil {
		enrichFromLsblk(arrays, lsblkRaw)
	}

	return arrays, nil
}

// parseMdstat parses the content of /proc/mdstat and returns the array list.
// It handles active, degraded, and rebuilding arrays.
func parseMdstat(raw []byte) ([]MDArray, error) {
	var arrays []MDArray
	var current *MDArray

	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := scanner.Text()

		// Array header line: "md0 : active raid1 sda1[0] sdb1[1]"
		if strings.HasPrefix(line, "md") && strings.Contains(line, ":") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}

			name := fields[0]
			arr := MDArray{
				Name: name,
				Path: "/dev/" + name,
			}

			// Parse state and level from the colon-separated part.
			colonIdx := strings.Index(line, ":")
			rest := strings.TrimSpace(line[colonIdx+1:])
			parts := strings.Fields(rest)

			if len(parts) >= 1 {
				arr.State = parts[0]
			}
			if len(parts) >= 2 {
				arr.Level = parts[1]
			}

			// Collect member devices (sda1[0], sdb1[1](F), etc.)
			for _, p := range parts[2:] {
				devName := p
				if bracketIdx := strings.Index(devName, "["); bracketIdx != -1 {
					devName = devName[:bracketIdx]
				}
				if parenIdx := strings.Index(devName, "("); parenIdx != -1 {
					devName = devName[:parenIdx]
				}
				if devName != "" {
					arr.Members = append(arr.Members, "/dev/"+devName)
				}
			}

			arrays = append(arrays, arr)
			current = &arrays[len(arrays)-1]
			continue
		}

		// Size line: "      1953382400 blocks super 1.2 [2/2] [UU]"
		if current != nil && strings.Contains(line, "blocks") {
			fields := strings.Fields(line)
			if len(fields) > 0 {
				if n, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
					// /proc/mdstat reports size in 1-KiB blocks.
					current.Size = n * 1024
				}
			}
		}

		// Chunk size line: "      512K chunks, algorithm 2"
		if current != nil && strings.Contains(line, "chunk") {
			for _, f := range strings.Fields(line) {
				upper := strings.ToUpper(f)
				if strings.HasSuffix(upper, "K") {
					val := strings.TrimSuffix(upper, "K")
					if n, err := strconv.Atoi(val); err == nil {
						current.ChunkKB = n
					}
				}
			}
		}

		// Empty line signals end of current array block.
		if strings.TrimSpace(line) == "" {
			current = nil
		}
	}

	return arrays, nil
}

// enrichFromSysfs reads additional attributes for an array from /sys/block/<name>/md/.
func enrichFromSysfs(arr *MDArray) {
	sysBase := filepath.Join("/sys/block", arr.Name, "md")

	// Array state (may be more precise than mdstat).
	if s := readSysStr(filepath.Join(sysBase, "array_state")); s != "" {
		arr.State = normalizeArrayState(s)
	}

	// RAID level.
	if lvl := readSysStr(filepath.Join(sysBase, "level")); lvl != "" {
		arr.Level = lvl
	}

	// UUID.
	if uuid := readSysStr(filepath.Join(sysBase, "uuid")); uuid != "" {
		arr.UUID = uuid
	}

	// Chunk size in bytes from sysfs, convert to KB.
	if cs := readSysStr(filepath.Join(sysBase, "chunk_size")); cs != "" {
		if n, err := strconv.ParseUint(cs, 10, 64); err == nil && n > 0 {
			arr.ChunkKB = int(n / 1024)
		}
	}

	// Member devices from dev-* symlinks under /sys/block/<name>/md/.
	members := readMDMembers(sysBase)
	if len(members) > 0 {
		arr.Members = members
	}
}

// readMDMembers reads member device paths from /sys/block/<name>/md/dev-* entries.
func readMDMembers(sysBase string) []string {
	entries, err := os.ReadDir(sysBase)
	if err != nil {
		return nil
	}

	var members []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "dev-") {
			continue
		}
		// Resolve the block symlink inside dev-<name>/block → actual device.
		blockLink := filepath.Join(sysBase, e.Name(), "block")
		target, err := os.Readlink(blockLink)
		if err != nil {
			// Fallback: derive device name from directory name.
			// Kernel encodes '/' as '!' for hierarchical names (nvme0n1p1 → dev-nvme0n1p1).
			devName := strings.TrimPrefix(e.Name(), "dev-")
			devName = strings.ReplaceAll(devName, "!", "/")
			members = append(members, "/dev/"+devName)
			continue
		}
		// target is something like "../../sda1" — take the last path component.
		devName := filepath.Base(target)
		members = append(members, "/dev/"+devName)
	}
	return members
}

// allLsblkOutput is a superset of lsblkOutput that does not filter by type.
type allLsblkOutput struct {
	Blockdevices []lsblkDevice `json:"blockdevices"`
}

// enrichFromLsblk merges filesystem and mountpoint info from lsblk output into
// the discovered arrays. It walks all block devices (not just "disk" type) to
// find md devices.
func enrichFromLsblk(arrays []MDArray, lsblkRaw []byte) {
	var all allLsblkOutput
	if err := json.Unmarshal(lsblkRaw, &all); err != nil {
		return
	}

	// Build a flat name → device map from top-level and children.
	devMap := make(map[string]lsblkDevice)
	var flatten func(devs []lsblkDevice)
	flatten = func(devs []lsblkDevice) {
		for _, dev := range devs {
			devMap[dev.Name] = dev
			flatten(dev.Children)
		}
	}
	flatten(all.Blockdevices)

	for i := range arrays {
		if dev, ok := devMap[arrays[i].Name]; ok {
			arrays[i].Filesystem = dev.FSType
			arrays[i].MountPoint = dev.MountPoint
		}
	}
}

// normalizeArrayState maps sysfs array_state values to human-readable labels.
func normalizeArrayState(s string) string {
	switch s {
	case "active", "active-idle", "clean":
		return "active"
	case "degraded":
		return "degraded"
	case "resyncing", "recovering", "reshaping", "check", "repair":
		return "rebuilding"
	default:
		return s
	}
}
