package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

	log.Printf("deploy: target disk: %s", disk)

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
	log.Printf("deploy: partitioning disk %s (%d partitions)", disk, len(d.layout.Partitions))
	if progress != nil {
		progress(0, 0, "partitioning")
	}

	// Create RAID arrays before partitioning, if any are specified.
	if len(d.layout.RAIDArrays) > 0 {
		log.Printf("deploy: creating %d RAID arrays", len(d.layout.RAIDArrays))
		if err := CreateRAIDArrays(ctx, d.layout, hardware.SystemInfo{}); err != nil {
			doRollback("RAID array creation failed")
			return fmt.Errorf("deploy: create raid arrays: %w", err)
		}
	}

	// Partition the disk.
	if err := d.partitionDisk(ctx, disk); err != nil {
		log.Printf("deploy: ERROR: partitioning failed on %s: %v", disk, err)
		doRollback("partitioning failed")
		return err // partitionDisk already produces an actionable error
	}
	log.Printf("deploy: partitioning complete on %s", disk)

	for i, p := range d.layout.Partitions {
		sizeStr := "fill"
		if p.SizeBytes > 0 {
			sizeStr = fmt.Sprintf("%s", humanReadableBytes(p.SizeBytes))
		}
		log.Printf("deploy: partition %d: %s fs=%s size=%s", i+1, p.MountPoint, p.Filesystem, sizeStr)
	}

	// Emit progress: formatting phase.
	log.Printf("deploy: formatting partitions on %s", disk)
	if progress != nil {
		progress(0, 0, "formatting")
	}

	// Create filesystems.
	partDevs, err := d.createFilesystems(ctx, disk)
	if err != nil {
		log.Printf("deploy: ERROR: filesystem creation failed on %s: %v", disk, err)
		doRollback("filesystem creation failed")
		return fmt.Errorf("deploy: create filesystems: %w", err)
	}
	log.Printf("deploy: filesystems created: %s", strings.Join(partDevs, ", "))

	// Mount partitions.
	log.Printf("deploy: mounting partitions at %s", opts.MountRoot)
	if err := d.mountPartitions(ctx, partDevs, opts.MountRoot); err != nil {
		log.Printf("deploy: ERROR: mount failed: %v", err)
		doRollback("partition mount failed")
		return fmt.Errorf("deploy: mount partitions: %w", err)
	}
	log.Printf("deploy: partitions mounted")
	// Always attempt unmount on exit.
	defer d.unmountAll(opts.MountRoot)

	// Emit progress: downloading/extracting phase.
	log.Printf("deploy: downloading image blob from server")
	if progress != nil {
		progress(0, 0, "downloading")
	}

	// Download, verify checksum, and stream-extract.
	if err := d.downloadVerifyAndExtract(ctx, opts, progress); err != nil {
		log.Printf("deploy: ERROR: image download/extract failed: %v", err)
		doRollback("image download/extract failed")
		return fmt.Errorf("deploy: extract: %w", err)
	}
	log.Printf("deploy: extraction complete")

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
	log.Printf("deploy: wiping existing partition table on %s", disk)
	// Wipe existing partition table.
	if err := runCmd(ctx, "sgdisk", "--zap-all", disk); err != nil {
		log.Printf("deploy: sgdisk --zap-all failed (%v), trying wipefs", err)
		// Fall back to wipefs if sgdisk is unavailable.
		if err2 := runCmd(ctx, "wipefs", "-a", disk); err2 != nil {
			return fmt.Errorf("wipe disk %s: sgdisk failed (%v) and wipefs also failed (%v) — "+
				"check if the disk has an active RAID superblock (wipefs -a %s)", disk, err, err2, disk)
		}
	}
	log.Printf("deploy: disk %s wiped", disk)

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
		// Set partition type GUID based on flags.
		for _, flag := range p.Flags {
			switch flag {
			case "esp", "boot":
				// EFI System Partition: type code ef00
				args = append(args, fmt.Sprintf("--typecode=%d:ef00", num))
			case "bios_grub":
				// BIOS boot partition: type code ef02 (used by GRUB2 on GPT/BIOS systems)
				args = append(args, fmt.Sprintf("--typecode=%d:ef02", num))
			}
		}
	}
	args = append(args, disk)

	log.Printf("deploy: running sgdisk to create %d partitions on %s", len(d.layout.Partitions), disk)
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
	log.Printf("deploy: sgdisk partition creation succeeded on %s", disk)

	// Allow kernel to re-read the new partition table.
	log.Printf("deploy: running partprobe on %s", disk)
	_ = runCmd(ctx, "partprobe", disk)
	_ = runCmd(ctx, "udevadm", "settle")
	// In minimal initramfs environments without udevd, partition uevent
	// notifications are not processed, so /dev/sdaN nodes don't appear
	// after partprobe. Run 'mdev -s' (busybox) to scan sysfs and create
	// device nodes, then fall back to manually creating them if needed.
	log.Printf("deploy: triggering device node creation for new partitions")
	_ = exec.CommandContext(ctx, "mdev", "-s").Run()
	// Also trigger re-read via blockdev for kernels that support it.
	_ = exec.CommandContext(ctx, "blockdev", "--rereadpt", disk).Run()
	// Give the kernel time to create partition device nodes in /dev.
	if err := waitForPartitions(ctx, disk, len(d.layout.Partitions), 15); err != nil {
		log.Printf("deploy: waitForPartitions timed out — attempting manual device node creation")
		ensurePartitionNodes(disk, len(d.layout.Partitions))
	}
	log.Printf("deploy: partition table re-read complete")

	return nil
}

// ensurePartitionNodes attempts multiple strategies to create partition device
// nodes in /dev after sgdisk. In a minimal initramfs without udevd, the kernel
// fires uevents for new partitions but nothing processes them unless mdev is
// registered as the hotplug handler. We try three strategies in order:
//
//  1. partx --add  — BLKPG ioctl: tells the kernel to re-read the partition
//     table and update /dev. Most reliable when util-linux is present.
//  2. mdev -s      — rescans sysfs and creates nodes for all discovered devices.
//  3. mknod        — last resort: directly create block device nodes using
//     major/minor numbers read from /sys/class/block/<name>/dev.
func ensurePartitionNodes(disk string, count int) {
	log.Printf("deploy: running partx to force partition node creation on %s", disk)
	cmd := exec.Command("partx", "--add", disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("deploy: partx --add %s: %v\noutput: %s", disk, err, string(out))
	} else {
		log.Printf("deploy: partx --add succeeded on %s", disk)
	}

	// Re-run mdev -s to pick up any new sysfs entries for the partitions.
	if out, err := exec.Command("mdev", "-s").CombinedOutput(); err != nil {
		log.Printf("deploy: mdev -s after partx: %v (output: %s)", err, string(out))
	} else {
		log.Printf("deploy: mdev -s ran after partx")
	}

	// Last resort: create nodes directly from sysfs major:minor data.
	diskBase := filepath.Base(disk)
	for num := 1; num <= count; num++ {
		devPath := partitionDevice(disk, num)
		if _, err := os.Stat(devPath); err == nil {
			continue // already exists
		}
		// sysfs exposes the partition's major:minor at
		// /sys/class/block/<disk><num>/dev (e.g. /sys/class/block/sda1/dev)
		partName := fmt.Sprintf("%s%d", diskBase, num)
		sysDevPath := fmt.Sprintf("/sys/class/block/%s/dev", partName)
		devData, readErr := os.ReadFile(sysDevPath)
		if readErr != nil {
			log.Printf("deploy: mknod fallback: cannot read %s: %v", sysDevPath, readErr)
			continue
		}
		// devData is "MAJOR:MINOR\n"
		var major, minor uint32
		if _, err := fmt.Sscanf(strings.TrimSpace(string(devData)), "%d:%d", &major, &minor); err != nil {
			log.Printf("deploy: mknod fallback: cannot parse major:minor from %q: %v", string(devData), err)
			continue
		}
		// syscall.Mknod creates a block device node (S_IFBLK = 0x6000).
		devNum := major*256 + minor
		if err := syscall.Mknod(devPath, syscall.S_IFBLK|0o600, int(devNum)); err != nil {
			log.Printf("deploy: mknod %s (%d:%d): %v", devPath, major, minor, err)
		} else {
			log.Printf("deploy: mknod created %s (%d:%d)", devPath, major, minor)
		}
	}

	// Log final /dev state for diagnostics.
	if out, err := exec.Command("ls", "-la", filepath.Dir(disk)).CombinedOutput(); err == nil {
		log.Printf("deploy: /dev after ensurePartitionNodes:\n%s", string(out))
	}
}

// waitForPartitions waits until all expected partition device nodes appear in /dev.
// This is necessary in initramfs environments where devtmpfs creates nodes
// asynchronously after partprobe signals the kernel.
func waitForPartitions(ctx context.Context, disk string, count int, maxWaitSec int) error {
	start := time.Now()
	for i := 0; i < maxWaitSec; i++ {
		allPresent := true
		for num := 1; num <= count; num++ {
			dev := partitionDevice(disk, num)
			if _, err := os.Stat(dev); os.IsNotExist(err) {
				allPresent = false
				log.Printf("deploy: waiting for %s to appear... (%ds elapsed)", dev, i)
				break
			}
		}
		if allPresent {
			log.Printf("deploy: all %d partition device nodes present after %s", count, time.Since(start).Round(time.Millisecond))
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("partition devices not ready after %ds", maxWaitSec)
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
		case "", "biosboot", "bios_grub":
			// No filesystem — raw partition for BIOS boot (GPT BIOS boot partition)
			// or an explicitly unformatted partition. sgdisk already set the type GUID.
			log.Printf("deploy: partition %d: skipping mkfs for %q (BIOS boot / raw)", i+1, p.Filesystem)
			continue
		default:
			return nil, fmt.Errorf("unsupported filesystem %q for partition %d", p.Filesystem, i+1)
		}

		log.Printf("deploy: formatting %s as %s", dev, p.Filesystem)
		if err := runCmd(ctx, mkfsBin, mkfsArgs...); err != nil {
			log.Printf("deploy: ERROR: mkfs failed on %s (%s): %v", dev, p.Filesystem, err)
			return nil, fmt.Errorf("mkfs partition %d (%s): %w", i+1, p.Filesystem, err)
		}
		log.Printf("deploy: filesystem %s created on %s", p.Filesystem, dev)
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

// attemptDownloadAndExtract performs a single download attempt.
// Always streams the blob directly into tar — no temp file — so a 1.6GB image
// can be deployed from an initramfs with 1GB RAM. When ExpectedChecksum is set,
// we tee the stream through a sha256 hasher concurrently so we still verify
// integrity without buffering the whole blob to disk first.
func (d *FilesystemDeployer) attemptDownloadAndExtract(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	if opts.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	}

	log.Printf("deploy: downloading image blob from %s", opts.ImageURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error downloading image blob: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("network error downloading image blob: HTTP %d from %s", resp.StatusCode, opts.ImageURL)
	}

	totalBytes := resp.ContentLength
	if totalBytes > 0 {
		log.Printf("deploy: image size: %s", humanReadableBytes(totalBytes))
	}

	needsVerify := !opts.SkipVerify && opts.ExpectedChecksum != ""
	if opts.SkipVerify && opts.ExpectedChecksum != "" {
		log.Printf("deploy: WARNING: checksum verification skipped for image download (--skip-verify set)")
	}

	return d.streamExtract(ctx, resp.Body, totalBytes, opts, needsVerify, progress)
}

// streamExtract streams the HTTP body directly into tar, computing the sha256
// concurrently via a pipe so we never buffer the full blob to disk. This uses
// constant memory regardless of image size.
//
// When needsVerify is true, the hasher result is checked after tar exits — if
// checksum mismatches, an error is returned (but extraction has already run, so
// the data on disk may be corrupt; the caller's retry loop will re-run from scratch).
func (d *FilesystemDeployer) streamExtract(ctx context.Context, body io.Reader, totalBytes int64, opts DeployOpts, needsVerify bool, progress ProgressFunc) error {
	log.Printf("deploy: starting stream-extract into %s", opts.MountRoot)

	// Set up the reader chain: body → [hasher tee] → progress → tar stdin.
	var hasher hash.Hash
	var reader io.Reader = body

	if needsVerify {
		hasher = sha256.New()
		reader = io.TeeReader(body, hasher)
	}

	pr := &progressReader{r: reader, total: totalBytes, fn: progress, phase: "downloading+extracting"}

	// Use 'tar -xf -' which relies on GNU tar 1.29+ transparent decompression: when
	// reading from stdin, GNU tar detects compression format from the stream's magic
	// bytes (gzip=1f8b, xz=fd37, zstd=28b5) rather than the filename extension.
	// Do NOT use '-a' (--auto-compress) — that flag uses the archive filename suffix
	// to select the compressor, which always fails when reading from stdin ('-')
	// because there is no filename to inspect.
	tarCmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", opts.MountRoot)
	tarCmd.Stdin = pr

	tarOut, err := tarCmd.CombinedOutput()
	if err != nil {
		exitCode := -1
		if tarCmd.ProcessState != nil {
			exitCode = tarCmd.ProcessState.ExitCode()
		}
		return fmt.Errorf("tar extract failed (exit %d): %w\noutput: %s", exitCode, err, string(tarOut))
	}

	log.Printf("deploy: stream-extract complete, %s read", humanReadableBytes(pr.written))

	// Post-extraction checksum verification. If this fails the data is on disk
	// but potentially corrupt — the caller's retry loop will restart the download.
	if needsVerify && hasher != nil {
		gotChecksum := hex.EncodeToString(hasher.Sum(nil))
		if gotChecksum != opts.ExpectedChecksum {
			return fmt.Errorf("image integrity check failed: downloaded blob sha256=%s does not match "+
				"expected=%s — the image may be corrupt or the server checksum is stale; "+
				"use --skip-verify to deploy anyway", gotChecksum, opts.ExpectedChecksum)
		}
		log.Printf("deploy: image checksum verified: sha256=%s", gotChecksum)
	}

	if progress != nil {
		progress(pr.written, totalBytes, "extract complete")
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
