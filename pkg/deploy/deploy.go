// Package deploy provides deployment engines for writing images to target nodes.
// Supported engines: rsync (filesystem-level tar extraction) and block (dd/partclone).
// The Deployer interface enforces a three-phase contract: Preflight → Deploy → Finalize.
package deploy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/rs/zerolog"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// pkgLogger is the package-level zerolog logger used by deploy helpers.
// It defaults to a no-op logger. Call SetLogger from main.go after building deployLog.
var (
	pkgLogMu sync.RWMutex
	pkgLog   = zerolog.Nop()
)

// SetLogger sets the zerolog logger used by the deploy package for subprocess
// output streaming and phase progress messages. Call this once before running
// any deployment, passing the same logger used for the deploy command.
func SetLogger(l zerolog.Logger) {
	pkgLogMu.Lock()
	pkgLog = l
	pkgLogMu.Unlock()
}

// logger returns a pointer to a snapshot of the current package logger.
// A pointer is returned so callers can use zerolog's pointer-receiver methods
// (Info, Warn, Error, etc.) directly on the returned value without needing
// to assign to a local variable first.
func logger() *zerolog.Logger {
	pkgLogMu.RLock()
	l := pkgLog
	pkgLogMu.RUnlock()
	return &l
}

// runAndLog executes cmd and streams each line of stdout and stderr to the
// package logger at Info level with the command name and stream as fields.
// Returns an error if the process exits non-zero (error message includes the
// last few lines of combined output for context).
func runAndLog(ctx context.Context, name string, cmd *exec.Cmd) error {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("deploy: %s: stdout pipe: %w", name, err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("deploy: %s: stderr pipe: %w", name, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("deploy: %s: start: %w", name, err)
	}

	log := logger()

	// Stream stdout and stderr in parallel, collecting the last 20 lines for
	// error context in case the process exits non-zero.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		tailBuf []string
	)

	scanStream := func(r io.Reader, stream string) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			log.Info().Str("cmd", name).Str("stream", stream).Msg(line)
			mu.Lock()
			tailBuf = append(tailBuf, stream+": "+line)
			if len(tailBuf) > 20 {
				tailBuf = tailBuf[len(tailBuf)-20:]
			}
			mu.Unlock()
		}
	}

	wg.Add(2)
	go scanStream(stdoutPipe, "stdout")
	go scanStream(stderrPipe, "stderr")
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return fmt.Errorf("deploy: %s exited with code %d\nlast output:\n%s",
			name, exitCode, strings.Join(tailBuf, "\n"))
	}
	return nil
}

// ErrNotImplemented is returned by engine stubs pending full implementation.
var ErrNotImplemented = errors.New("not implemented")

// ErrPreflightFailed is returned when preconditions for deployment are not met.
var ErrPreflightFailed = errors.New("preflight failed")

// BootloaderError is returned by Finalize when grub2-install fails on ALL
// target disks for a BIOS deployment. A partial failure (at least one raw
// disk succeeded) is survivable for RAID1 and is only logged as a warning.
//
// The caller (cmd/clonr/main.go) maps this to ExitBootloader so the
// deploy-failed callback carries the correct exit code and phase, and the
// deploy-complete callback is never fired.
type BootloaderError struct {
	// Targets is the list of raw disk devices that were attempted.
	Targets []string
	// Cause is the last non-nil error from a grub2-install invocation.
	Cause error
}

func (e *BootloaderError) Error() string {
	return fmt.Sprintf("grub2-install failed on all target disks %v: %v", e.Targets, e.Cause)
}

func (e *BootloaderError) Unwrap() error { return e.Cause }

// ProgressFunc is called periodically during a Deploy operation to report progress.
// phase is a human-readable label such as "downloading", "partitioning", "writing".
type ProgressFunc func(bytesWritten, totalBytes int64, phase string)

// DeployOpts holds the resolved parameters for a single deployment run.
type DeployOpts struct {
	// ImageURL is the full URL to download the image blob from.
	ImageURL string
	// AuthToken is the Bearer token sent with the blob download request.
	AuthToken string
	// TargetDisk is the resolved block device path, e.g. /dev/nvme0n1.
	// Set by Preflight based on DiskLayout constraints and hardware profile.
	TargetDisk string
	// Format is "filesystem" (tar archive) or "block" (raw image).
	Format string
	// MountRoot is the temporary directory where partitions are mounted
	// during a filesystem-format deployment. Unused for block deployments.
	MountRoot string
	// NoRollback disables partition table backup/restore on failure.
	// Use this when intentionally wiping a disk that has no meaningful state.
	NoRollback bool
	// SkipVerify skips image checksum verification after download.
	SkipVerify bool
	// ExpectedChecksum is the sha256 hex string to verify the downloaded blob against.
	// When empty and SkipVerify is false, verification is skipped with a warning.
	ExpectedChecksum string
	// Reporter is the optional structured progress reporter used to send
	// real-time byte-level progress updates to the server.
	// When nil, progress is reported only via ProgressFunc and logging.
	Reporter ProgressReporter
}

// ProgressReporter is the interface used by deploy engines to emit structured
// progress events. Implemented by client.ProgressReporter; may be nil-safe.
type ProgressReporter interface {
	StartPhase(phase string, total int64)
	Update(bytesDone int64)
	EndPhase(errMsg string)
	Complete()
	Fail(err error)
}

// Deployer is the interface implemented by all deployment backends.
// Callers must invoke Preflight before Deploy, and Deploy before Finalize.
type Deployer interface {
	// Preflight validates that the target hardware can accept this deployment.
	// It resolves the target disk and writes it into opts.TargetDisk.
	Preflight(ctx context.Context, layout api.DiskLayout, hw hardware.SystemInfo) error

	// Deploy downloads the image and writes it to the target disk.
	// progress is called periodically and may be nil.
	Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error

	// Finalize applies node-specific identity (hostname, network, SSH keys)
	// to the freshly deployed filesystem rooted at mountRoot.
	Finalize(ctx context.Context, cfg api.NodeConfig, mountRoot string) error

	// ResolvedDisk returns the target disk path resolved by Preflight, e.g.
	// /dev/sda or /dev/nvme0n1. Returns "" if Preflight has not been called.
	ResolvedDisk() string
}

// runCmd executes a command and streams its output through the package logger.
// Returns an error (including tail of output) if the process exits non-zero.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	return runAndLog(ctx, name, cmd)
}

// diskSizeBytes returns the size of a block device in bytes by reading
// /sys/class/block/<name>/size (512-byte sectors).
func diskSizeBytes(devPath string) (int64, error) {
	// Use blockdev --getsize64 for a direct byte count — simpler and cross-distro.
	cmd := exec.Command("blockdev", "--getsize64", devPath)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("deploy: blockdev --getsize64 %s: %w", devPath, err)
	}
	var size int64
	if _, err := fmt.Sscanf(string(out), "%d", &size); err != nil {
		return 0, fmt.Errorf("deploy: parse disk size for %s: %w", devPath, err)
	}
	return size, nil
}

// totalLayoutBytes returns the sum of all fixed-size partitions in the layout.
// Partitions with SizeBytes == 0 (fill-remaining) are excluded from the sum
// but imply that any remaining space is consumed.
func totalLayoutBytes(layout api.DiskLayout) int64 {
	var total int64
	for _, p := range layout.Partitions {
		total += p.SizeBytes
	}
	return total
}

// isBootDisk returns true if any partition on the disk is mounted at "/" or "/boot".
// This identifies the currently running system disk, which must not be overwritten.
func isBootDisk(disk hardware.Disk) bool {
	for _, p := range disk.Partitions {
		mp := strings.TrimSpace(p.MountPoint)
		if mp == "/" || mp == "/boot" || mp == "/boot/efi" {
			return true
		}
	}
	return false
}

// backupPartitionTable saves the existing GPT/MBR partition table to a temp file
// using sgdisk --backup. Returns the path to the backup file (caller must remove
// on success). Returns ("", nil) if the disk appears to have no partition table
// (empty disk) — in that case rollback is not possible and the caller should log
// accordingly rather than treating it as an error.
func backupPartitionTable(disk string) (backupPath string, isEmpty bool, err error) {
	// Check whether the disk has any existing partition table by probing with
	// sgdisk --print. Exit code 2 means "no partition table found".
	probe := exec.Command("sgdisk", "--print", disk)
	probeOut, probeErr := probe.CombinedOutput()
	if probeErr != nil {
		// sgdisk exits non-zero when no GPT is found. Check for "not found" indicator.
		if strings.Contains(string(probeOut), "doesn't contain a valid partition table") ||
			strings.Contains(string(probeOut), "Problem opening") ||
			probe.ProcessState != nil && probe.ProcessState.ExitCode() == 2 {
			return "", true, nil
		}
		// Could be a valid MBR disk or other condition — still attempt backup.
	}

	f, err := os.CreateTemp("", "clonr-ptbackup-*.sgdisk")
	if err != nil {
		return "", false, fmt.Errorf("rollback: create backup temp file: %w", err)
	}
	f.Close()
	backupPath = f.Name()

	cmd := exec.Command("sgdisk", "--backup="+backupPath, disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.Remove(backupPath)
		return "", false, fmt.Errorf("rollback: sgdisk --backup %s: %w\noutput: %s", disk, err, string(out))
	}

	return backupPath, false, nil
}

// restorePartitionTable restores a previously saved partition table backup.
// backupPath must be a file written by sgdisk --backup. The file is removed
// after a successful restore.
func restorePartitionTable(disk, backupPath string) error {
	defer os.Remove(backupPath)

	cmd := exec.Command("sgdisk", "--load-backup="+backupPath, disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rollback: sgdisk --load-backup %s: %w\noutput: %s", disk, err, string(out))
	}

	// Re-read partition table so the kernel reflects the restored layout.
	_ = exec.Command("partprobe", disk).Run()
	_ = exec.Command("udevadm", "settle").Run()

	return nil
}

// selectTargetDisk picks the best disk from hw for the given layout.
//
// Selection priority (highest to lowest):
//  1. layout.TargetDevice hint — if set, use that device if it exists and is large enough.
//  2. Exclude the active boot disk (any disk with "/" or "/boot" mounted).
//  3. Prefer non-removable, non-USB disks.
//  4. Among remaining candidates, pick the smallest disk that still fits
//     (avoids accidentally wiping a large data disk).
//
// The selected disk and the reason for the choice are logged for operator audit.
func selectTargetDisk(layout api.DiskLayout, hw hardware.SystemInfo) (string, error) {
	needed := totalLayoutBytes(layout)

	// 1. Honor an explicit target_device hint from the layout.
	if layout.TargetDevice != "" {
		for _, disk := range hw.Disks {
			if disk.Name == layout.TargetDevice {
				if int64(disk.Size) < needed {
					return "", fmt.Errorf("%w: hinted disk %s (%d bytes) is smaller than layout requires (%d bytes)",
						ErrPreflightFailed, disk.Name, disk.Size, needed)
				}
				devPath := "/dev/" + disk.Name
				logger().Info().Str("disk", devPath).Msg("selected disk (reason: target_device hint in layout)")
				return devPath, nil
			}
		}
		return "", fmt.Errorf("%w: hinted target_device %q not found in discovered disks",
			ErrPreflightFailed, layout.TargetDevice)
	}

	// Collect candidates: disks that are large enough and not the boot disk.
	type candidate struct {
		disk   hardware.Disk
		reason string
	}
	var preferred []candidate // non-removable, non-USB
	var fallback []candidate  // removable or USB (lower preference)

	for _, disk := range hw.Disks {
		if int64(disk.Size) < needed {
			continue // too small
		}
		if isBootDisk(disk) {
			logger().Info().Str("disk", disk.Name).Msg("skipping disk (boot disk — has / or /boot mounted)")
			continue
		}
		transport := strings.ToLower(disk.Transport)
		if transport == "usb" {
			fallback = append(fallback, candidate{disk, "usb/removable"})
		} else {
			preferred = append(preferred, candidate{disk, "non-removable, non-USB"})
		}
	}

	pool := preferred
	if len(pool) == 0 {
		pool = fallback
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("%w: no disk >= %d bytes found that is not the active boot disk",
			ErrPreflightFailed, needed)
	}

	// 4. Pick the smallest disk that fits among the pool.
	best := pool[0]
	for _, c := range pool[1:] {
		if c.disk.Size < best.disk.Size {
			best = c
		}
	}

	devPath := "/dev/" + best.disk.Name
	logger().Info().Str("disk", devPath).Int64("size_bytes", int64(best.disk.Size)).
		Str("reason", "smallest fitting "+best.reason+" disk").Msg("selected target disk")
	return devPath, nil
}
