package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

	// Validate that the selected disk is large enough for the layout, producing
	// an actionable error message that names both the disk and the image requirement.
	diskSize, sizeErr := diskSizeBytes(target)
	if sizeErr == nil {
		needed := totalLayoutBytes(layout)
		if needed > 0 && diskSize < needed {
			return fmt.Errorf("%w: disk %s is too small (%s) — layout requires at least %s minimum",
				ErrPreflightFailed, target,
				humanReadableBytes(diskSize), humanReadableBytes(needed))
		}
	}

	d.layout = layout
	d.targetDisk = target
	return nil
}

// humanReadableBytes formats bytes as a human-readable string (e.g. "40GB", "512MB").
func humanReadableBytes(b int64) string {
	const gb = 1 << 30
	const mb = 1 << 20
	switch {
	case b >= gb:
		return fmt.Sprintf("%.0fGB", math.Round(float64(b)/gb))
	case b >= mb:
		return fmt.Sprintf("%.0fMB", math.Round(float64(b)/mb))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// Deploy partitions the disk, creates filesystems, mounts them, and extracts the tarball.
func (d *FilesystemDeployer) Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	if d.targetDisk == "" {
		return fmt.Errorf("deploy: Preflight must be called before Deploy")
	}

	disk := opts.TargetDisk
	if disk == "" {
		disk = d.targetDisk
	}

	// ── Rollback setup ────────────────────────────────────────────────────────
	var rollbackPath string
	if !opts.NoRollback {
		backup, empty, err := backupPartitionTable(disk)
		if err != nil {
			// Non-fatal: log and continue. We still proceed, just without rollback.
			log.Printf("deploy: WARNING: could not back up partition table on %s: %v — proceeding without rollback", disk, err)
		} else if empty {
			log.Printf("deploy: disk %s has no existing partition table — no rollback possible if deployment fails", disk)
		} else {
			rollbackPath = backup
			log.Printf("deploy: partition table backup saved to %s (will restore on failure)", rollbackPath)
		}
	}

	// doRollback restores the partition table if a backup was taken.
	doRollback := func(reason string) {
		if rollbackPath == "" {
			return
		}
		log.Printf("deploy: ROLLBACK triggered (%s) — restoring partition table on %s", reason, disk)
		if err := restorePartitionTable(disk, rollbackPath); err != nil {
			log.Printf("deploy: ROLLBACK FAILED: %v — disk %s may be in an inconsistent state; re-run deployment to recover", err, disk)
		} else {
			log.Printf("deploy: rollback complete — partition table on %s restored to pre-deployment state", disk)
			rollbackPath = "" // already removed by restorePartitionTable
		}
	}

	// Emit progress: partitioning phase.
	if progress != nil {
		progress(0, 0, "partitioning")
	}

	// Create RAID arrays before partitioning, if any are specified.
	if len(d.layout.RAIDArrays) > 0 {
		if err := CreateRAIDArrays(ctx, d.layout, hardware.SystemInfo{}); err != nil {
			doRollback("RAID array creation failed")
			return fmt.Errorf("deploy: create raid arrays: %w", err)
		}
	}

	// Partition the disk.
	if err := d.partitionDisk(ctx, disk); err != nil {
		doRollback("partitioning failed")
		return err // partitionDisk already produces an actionable error
	}

	// Emit progress: formatting phase.
	if progress != nil {
		progress(0, 0, "formatting")
	}

	// Create filesystems.
	partDevs, err := d.createFilesystems(ctx, disk)
	if err != nil {
		doRollback("filesystem creation failed")
		return fmt.Errorf("deploy: create filesystems: %w", err)
	}

	// Mount partitions.
	if err := d.mountPartitions(ctx, partDevs, opts.MountRoot); err != nil {
		doRollback("partition mount failed")
		return fmt.Errorf("deploy: mount partitions: %w", err)
	}
	// Always attempt unmount on exit.
	defer d.unmountAll(opts.MountRoot)

	// Emit progress: downloading/extracting phase.
	if progress != nil {
		progress(0, 0, "downloading")
	}

	// Download, verify checksum, and extract.
	if err := d.downloadVerifyAndExtract(ctx, opts, progress); err != nil {
		doRollback("image download/extract failed")
		return fmt.Errorf("deploy: extract: %w", err)
	}

	// Deployment succeeded — remove the rollback backup.
	if rollbackPath != "" {
		os.Remove(rollbackPath)
		log.Printf("deploy: deployment succeeded — partition table backup removed")
	}

	return nil
}

// Finalize re-mounts the deployed partitions, applies node-specific identity,
// then unmounts cleanly. Re-mounting is necessary because Deploy defers
// unmountAll, so by the time Finalize is called the partitions are already
// unmounted. This mirrors BlockDeployer.Finalize.
func (d *FilesystemDeployer) Finalize(ctx context.Context, cfg api.NodeConfig, mountRoot string) error {
	if d.targetDisk == "" {
		return fmt.Errorf("deploy: Preflight must be called before Finalize")
	}

	// Re-create the partition device list from the stored layout.
	partDevs := make([]string, len(d.layout.Partitions))
	for i := range d.layout.Partitions {
		partDevs[i] = partitionDevice(d.targetDisk, i+1)
	}

	// Re-mount all partitions so applyNodeConfig can write into the filesystem.
	if err := d.mountPartitions(ctx, partDevs, mountRoot); err != nil {
		return fmt.Errorf("deploy: finalize: re-mount partitions: %w", err)
	}
	defer d.unmountAll(mountRoot)

	if err := applyNodeConfig(ctx, cfg, mountRoot); err != nil {
		return err
	}

	// If the layout includes RAID arrays, write mdadm.conf and update initramfs
	// so the deployed system can reassemble its arrays on next boot.
	if len(d.layout.RAIDArrays) > 0 {
		if err := GenerateMdadmConf(ctx, mountRoot); err != nil {
			// Non-fatal: log and continue. The node may still boot if the kernel
			// auto-assembles the RAID arrays via superblock scanning.
			log.Printf("deploy: generate mdadm.conf (non-fatal): %v", err)
		}
	}

	return nil
}

// partitionDisk wipes and repartitions the disk according to the layout.
// Uses sgdisk for GPT layouts (standard for EFI systems).
func (d *FilesystemDeployer) partitionDisk(ctx context.Context, disk string) error {
	// Wipe existing partition table.
	if err := runCmd(ctx, "sgdisk", "--zap-all", disk); err != nil {
		// Fall back to wipefs if sgdisk is unavailable.
		if err2 := runCmd(ctx, "wipefs", "-a", disk); err2 != nil {
			return fmt.Errorf("wipe disk %s: sgdisk failed (%v) and wipefs also failed (%v) — "+
				"check if the disk has an active RAID superblock (wipefs -a %s)", disk, err, err2, disk)
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

	cmd := exec.CommandContext(ctx, "sgdisk", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return fmt.Errorf("failed to create partitions on %s: sgdisk exited with code %d — "+
			"check if the disk has an existing RAID superblock (wipefs -a %s)\noutput: %s",
			disk, exitCode, disk, string(out))
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

// maxDownloadAttempts is the number of download attempts before giving up.
const maxDownloadAttempts = 3

// downloadVerifyAndExtract downloads the image blob from opts.ImageURL, verifies
// its sha256 checksum if opts.ExpectedChecksum is set, then extracts it into
// opts.MountRoot. Retries up to maxDownloadAttempts with exponential backoff.
func (d *FilesystemDeployer) downloadVerifyAndExtract(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	var lastErr error
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Printf("deploy: network error downloading image blob — retrying in %s (attempt %d/%d)",
				backoff, attempt, maxDownloadAttempts)
			if progress != nil {
				progress(0, 0, fmt.Sprintf("retrying (attempt %d/%d)", attempt, maxDownloadAttempts))
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		err := d.attemptDownloadAndExtract(ctx, opts, progress)
		if err == nil {
			return nil
		}
		lastErr = err

		// Only retry on network-level errors (context cancelled means timeout — don't retry).
		if ctx.Err() != nil {
			return ctx.Err()
		}
		log.Printf("deploy: download attempt %d/%d failed: %v", attempt, maxDownloadAttempts, err)
	}
	return fmt.Errorf("image download failed after %d attempts: %w", maxDownloadAttempts, lastErr)
}

// attemptDownloadAndExtract performs a single download attempt. For filesystem
// deployers it downloads the tarball into a temp file so we can verify its
// checksum before extracting.
func (d *FilesystemDeployer) attemptDownloadAndExtract(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	if opts.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error downloading image blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("network error downloading image blob: HTTP %d from %s", resp.StatusCode, opts.ImageURL)
	}

	totalBytes := resp.ContentLength

	// If checksum verification is needed, write to a temp file first so we can
	// hash the full stream before extracting. This avoids extracting a corrupt blob.
	needsVerify := !opts.SkipVerify && opts.ExpectedChecksum != ""

	if needsVerify {
		return d.downloadToTempAndExtract(ctx, resp.Body, totalBytes, opts, progress)
	}

	// Skip-verify path: pipe directly into tar for extraction (avoids temp disk space).
	if opts.SkipVerify && opts.ExpectedChecksum != "" {
		log.Printf("deploy: WARNING: checksum verification skipped for image download (--skip-verify set)")
	}
	return d.streamExtract(ctx, resp.Body, totalBytes, opts.MountRoot, progress)
}

// downloadToTempAndExtract downloads the blob to a temp file, verifies its
// checksum, then extracts it. The temp file is removed after extraction.
func (d *FilesystemDeployer) downloadToTempAndExtract(ctx context.Context, body io.Reader, totalBytes int64, opts DeployOpts, progress ProgressFunc) error {
	tmpFile, err := os.CreateTemp("", "clonr-blob-*.tar.gz")
	if err != nil {
		return fmt.Errorf("create temp file for checksum verification: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Download and hash simultaneously.
	hasher := sha256.New()
	tee := io.TeeReader(body, hasher)
	pr := &progressReader{r: tee, total: totalBytes, fn: progress, phase: "downloading"}

	if _, err := io.Copy(tmpFile, pr); err != nil {
		return fmt.Errorf("network error downloading image blob: %w", err)
	}

	// Verify checksum.
	gotChecksum := hex.EncodeToString(hasher.Sum(nil))
	if gotChecksum != opts.ExpectedChecksum {
		return fmt.Errorf("image integrity check failed: downloaded blob sha256=%s does not match "+
			"expected=%s — the image may be corrupt or the server checksum is stale; "+
			"use --skip-verify to deploy anyway", gotChecksum, opts.ExpectedChecksum)
	}
	log.Printf("deploy: image checksum verified: sha256=%s", gotChecksum)

	// Seek back to start for extraction.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file for extraction: %w", err)
	}

	if progress != nil {
		progress(0, totalBytes, "extracting")
	}

	tarCmd := exec.CommandContext(ctx, "tar", "-xaf", "-", "-C", opts.MountRoot)
	tarCmd.Stdin = &progressReader{r: tmpFile, total: totalBytes, fn: progress, phase: "extracting"}
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w\noutput: %s", err, string(out))
	}

	return nil
}

// streamExtract pipes the download body directly into tar without checksum verification.
func (d *FilesystemDeployer) streamExtract(ctx context.Context, body io.Reader, totalBytes int64, mountRoot string, progress ProgressFunc) error {
	tarCmd := exec.CommandContext(ctx, "tar", "-xzf", "-", "-C", mountRoot)
	tarCmd.Stdin = &progressReader{r: body, total: totalBytes, fn: progress, phase: "extracting"}
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
