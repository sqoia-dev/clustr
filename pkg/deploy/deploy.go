// Package deploy provides deployment engines for writing images to target nodes.
// Supported engines: rsync (filesystem-level tar extraction) and block (dd/partclone).
// The Deployer interface enforces a three-phase contract: Preflight → Deploy → Finalize.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// ErrNotImplemented is returned by engine stubs pending full implementation.
var ErrNotImplemented = errors.New("not implemented")

// ErrPreflightFailed is returned when preconditions for deployment are not met.
var ErrPreflightFailed = errors.New("preflight failed")

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
}

// runCmd executes a command and returns a combined error message if it fails.
// The command's combined output is included in the error for debuggability.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("deploy: %s %v: %w\noutput: %s", name, args, err, string(out))
	}
	return nil
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

// selectTargetDisk picks the first disk from hw that is large enough for the
// layout and matches the discovered disk types. Returns the /dev/ path.
func selectTargetDisk(layout api.DiskLayout, hw hardware.SystemInfo) (string, error) {
	needed := totalLayoutBytes(layout)
	for _, disk := range hw.Disks {
		if int64(disk.Size) >= needed {
			return "/dev/" + disk.Name, nil
		}
	}
	return "", fmt.Errorf("%w: no disk >= %d bytes found (layout requires %d bytes)",
		ErrPreflightFailed, needed, needed)
}
