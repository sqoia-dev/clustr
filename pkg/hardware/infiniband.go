package hardware

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IBPort represents a single port on an InfiniBand HCA or OPA adapter.
type IBPort struct {
	Number    int    `json:"number"`
	State     string `json:"state"`      // "Active", "Down", "Init"
	PhysState string `json:"phys_state"` // "LinkUp", "Polling", "Disabled"
	Rate      string `json:"rate"`       // "100 Gb/sec", "200 Gb/sec"
	LID       string `json:"lid"`
	SMLID     string `json:"sm_lid"`
	GID       string `json:"gid,omitempty"` // first GID (index 0), populated for RoCE
	LinkLayer string `json:"link_layer"`    // "InfiniBand" or "Ethernet" (RoCE)
}

// IBDevice represents an InfiniBand HCA, OPA adapter, or RoCE interface
// discovered via /sys/class/infiniband/.
type IBDevice struct {
	Name         string   `json:"name"`           // "mlx5_0", "hfi1_0"
	BoardID      string   `json:"board_id"`
	FWVersion    string   `json:"fw_version"`
	NodeGUID     string   `json:"node_guid"`
	SysImageGUID string   `json:"sys_image_guid"`
	Ports        []IBPort `json:"ports"`
}

const ibSysDir = "/sys/class/infiniband"

// DiscoverIBDevices enumerates InfiniBand/OPA/RoCE devices via /sys/class/infiniband/.
// Returns an empty slice (not an error) if the directory does not exist — that simply
// means there is no IB hardware on this node.
func DiscoverIBDevices() ([]IBDevice, error) {
	entries, err := os.ReadDir(ibSysDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []IBDevice{}, nil
		}
		return nil, fmt.Errorf("hardware/infiniband: readdir %s: %w", ibSysDir, err)
	}

	var devices []IBDevice
	for _, e := range entries {
		dev, err := readIBDevice(filepath.Join(ibSysDir, e.Name()), e.Name())
		if err != nil {
			// Partial failure — include what we can.
			devices = append(devices, IBDevice{Name: e.Name()})
			continue
		}
		devices = append(devices, dev)
	}
	return devices, nil
}

// readIBDevice reads all attributes for a single IB device at devPath.
func readIBDevice(devPath, name string) (IBDevice, error) {
	dev := IBDevice{
		Name:         name,
		BoardID:      readSysStr(filepath.Join(devPath, "board_id")),
		FWVersion:    readSysStr(filepath.Join(devPath, "fw_ver")),
		NodeGUID:     readSysStr(filepath.Join(devPath, "node_guid")),
		SysImageGUID: readSysStr(filepath.Join(devPath, "sys_image_guid")),
	}

	ports, err := readIBPorts(filepath.Join(devPath, "ports"))
	if err != nil {
		return dev, fmt.Errorf("ports: %w", err)
	}
	dev.Ports = ports
	return dev, nil
}

// readIBPorts enumerates and reads port attributes from <devPath>/ports/<N>/.
func readIBPorts(portsDir string) ([]IBPort, error) {
	entries, err := os.ReadDir(portsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []IBPort{}, nil
		}
		return nil, fmt.Errorf("readdir %s: %w", portsDir, err)
	}

	var ports []IBPort
	for _, e := range entries {
		portDir := filepath.Join(portsDir, e.Name())

		var portNum int
		fmt.Sscanf(e.Name(), "%d", &portNum)

		port := IBPort{
			Number:    portNum,
			State:     cleanIBState(readSysStr(filepath.Join(portDir, "state"))),
			PhysState: cleanIBState(readSysStr(filepath.Join(portDir, "phys_state"))),
			Rate:      readSysStr(filepath.Join(portDir, "rate")),
			LID:       readSysStr(filepath.Join(portDir, "lid")),
			SMLID:     readSysStr(filepath.Join(portDir, "sm_lid")),
			LinkLayer: readSysStr(filepath.Join(portDir, "link_layer")),
		}

		// GID index 0 is the primary GID; especially useful for RoCE (link_layer=Ethernet).
		port.GID = readSysStr(filepath.Join(portDir, "gids", "0"))

		ports = append(ports, port)
	}
	return ports, nil
}

// cleanIBState strips the leading numeric prefix from IB state strings.
// Kernel sysfs returns states like "4: ACTIVE" or "5: LinkUp" — callers
// want just "ACTIVE" or "LinkUp".
func cleanIBState(s string) string {
	if idx := strings.Index(s, ": "); idx != -1 {
		return strings.TrimSpace(s[idx+2:])
	}
	return strings.TrimSpace(s)
}
