package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// FilesystemDeployer deploys a filesystem-format image (tar archive).
// Flow: download tarball → partition disk → mkfs → mount → extract → unmount.
type FilesystemDeployer struct {
	// partitions holds state set by Preflight for use during Deploy.
	layout     api.DiskLayout
	targetDisk string
}

// Preflight validates disk size and resolves the target disk.
func (d *FilesystemDeployer) Preflight(ctx context.Context, layout api.DiskLayout, hw hardware.SystemInfo) error {
	target, err := selectTargetDisk(layout, hw)
	if err != nil {
		return err
	}
	d.layout = layout
	d.targetDisk = target
	return nil
}

// Deploy partitions the disk, creates filesystems, mounts them, and extracts the tarball.
func (d *FilesystemDeployer) Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	if d.targetDisk == "" {
		return fmt.Errorf("deploy: Preflight must be called before Deploy")
	}

	// Partition the disk.
	if err := d.partitionDisk(ctx, opts.TargetDisk); err != nil {
		return fmt.Errorf("deploy: partition disk: %w", err)
	}

	// Create filesystems.
	partDevs, err := d.createFilesystems(ctx, opts.TargetDisk)
	if err != nil {
		return fmt.Errorf("deploy: create filesystems: %w", err)
	}

	// Mount partitions.
	if err := d.mountPartitions(ctx, partDevs, opts.MountRoot); err != nil {
		return fmt.Errorf("deploy: mount partitions: %w", err)
	}
	// Always attempt unmount on exit.
	defer d.unmountAll(opts.MountRoot)

	// Download and extract.
	if err := d.downloadAndExtract(ctx, opts, progress); err != nil {
		return fmt.Errorf("deploy: extract: %w", err)
	}

	return nil
}

// Finalize applies node-specific identity to the deployed filesystem.
func (d *FilesystemDeployer) Finalize(ctx context.Context, cfg api.NodeConfig, mountRoot string) error {
	return applyNodeConfig(ctx, cfg, mountRoot)
}

// partitionDisk wipes and repartitions the disk according to the layout.
// Uses sgdisk for GPT layouts (standard for EFI systems).
func (d *FilesystemDeployer) partitionDisk(ctx context.Context, disk string) error {
	// Wipe existing partition table.
	if err := runCmd(ctx, "sgdisk", "--zap-all", disk); err != nil {
		// Fall back to wipefs if sgdisk is unavailable.
		if err2 := runCmd(ctx, "wipefs", "-a", disk); err2 != nil {
			return fmt.Errorf("wipe disk: %w (sgdisk: %v, wipefs: %v)", ErrPreflightFailed, err, err2)
		}
	}

	// Build sgdisk partition arguments.
	args := []string{}
	for i, p := range d.layout.Partitions {
		num := i + 1
		var sizeSpec string
		if p.SizeBytes == 0 {
			sizeSpec = "0" // fill remaining
		} else {
			sizeSpec = fmt.Sprintf("+%dK", p.SizeBytes/1024)
		}
		args = append(args, fmt.Sprintf("--new=%d:0:%s", num, sizeSpec))

		if p.Label != "" {
			args = append(args, fmt.Sprintf("--change-name=%d:%s", num, p.Label))
		}
		// Set partition type GUID for ESP.
		for _, flag := range p.Flags {
			if flag == "esp" || flag == "boot" {
				args = append(args, fmt.Sprintf("--typecode=%d:ef00", num))
			}
		}
	}
	args = append(args, disk)

	if err := runCmd(ctx, "sgdisk", args...); err != nil {
		return fmt.Errorf("sgdisk partition: %w", err)
	}

	// Allow kernel to re-read the new partition table.
	_ = runCmd(ctx, "partprobe", disk)
	_ = runCmd(ctx, "udevadm", "settle")

	return nil
}

// partitionDevice returns the partition device path for a given disk and number.
// Handles both nvme-style (nvme0n1p1) and sda-style (sda1) naming.
func partitionDevice(disk string, num int) string {
	if strings.Contains(disk, "nvme") || strings.Contains(disk, "mmcblk") {
		return fmt.Sprintf("%sp%d", disk, num)
	}
	return fmt.Sprintf("%s%d", disk, num)
}

// createFilesystems creates the appropriate filesystem on each partition.
// Returns a slice of resolved partition device paths in layout order.
func (d *FilesystemDeployer) createFilesystems(ctx context.Context, disk string) ([]string, error) {
	devs := make([]string, len(d.layout.Partitions))
	for i, p := range d.layout.Partitions {
		dev := partitionDevice(disk, i+1)
		devs[i] = dev

		var mkfsArgs []string
		var mkfsBin string
		switch p.Filesystem {
		case "vfat", "fat32", "fat":
			mkfsBin = "mkfs.vfat"
			mkfsArgs = []string{"-F", "32", dev}
		case "ext4":
			mkfsBin = "mkfs.ext4"
			mkfsArgs = []string{"-F", dev}
		case "xfs":
			mkfsBin = "mkfs.xfs"
			mkfsArgs = []string{"-f", dev}
		case "swap":
			mkfsBin = "mkswap"
			mkfsArgs = []string{dev}
		case "":
			// No filesystem (raw partition, e.g. BIOS boot).
			continue
		default:
			return nil, fmt.Errorf("unsupported filesystem %q for partition %d", p.Filesystem, i+1)
		}

		if err := runCmd(ctx, mkfsBin, mkfsArgs...); err != nil {
			return nil, fmt.Errorf("mkfs partition %d (%s): %w", i+1, p.Filesystem, err)
		}
	}
	return devs, nil
}

// mountPartitions mounts all non-swap partitions into the mountRoot hierarchy.
// Partitions are mounted in layout order (root first, then subdirs).
func (d *FilesystemDeployer) mountPartitions(ctx context.Context, devs []string, mountRoot string) error {
	// Sort by mountpoint depth so / is mounted before /boot, /boot before /boot/efi.
	type mp struct {
		dev   string
		mount string
		fs    string
	}
	var mps []mp
	for i, p := range d.layout.Partitions {
		if p.MountPoint == "" || p.Filesystem == "swap" {
			continue
		}
		mps = append(mps, mp{dev: devs[i], mount: p.MountPoint, fs: p.Filesystem})
	}

	// Mount in order (layout is expected to be ordered root → boot → esp).
	for _, m := range mps {
		target := filepath.Join(mountRoot, m.mount)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", target, err)
		}
		if err := runCmd(ctx, "mount", m.dev, target); err != nil {
			return fmt.Errorf("mount %s → %s: %w", m.dev, target, err)
		}
	}
	return nil
}

// unmountAll unmounts everything under mountRoot in reverse order (deepest first).
func (d *FilesystemDeployer) unmountAll(mountRoot string) {
	// umount -R handles reverse-order unmounting for nested mounts.
	_ = exec.Command("umount", "-R", mountRoot).Run()
}

// downloadAndExtract downloads the tarball from opts.ImageURL and extracts it
// into opts.MountRoot. Progress is reported via opts progress callback.
func (d *FilesystemDeployer) downloadAndExtract(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	if opts.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}

	totalBytes := resp.ContentLength

	// Pipe the download directly into tar for extraction — avoids requiring
	// enough temp disk to hold the full tarball.
	tarCmd := exec.CommandContext(ctx, "tar", "-xzf", "-", "-C", opts.MountRoot)
	tarCmd.Stdin = &progressReader{r: resp.Body, total: totalBytes, fn: progress, phase: "extracting"}

	if out, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w\noutput: %s", err, string(out))
	}

	return nil
}

// progressReader wraps an io.Reader and calls a ProgressFunc on each read.
type progressReader struct {
	r       io.Reader
	total   int64
	written int64
	fn      ProgressFunc
	phase   string
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.written += int64(n)
	if p.fn != nil {
		p.fn(p.written, p.total, p.phase)
	}
	return n, err
}
