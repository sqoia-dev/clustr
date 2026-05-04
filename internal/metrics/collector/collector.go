// Package collector provides the shared host-metrics collection layer for
// clustr-serverd (control-plane self-monitoring) and clustr-clientd (cluster
// node monitoring).
//
// # Architecture
//
// Both binaries call the same Collect() function, which reads from /proc,
// statfs(2), systemd D-Bus, and chronyc.  The result is a []Sample slice
// using the same type as internal/clientd/stats — the control-plane path
// injects samples directly into the MetricsIngest interface rather than
// shipping them over WebSocket.
//
// # Thread safety
//
// Collector is NOT safe for concurrent use.  The caller (usually a dedicated
// goroutine) owns it exclusively.  The single-goroutine invariant is documented
// on each exported function.
package collector

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/unix"
)

// Sample is a single host metric sample.  Identical in shape to
// internal/clientd/stats.Sample so that the same DB insert path is reused.
type Sample struct {
	Plugin string
	Sensor string
	Value  float64
	Unit   string
	Labels map[string]string
	TS     time.Time
}

// HostID is the stable identifier for the host producing samples.
// For the control plane this is the hosts.id UUID.
// For cluster nodes it is the node_configs.id UUID.
type HostID = string

// Collector gathers host-level metrics in one synchronous pass.
//
// THREAD-SAFETY: not safe for concurrent use.  All callers must guarantee
// single-goroutine access.  State (prevDiskStats, prevTime) is updated on
// each Collect call; concurrent calls would corrupt this state.
type Collector struct {
	// prevDiskIO tracks /proc/diskstats counters for delta computation.
	// Protected by single-goroutine invariant (see doc above).
	prevDiskIO map[string]diskIO
	prevTime   time.Time

	// Injectable paths for testing.
	procMeminfo string
	procMounts  string
	procPressure string // dir: /proc/pressure/
}

// diskIO holds the raw /proc/diskstats counters from the previous tick.
type diskIO struct {
	readSectors  uint64
	writeSectors uint64
}

// New returns a ready-to-use Collector reading from standard /proc paths.
func New() *Collector {
	return &Collector{
		prevDiskIO:   make(map[string]diskIO),
		procMeminfo:  "/proc/meminfo",
		procMounts:   "/proc/mounts",
		procPressure: "/proc/pressure",
	}
}

// Collect runs one full collection pass and returns all samples.
// hostID is embedded in the Plugin field as a routing tag by the serverd
// ingestion path; clientd ignores it (uses the WebSocket connection identity).
//
// THREAD-SAFETY: must be called from a single goroutine.
func (c *Collector) Collect(ctx context.Context) []Sample {
	now := time.Now().UTC()
	var out []Sample

	out = append(out, c.collectMemory(now)...)
	out = append(out, c.collectFilesystems(now)...)
	out = append(out, c.collectPSI(now)...)
	out = append(out, c.collectNTP(ctx, now)...)
	out = append(out, c.collectSystemd(ctx, now)...)

	// Stamp zero timestamps.
	for i := range out {
		if out[i].TS.IsZero() {
			out[i].TS = now
		}
	}
	return out
}

// ─── Memory ──────────────────────────────────────────────────────────────────

func (c *Collector) collectMemory(now time.Time) []Sample {
	fields, err := parseProcMeminfo(c.procMeminfo)
	if err != nil {
		log.Debug().Err(err).Msg("collector: /proc/meminfo read failed")
		return nil
	}

	total := fields["MemTotal"]
	free := fields["MemFree"]
	buffers := fields["Buffers"]
	cached := fields["Cached"]
	sReclaimable := fields["SReclaimable"]

	kbToBytes := func(kb uint64) float64 { return float64(kb) * 1024 }

	usedKB := int64(total) - int64(free) - int64(buffers) - int64(cached) - int64(sReclaimable)
	if usedKB < 0 {
		usedKB = 0
	}

	var samples []Sample
	samples = append(samples,
		Sample{Plugin: "memory", Sensor: "total", Value: kbToBytes(total), Unit: "bytes", TS: now},
		Sample{Plugin: "memory", Sensor: "used", Value: kbToBytes(uint64(usedKB)), Unit: "bytes", TS: now},
		Sample{Plugin: "memory", Sensor: "free", Value: kbToBytes(free), Unit: "bytes", TS: now},
	)
	if total > 0 {
		usedPct := float64(usedKB) / float64(total) * 100.0
		samples = append(samples, Sample{Plugin: "memory", Sensor: "used_pct", Value: usedPct, Unit: "pct", TS: now})
	}
	return samples
}

// ─── Filesystem usage ─────────────────────────────────────────────────────────

func (c *Collector) collectFilesystems(now time.Time) []Sample {
	mounts, err := parseMountsForSelfmon(c.procMounts)
	if err != nil {
		log.Debug().Err(err).Msg("collector: /proc/mounts read failed")
		return nil
	}

	var samples []Sample
	for _, mp := range mounts {
		var st unix.Statfs_t
		if err := unix.Statfs(mp, &st); err != nil {
			continue
		}
		if st.Blocks == 0 {
			continue
		}
		labels := map[string]string{"mount": mp}
		used := st.Blocks - st.Bavail
		usedPct := float64(used) / float64(st.Blocks) * 100.0
		freeBytes := float64(st.Bavail) * float64(st.Bsize)
		totalInodes := st.Files
		freeInodes := st.Ffree
		var inodeUsedPct float64
		if totalInodes > 0 {
			inodeUsedPct = float64(totalInodes-freeInodes) / float64(totalInodes) * 100.0
		}

		samples = append(samples,
			Sample{Plugin: "disks", Sensor: "used_pct", Value: usedPct, Unit: "pct", Labels: labels, TS: now},
			Sample{Plugin: "disks", Sensor: "free_bytes", Value: freeBytes, Unit: "bytes", Labels: labels, TS: now},
			Sample{Plugin: "disks", Sensor: "inode_used_pct", Value: inodeUsedPct, Unit: "pct", Labels: labels, TS: now},
		)
	}
	return samples
}

// ─── PSI (Pressure Stall Information) ────────────────────────────────────────

// collectPSI reads /proc/pressure/{memory,io,cpu} and emits PSI some avg10 samples.
// Silently skips on kernels without PSI support (pre-4.20).
func (c *Collector) collectPSI(now time.Time) []Sample {
	type psiEntry struct {
		resource string
		sensor   string
		line     string // "some" or "full"
		field    string // "avg10", "avg60", "avg300"
	}
	targets := []struct {
		resource string
		sensor   string
	}{
		{"memory", "cp.psi.mem"},
		{"io", "cp.psi.io"},
		{"cpu", "cp.psi.cpu"},
	}

	var samples []Sample
	for _, t := range targets {
		path := filepath.Join(c.procPressure, t.resource)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // PSI not available for this resource
		}
		_ = psiEntry{}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			kind := fields[0] // "some" or "full"
			if kind != "some" {
				continue
			}
			// Parse avg10=<value>
			for _, kv := range fields[1:] {
				parts := strings.SplitN(kv, "=", 2)
				if len(parts) != 2 {
					continue
				}
				if parts[0] != "avg10" {
					continue
				}
				v, err := strconv.ParseFloat(parts[1], 64)
				if err != nil {
					continue
				}
				samples = append(samples, Sample{
					Plugin: "psi",
					Sensor: t.sensor + "_some_avg10",
					Value:  v,
					Unit:   "pct",
					TS:     now,
				})
			}
		}
	}
	return samples
}

// ─── NTP (chronyc tracking) ──────────────────────────────────────────────────

func (c *Collector) collectNTP(ctx context.Context, now time.Time) []Sample {
	chronyPath, err := exec.LookPath("chronyc")
	if err != nil {
		return nil
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, chronyPath, "tracking").Output()
	if err != nil {
		return nil
	}

	var offsetSec float64
	var synced bool
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "System time":
			// "0.000012345 seconds slow of NTP time"
			fields := strings.Fields(val)
			if len(fields) >= 1 {
				if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
					offsetSec = v
					if len(fields) >= 3 && strings.EqualFold(fields[2], "fast") {
						offsetSec = -v
					}
					synced = true
				}
			}
		case "Reference ID":
			// "00000000 ()" means unsynchronised
			if strings.HasPrefix(val, "00000000") {
				synced = false
			}
		}
	}

	syncedVal := 0.0
	if synced {
		syncedVal = 1.0
	}
	return []Sample{
		{Plugin: "ntp", Sensor: "offset_seconds", Value: offsetSec, Unit: "seconds", TS: now},
		{Plugin: "ntp", Sensor: "synced", Value: syncedVal, Unit: "bool", TS: now},
	}
}

// ─── Systemd unit state ───────────────────────────────────────────────────────

// collectSystemd queries systemd for the clustr-serverd unit state.
// Uses os/exec (systemctl show) rather than D-Bus to avoid CGO and keep the
// binary statically linkable. D-Bus via coreos/go-systemd is available for
// future expansion but shells out here for portability and simplicity.
//
// A 5-second context timeout is applied so that a hung dbus / journald under
// disk pressure cannot block the collection goroutine indefinitely (which would
// delay the heartbeat past WatchdogSec and get clustr-serverd killed). On
// timeout the function logs one ERR line and returns nil — other collectors
// still report and the heartbeat is still touched.
func (c *Collector) collectSystemd(ctx context.Context, now time.Time) []Sample {
	const unit = "clustr-serverd.service"
	const timeout = 5 * time.Second

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx,
		"systemctl", "show", unit,
		"--property=ActiveState,NRestarts",
		"--no-pager",
	).Output()
	if err != nil {
		if cmdCtx.Err() != nil {
			// Timed out — log at error level so the operator notices.
			log.Error().
				Dur("timeout", timeout).
				Msg("collectSystemd: systemctl show timed out; skipping systemd samples this cycle")
		}
		// systemctl not available (e.g. in tests or containers), or timed out —
		// return zero values rather than propagating the error so the rest of the
		// collection cycle still runs.
		return nil
	}

	var activeState string
	var nRestarts float64

	for _, line := range strings.Split(string(out), "\n") {
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "ActiveState":
			activeState = strings.TrimSpace(kv[1])
		case "NRestarts":
			if v, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64); err == nil {
				nRestarts = v
			}
		}
	}

	activeVal := 0.0
	if activeState == "active" {
		activeVal = 1.0
	}

	return []Sample{
		{Plugin: "systemd", Sensor: "serverd_active", Value: activeVal, Unit: "bool", TS: now},
		{Plugin: "systemd", Sensor: "serverd_restarts", Value: nRestarts, Unit: "count", TS: now},
	}
}

// ─── Certificate expiry ───────────────────────────────────────────────────────

// CollectCertExpiry scans the given certPaths and emits a "days_until_expiry"
// sample for each certificate found.  Skips unreadable or unparseable files
// with a warning log.
func CollectCertExpiry(certPaths []string, now time.Time) []Sample {
	var samples []Sample
	for _, path := range certPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			log.Debug().Err(err).Str("path", path).Msg("collector: cert read failed")
			continue
		}
		var block *pem.Block
		rest := data
		for {
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			if block.Type != "CERTIFICATE" {
				continue
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			daysLeft := cert.NotAfter.Sub(now).Hours() / 24
			samples = append(samples, Sample{
				Plugin: "certs",
				Sensor: "days_until_expiry",
				Value:  daysLeft,
				Unit:   "days",
				Labels: map[string]string{"path": path},
				TS:     now,
			})
		}
	}
	return samples
}

// ─── Orphan image bytes ───────────────────────────────────────────────────────

// CollectImageOrphans returns the total bytes of orphaned image files under
// imageDir that do not appear in registeredIDs.  registeredIDs is the set of
// image IDs whose blob files are still referenced.
func CollectImageOrphans(imageDir string, registeredIDs map[string]struct{}, now time.Time) []Sample {
	var orphanBytes int64
	entries, err := os.ReadDir(imageDir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Strip common extensions to get the base ID.
		base := strings.TrimSuffix(strings.TrimSuffix(name, ".img"), ".qcow2")
		if _, ok := registeredIDs[base]; !ok {
			if info, err := e.Info(); err == nil {
				orphanBytes += info.Size()
			}
		}
	}
	return []Sample{{
		Plugin: "images",
		Sensor: "orphan_bytes",
		Value:  float64(orphanBytes),
		Unit:   "bytes",
		TS:     now,
	}}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// parseProcMeminfo reads /proc/meminfo and returns field → kB value map.
func parseProcMeminfo(path string) (map[string]uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	result := make(map[string]uint64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		rest := strings.TrimSpace(line[colonIdx+1:])
		rest = strings.TrimSuffix(rest, " kB")
		rest = strings.TrimSpace(rest)
		if v, err := strconv.ParseUint(rest, 10, 64); err == nil {
			result[key] = v
		}
	}
	return result, scanner.Err()
}

// parseMountsForSelfmon returns real filesystem mount points from /proc/mounts.
// We only report the specific paths relevant to clustr's footprint:
// /, /var, /var/lib/clustr, /var/lib/clustr/tmp (build scratch), /tmp.
func parseMountsForSelfmon(procMounts string) ([]string, error) {
	f, err := os.Open(procMounts)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Build a set of real mounts.
	mountSet := make(map[string]bool)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		mp := fields[1]
		fsType := fields[2]
		// Skip pseudo-filesystems.
		switch fsType {
		case "tmpfs", "devtmpfs", "sysfs", "proc", "cgroup", "cgroup2",
			"pstore", "securityfs", "debugfs", "tracefs", "configfs",
			"hugetlbfs", "mqueue", "fusectl", "autofs", "bpf", "overlay":
			continue
		}
		if strings.HasPrefix(mp, "/proc") || strings.HasPrefix(mp, "/sys") ||
			strings.HasPrefix(mp, "/dev") || strings.HasPrefix(mp, "/run") {
			continue
		}
		mountSet[mp] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parseMounts: %w", err)
	}

	// Return all real mounts found.
	result := make([]string, 0, len(mountSet))
	for mp := range mountSet {
		result = append(result, mp)
	}
	return result, nil
}

// TouchHeartbeat writes the current timestamp to path, creating the file and
// any parent directories as needed.  Used by the selfmon watchdog.
func TouchHeartbeat(path string) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Debug().Err(err).Str("path", path).Msg("collector: heartbeat mkdir failed")
		return
	}
	if err := os.WriteFile(path, []byte(strconv.FormatInt(time.Now().Unix(), 10)+"\n"), 0644); err != nil {
		log.Debug().Err(err).Str("path", path).Msg("collector: heartbeat write failed")
	}
}
