package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// BlockDeployer deploys a raw block image directly to a disk.
// It supports two modes:
//   - streaming: pipes the HTTP download directly into dd (no temp file needed)
//   - buffered: downloads to a temp file first, then writes with dd
//
// Streaming is the default — it avoids requiring disk space equal to the image size.
type BlockDeployer struct {
	// layout and targetDisk are resolved by Preflight.
	layout     api.DiskLayout
	targetDisk string
}

// Preflight validates that a suitable target disk exists and resolves its path.
func (d *BlockDeployer) Preflight(ctx context.Context, layout api.DiskLayout, hw hardware.SystemInfo) error {
	target, err := selectTargetDisk(layout, hw)
	if err != nil {
		return err
	}
	d.layout = layout
	d.targetDisk = target
	return nil
}

// Deploy streams the block image from opts.ImageURL and writes it directly
// to the target disk using dd. No intermediate temp file is created.
// The target disk is used from d.targetDisk (set by Preflight) unless
// opts.TargetDisk is explicitly set.
func (d *BlockDeployer) Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	disk := opts.TargetDisk
	if disk == "" {
		disk = d.targetDisk
	}
	if disk == "" {
		return fmt.Errorf("deploy/block: Preflight must be called before Deploy")
	}

	// Build the HTTP download request.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
	if err != nil {
		return fmt.Errorf("deploy/block: build request: %w", err)
	}
	if opts.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("deploy/block: download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("deploy/block: download: HTTP %d", resp.StatusCode)
	}

	totalBytes := resp.ContentLength

	// Open target disk for writing.
	// O_SYNC ensures that data is flushed to the physical device before dd exits.
	f, err := os.OpenFile(disk, os.O_WRONLY|os.O_SYNC, 0o600)
	if err != nil {
		return fmt.Errorf("deploy/block: open disk %s: %w", disk, err)
	}
	defer f.Close()

	// Stream download → disk with progress reporting.
	pr := &progressReader{
		r:     resp.Body,
		total: totalBytes,
		fn:    progress,
		phase: "writing",
	}

	// Use a 4MB copy buffer — dd default (512 bytes) is too small for network I/O.
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(f, pr, buf); err != nil {
		return fmt.Errorf("deploy/block: write to %s: %w", disk, err)
	}

	// Sync buffers to disk.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("deploy/block: sync %s: %w", disk, err)
	}

	// Re-read the partition table after writing.
	_ = exec.CommandContext(ctx, "partprobe", disk).Run()
	_ = exec.CommandContext(ctx, "udevadm", "settle").Run()

	return nil
}

// Finalize applies node-specific configuration to the deployed filesystem.
// For block images, the partitions must be mounted first. This method mounts
// the root partition at mountRoot, applies config, then unmounts.
func (d *BlockDeployer) Finalize(ctx context.Context, cfg api.NodeConfig, mountRoot string) error {
	if mountRoot == "" {
		return fmt.Errorf("deploy/block: mountRoot is required for Finalize")
	}

	// Mount the root partition (first partition with mountpoint "/").
	rootDev := ""
	for i, p := range d.layout.Partitions {
		if p.MountPoint == "/" {
			rootDev = partitionDevice(d.targetDisk, i+1)
			break
		}
	}
	if rootDev == "" && len(d.layout.Partitions) > 0 {
		// Fall back to first partition if no explicit "/" mountpoint.
		rootDev = partitionDevice(d.targetDisk, 1)
	}
	if rootDev == "" {
		return fmt.Errorf("deploy/block: cannot determine root partition for Finalize")
	}

	if err := os.MkdirAll(mountRoot, 0o755); err != nil {
		return fmt.Errorf("deploy/block: mkdir mountRoot: %w", err)
	}
	if err := runCmd(ctx, "mount", rootDev, mountRoot); err != nil {
		return fmt.Errorf("deploy/block: mount root %s: %w", rootDev, err)
	}
	defer func() {
		_ = exec.Command("umount", mountRoot).Run()
	}()

	return applyNodeConfig(ctx, cfg, mountRoot)
}
