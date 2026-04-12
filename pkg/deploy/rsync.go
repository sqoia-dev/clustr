package deploy

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
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
// Blob download is started concurrently with partition/format/mount to overlap I/O:
//  1. Start HTTP connection + open blob download in a goroutine (establishes TCP + TLS).
//  2. In parallel: partition, format filesystems, mount (CPU/disk ops, typically 2-5s).
//  3. Once mounts are ready, begin extracting from the already-open download pipe.
//
// This hides nearly all of the network round-trip latency behind local disk ops.
func (d *FilesystemDeployer) Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	if d.targetDisk == "" {
		return fmt.Errorf("deploy: Preflight must be called before Deploy")
	}

	disk := opts.TargetDisk
	if disk == "" {
		disk = d.targetDisk
	}

	logger().Info().Str("disk", disk).Msg("deploy: target disk selected")

	// ── Start blob download in background — overlaps with partition/format/mount ─
	// We open the HTTP connection now so the TCP handshake + TLS + server seek
	// happen while we are busy with local disk operations. The body is buffered
	// via an os.Pipe (64KB kernel buffer) so the server can push data ahead of us.
	type blobResult struct {
		resp       *http.Response
		totalBytes int64
		err        error
	}
	blobCh := make(chan blobResult, 1)

	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
		if err != nil {
			blobCh <- blobResult{err: fmt.Errorf("build download request: %w", err)}
			return
		}
		if opts.AuthToken != "" {
			req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
		}
		logger().Info().Str("url", opts.ImageURL).Msg("prefetching image blob (concurrent with partitioning)")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			blobCh <- blobResult{err: fmt.Errorf("network error fetching blob: %w", err)}
			return
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			resp.Body.Close()
			blobCh <- blobResult{err: fmt.Errorf("HTTP %d fetching blob from %s", resp.StatusCode, opts.ImageURL)}
			return
		}
		blobCh <- blobResult{resp: resp, totalBytes: resp.ContentLength}
	}()

	// ── Rollback setup ────────────────────────────────────────────────────────
	var rollbackPath string
	if !opts.NoRollback {
		backup, empty, err := backupPartitionTable(disk)
		if err != nil {
			// Non-fatal: log and continue. We still proceed, just without rollback.
			logger().Warn().Str("disk", disk).Err(err).Msg("could not back up partition table — proceeding without rollback")
		} else if empty {
			logger().Info().Str("disk", disk).Msg("disk has no existing partition table — no rollback possible if deployment fails")
		} else {
			rollbackPath = backup
			logger().Info().Str("backup", rollbackPath).Msg("partition table backup saved (will restore on failure)")
		}
	}

	// doRollback restores the partition table if a backup was taken.
	doRollback := func(reason string) {
		if rollbackPath == "" {
			return
		}
		logger().Warn().Str("reason", reason).Str("disk", disk).Msg("ROLLBACK triggered — restoring partition table")
		if err := restorePartitionTable(disk, rollbackPath); err != nil {
			logger().Error().Err(err).Str("disk", disk).Msg("ROLLBACK FAILED — disk may be in inconsistent state; re-run deployment to recover")
		} else {
			logger().Info().Str("disk", disk).Msg("rollback complete — partition table restored to pre-deployment state")
			rollbackPath = "" // already removed by restorePartitionTable
		}
	}

	// Emit progress: partitioning phase.
	logger().Info().Str("disk", disk).Int("partitions", len(d.layout.Partitions)).Msg("partitioning disk")
	if progress != nil {
		progress(0, 0, "partitioning")
	}
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("partitioning", int64(len(d.layout.Partitions)))
	}

	// Create RAID arrays before partitioning, if any are specified.
	if len(d.layout.RAIDArrays) > 0 {
		logger().Info().Int("count", len(d.layout.RAIDArrays)).Msg("creating RAID arrays")
		if err := CreateRAIDArrays(ctx, d.layout, hardware.SystemInfo{}); err != nil {
			doRollback("RAID array creation failed")
			if opts.Reporter != nil {
				opts.Reporter.EndPhase(err.Error())
			}
			return fmt.Errorf("deploy: create raid arrays: %w", err)
		}
	}

	// Partition the disk.
	if err := d.partitionDisk(ctx, disk); err != nil {
		logger().Error().Str("disk", disk).Err(err).Msg("partitioning failed")
		doRollback("partitioning failed")
		if opts.Reporter != nil {
			opts.Reporter.EndPhase(err.Error())
		}
		return err // partitionDisk already produces an actionable error
	}
	logger().Info().Str("disk", disk).Msg("partitioning complete")
	if opts.Reporter != nil {
		opts.Reporter.EndPhase("")
	}

	// Emit progress: formatting phase.
	logger().Info().Str("disk", disk).Msg("formatting partitions")
	if progress != nil {
		progress(0, 0, "formatting")
	}
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("formatting", int64(len(d.layout.Partitions)))
	}

	// Create filesystems.
	partDevs, err := d.createFilesystems(ctx, disk)
	if err != nil {
		logger().Error().Str("disk", disk).Err(err).Msg("filesystem creation failed")
		doRollback("filesystem creation failed")
		if opts.Reporter != nil {
			opts.Reporter.EndPhase(err.Error())
		}
		return fmt.Errorf("deploy: create filesystems: %w", err)
	}
	logger().Info().Str("devices", strings.Join(partDevs, ", ")).Msg("filesystems created")
	if opts.Reporter != nil {
		opts.Reporter.EndPhase("")
	}

	// Mount partitions.
	logger().Info().Str("mount_root", opts.MountRoot).Msg("mounting partitions")
	if err := d.mountPartitions(ctx, partDevs, opts.MountRoot); err != nil {
		logger().Error().Err(err).Msg("partition mount failed")
		doRollback("partition mount failed")
		return fmt.Errorf("deploy: mount partitions: %w", err)
	}
	logger().Info().Str("mount_root", opts.MountRoot).Msg("partitions mounted")
	// Always attempt unmount on exit.
	defer d.unmountAll(opts.MountRoot)

	// ── Wait for the pre-fetched blob connection, then extract ────────────────
	if progress != nil {
		progress(0, 0, "downloading")
	}

	blob := <-blobCh
	if blob.err != nil {
		logger().Error().Err(blob.err).Msg("blob prefetch failed")
		doRollback("blob prefetch failed")
		return fmt.Errorf("deploy: blob prefetch: %w", blob.err)
	}
	defer blob.resp.Body.Close()
	if blob.totalBytes > 0 {
		logger().Info().Str("size", humanReadableBytes(blob.totalBytes)).Msg("image blob connection ready — extracting")
	} else {
		logger().Info().Msg("image blob connection ready — extracting (unknown size)")
	}

	// Signal downloading phase with byte total for real-time UI progress.
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("downloading", blob.totalBytes)
	}

	needsVerify := !opts.SkipVerify && opts.ExpectedChecksum != ""
	if opts.SkipVerify && opts.ExpectedChecksum != "" {
		logger().Warn().Msg("checksum verification skipped for image download (--skip-verify set)")
	}

	if err := d.streamExtract(ctx, blob.resp.Body, blob.totalBytes, opts, needsVerify, progress); err != nil {
		logger().Error().Err(err).Msg("image download/extract failed")
		doRollback("image download/extract failed")
		if opts.Reporter != nil {
			opts.Reporter.EndPhase(err.Error())
		}
		return fmt.Errorf("deploy: extract: %w", err)
	}
	logger().Info().Msg("extraction complete")
	if opts.Reporter != nil {
		opts.Reporter.EndPhase("")
	}

	// Deployment succeeded — remove the rollback backup.
	if rollbackPath != "" {
		os.Remove(rollbackPath)
		logger().Info().Msg("deployment succeeded — partition table backup removed")
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

	// Install the GRUB bootloader if the layout includes a bios_grub partition.
	// This writes stage1/stage1.5 to the bios_grub partition and the MBR so the
	// deployed system can boot on BIOS/GPT systems without EFI.
	hasBIOSGrub := false
	for _, p := range d.layout.Partitions {
		for _, flag := range p.Flags {
			if flag == "bios_grub" || flag == "biosboot" {
				hasBIOSGrub = true
				break
			}
		}
	}
	if hasBIOSGrub {
		log := logger()
		bootDir := filepath.Join(mountRoot, "boot")
		log.Info().Str("disk", d.targetDisk).Str("bootDir", bootDir).
			Msg("finalize: installing GRUB bootloader (BIOS/GPT)")
		grubArgs := []string{
			"--target=i386-pc",
			"--boot-directory=" + bootDir,
			"--recheck",
			d.targetDisk,
		}
		if err := runAndLog(ctx, "grub2-install", exec.CommandContext(ctx, "grub2-install", grubArgs...)); err != nil {
			// Non-fatal: log prominently. The node may still boot if the image
			// already had a working bootloader installed (e.g. a snapshot of a
			// running system). Manual intervention: chroot in and run grub2-install.
			log.Warn().Err(err).Str("disk", d.targetDisk).
				Msg("WARNING: finalize: grub2-install failed (non-fatal) — node may not boot; run grub2-install manually")
		} else {
			log.Info().Str("disk", d.targetDisk).Msg("finalize: GRUB bootloader installed")
		}
	}

	// If the layout includes RAID arrays, write mdadm.conf and update initramfs
	// so the deployed system can reassemble its arrays on next boot.
	if len(d.layout.RAIDArrays) > 0 {
		if err := GenerateMdadmConf(ctx, mountRoot); err != nil {
			// Non-fatal: log and continue. The node may still boot if the kernel
			// auto-assembles the RAID arrays via superblock scanning.
			logger().Warn().Err(err).Msg("generate mdadm.conf failed (non-fatal)")
		}
	}

	return nil
}

// partitionDisk wipes and repartitions the disk according to the layout.
// Uses sgdisk for GPT layouts (standard for EFI systems).
func (d *FilesystemDeployer) partitionDisk(ctx context.Context, disk string) error {
	log := logger()
	log.Info().Str("disk", disk).Msg("wiping existing partition table")
	// Wipe existing partition table.
	if err := runCmd(ctx, "sgdisk", "--zap-all", disk); err != nil {
		log.Warn().Str("disk", disk).Err(err).Msg("sgdisk --zap-all failed, trying wipefs")
		// Fall back to wipefs if sgdisk is unavailable.
		if err2 := runCmd(ctx, "wipefs", "-a", disk); err2 != nil {
			return fmt.Errorf("wipe disk %s: sgdisk failed (%v) and wipefs also failed (%v) — "+
				"check if the disk has an active RAID superblock (wipefs -a %s)", disk, err, err2, disk)
		}
	}
	log.Info().Str("disk", disk).Msg("disk wiped")

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

		// Log each partition as it's being defined (granular progress, Problem 3).
		sizeStr := "fill"
		if p.SizeBytes > 0 {
			sizeStr = humanReadableBytes(p.SizeBytes)
		}
		log.Info().Int("partition", num).Str("mountpoint", p.MountPoint).
			Str("filesystem", p.Filesystem).Str("size", sizeStr).
			Str("flags", strings.Join(p.Flags, ",")).
			Msg("defining partition")
	}
	args = append(args, disk)

	log.Info().Int("count", len(d.layout.Partitions)).Str("disk", disk).Msg("running sgdisk to create partitions")
	if err := runAndLog(ctx, "sgdisk", exec.CommandContext(ctx, "sgdisk", args...)); err != nil {
		return fmt.Errorf("failed to create partitions on %s — "+
			"check if the disk has an existing RAID superblock (wipefs -a %s): %w",
			disk, disk, err)
	}
	log.Info().Str("disk", disk).Msg("sgdisk partition creation succeeded")

	// Allow kernel to re-read the new partition table.
	log.Info().Str("disk", disk).Msg("running partprobe to re-read partition table")
	_ = runCmd(ctx, "partprobe", disk)
	_ = runCmd(ctx, "udevadm", "settle")
	// In minimal initramfs environments without udevd, partition uevent
	// notifications are not processed, so /dev/sdaN nodes don't appear
	// after partprobe. Run 'mdev -s' (busybox) to scan sysfs and create
	// device nodes, then fall back to manually creating them if needed.
	log.Info().Msg("triggering device node creation for new partitions")
	_ = exec.CommandContext(ctx, "mdev", "-s").Run()
	// Also trigger re-read via blockdev for kernels that support it.
	_ = exec.CommandContext(ctx, "blockdev", "--rereadpt", disk).Run()
	// Give the kernel time to create partition device nodes in /dev.
	if err := waitForPartitions(ctx, disk, len(d.layout.Partitions), 15); err != nil {
		log.Warn().Err(err).Msg("waitForPartitions timed out — attempting manual device node creation")
		ensurePartitionNodes(disk, len(d.layout.Partitions))
	}
	log.Info().Str("disk", disk).Msg("partition table re-read complete")

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
	log := logger()
	log.Info().Str("disk", disk).Msg("running partx to force partition node creation")
	cmd := exec.Command("partx", "--add", disk)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Warn().Str("disk", disk).Err(err).Str("output", string(out)).Msg("partx --add failed")
	} else {
		log.Info().Str("disk", disk).Msg("partx --add succeeded")
	}

	// Re-run mdev -s to pick up any new sysfs entries for the partitions.
	if out, err := exec.Command("mdev", "-s").CombinedOutput(); err != nil {
		log.Warn().Err(err).Str("output", string(out)).Msg("mdev -s after partx failed")
	} else {
		log.Info().Msg("mdev -s ran after partx")
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
			log.Warn().Str("sysfs_path", sysDevPath).Err(readErr).Msg("mknod fallback: cannot read major:minor")
			continue
		}
		// devData is "MAJOR:MINOR\n"
		var major, minor uint32
		if _, err := fmt.Sscanf(strings.TrimSpace(string(devData)), "%d:%d", &major, &minor); err != nil {
			log.Warn().Str("data", string(devData)).Err(err).Msg("mknod fallback: cannot parse major:minor")
			continue
		}
		// syscall.Mknod creates a block device node (S_IFBLK = 0x6000).
		devNum := major*256 + minor
		if err := syscall.Mknod(devPath, syscall.S_IFBLK|0o600, int(devNum)); err != nil {
			log.Warn().Str("dev", devPath).Uint32("major", major).Uint32("minor", minor).Err(err).Msg("mknod failed")
		} else {
			log.Info().Str("dev", devPath).Uint32("major", major).Uint32("minor", minor).Msg("mknod created device node")
		}
	}

	// Log final /dev state for diagnostics.
	if out, err := exec.Command("ls", "-la", filepath.Dir(disk)).CombinedOutput(); err == nil {
		log.Info().Str("output", string(out)).Msg("/dev state after ensurePartitionNodes")
	}
}

// waitForPartitions waits until all expected partition device nodes appear in /dev.
// This is necessary in initramfs environments where devtmpfs creates nodes
// asynchronously after partprobe signals the kernel.
func waitForPartitions(ctx context.Context, disk string, count int, maxWaitSec int) error {
	log := logger()
	start := time.Now()
	for i := 0; i < maxWaitSec; i++ {
		allPresent := true
		for num := 1; num <= count; num++ {
			dev := partitionDevice(disk, num)
			if _, err := os.Stat(dev); os.IsNotExist(err) {
				allPresent = false
				log.Info().Str("dev", dev).Int("elapsed_sec", i).Msg("waiting for partition device node to appear")
				break
			}
		}
		if allPresent {
			log.Info().Int("count", count).Str("elapsed", time.Since(start).Round(time.Millisecond).String()).
				Msg("all partition device nodes present")
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
	log := logger()
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
			log.Info().Int("partition", i+1).Str("filesystem", p.Filesystem).
				Msg("skipping mkfs for BIOS boot / raw partition")
			continue
		default:
			return nil, fmt.Errorf("unsupported filesystem %q for partition %d", p.Filesystem, i+1)
		}

		// Log size if known (granular progress, Problem 3).
		sizeStr := "fill"
		if p.SizeBytes > 0 {
			sizeStr = humanReadableBytes(p.SizeBytes)
		}
		log.Info().Int("partition", i+1).Str("device", dev).Str("filesystem", p.Filesystem).
			Str("size", sizeStr).Msg("formatting partition")

		if err := runAndLog(ctx, mkfsBin, exec.CommandContext(ctx, mkfsBin, mkfsArgs...)); err != nil {
			log.Error().Str("device", dev).Str("filesystem", p.Filesystem).Err(err).Msg("mkfs failed")
			return nil, fmt.Errorf("mkfs partition %d (%s): %w", i+1, p.Filesystem, err)
		}
		log.Info().Int("partition", i+1).Str("device", dev).Str("filesystem", p.Filesystem).
			Str("size", sizeStr).Msg("filesystem created")
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
		// If the device is already mounted at this target (e.g. Deploy's lazy
		// umount left it attached), skip rather than failing with EBUSY.
		if isMounted(m.dev, target) {
			logger().Info().Str("dev", m.dev).Str("target", target).
				Msg("mountPartitions: already mounted — skipping")
			continue
		}
		// Retry mount up to 5 times with 1s backoff. After a lazy umount the kernel
		// may still mark the device as in-use for a brief window; retrying avoids a
		// spurious EBUSY failure.
		var mountErr error
		for attempt := 1; attempt <= 5; attempt++ {
			mountErr = runCmd(ctx, "mount", m.dev, target)
			if mountErr == nil {
				break
			}
			if attempt < 5 {
				logger().Debug().Str("dev", m.dev).Str("target", target).Err(mountErr).
					Int("attempt", attempt).Msg("mount: retrying in 1s (device may still be releasing)")
				time.Sleep(time.Second)
			}
		}
		if mountErr != nil {
			return fmt.Errorf("mount %s → %s: %w", m.dev, target, mountErr)
		}
	}
	return nil
}

// isMounted reports whether dev is currently mounted at target by scanning
// /proc/mounts. Returns false if /proc/mounts is unavailable (non-Linux).
func isMounted(dev, target string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == dev && fields[1] == target {
			return true
		}
	}
	return false
}

// xfsSyncMountRoot freezes then immediately thaws every XFS filesystem mounted
// under mountRoot. freeze(xfs_freeze -f) forces the XFS log to fully commit all
// in-memory journal entries to disk and quiesces I/O; thaw(xfs_freeze -u) resumes
// normal operation. After this cycle the filesystem has no pending dirty log
// entries and is safe to unmount without EBUSY from lingering log I/O.
//
// XFS uses an asynchronous delayed-logging model: sync(2) flushes the page cache
// but does NOT wait for the XFS circular log buffer to commit. On a large tar
// extraction that writes thousands of inodes, the log can remain active for several
// seconds after sync returns, causing umount to fail with EBUSY. xfs_freeze is the
// only reliable way to drain it without a fixed-duration sleep.
//
// Mounts are processed deepest-first so nested mounts (e.g. /boot nested inside /)
// are frozen before their parent.
func xfsSyncMountRoot(mountRoot string) {
	log := logger()

	// Read /proc/mounts and find all XFS mounts under mountRoot (or exactly mountRoot).
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		log.Warn().Err(err).Msg("xfsSyncMountRoot: cannot read /proc/mounts — skipping xfs_freeze")
		return
	}

	var xfsMounts []string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		target := fields[1]
		fstype := fields[2]
		// Only XFS mounts at or under mountRoot.
		if fstype != "xfs" {
			continue
		}
		if target != mountRoot && !strings.HasPrefix(target, mountRoot+"/") {
			continue
		}
		xfsMounts = append(xfsMounts, target)
	}

	if len(xfsMounts) == 0 {
		// No XFS mounts found — fall back to a global sync.
		_ = exec.Command("sync").Run()
		return
	}

	// Sort deepest-first (longest path first) so nested mounts are frozen before
	// their parents. Sorting by path length descending achieves this.
	for i := 0; i < len(xfsMounts)-1; i++ {
		for j := i + 1; j < len(xfsMounts); j++ {
			if len(xfsMounts[j]) > len(xfsMounts[i]) {
				xfsMounts[i], xfsMounts[j] = xfsMounts[j], xfsMounts[i]
			}
		}
	}

	for _, mp := range xfsMounts {
		if out, err := exec.Command("xfs_freeze", "-f", mp).CombinedOutput(); err != nil {
			log.Warn().Str("mountpoint", mp).Err(err).Str("output", string(out)).
				Msg("xfs_freeze -f failed — XFS log may not be fully committed")
			continue
		}
		log.Info().Str("mountpoint", mp).Msg("xfs_freeze: log committed and filesystem frozen")
		// Immediately thaw — we only needed the freeze to drain the log.
		if out, err := exec.Command("xfs_freeze", "-u", mp).CombinedOutput(); err != nil {
			log.Warn().Str("mountpoint", mp).Err(err).Str("output", string(out)).
				Msg("xfs_freeze -u failed — filesystem may remain frozen; unmount will proceed")
		} else {
			log.Info().Str("mountpoint", mp).Msg("xfs_freeze: filesystem thawed")
		}
	}
}

// logMountBusyDiagnostics emits lsof and /proc/mounts output at Warn level.
// Called when umount -R fails so the next failure has actionable context in logs.
func logMountBusyDiagnostics(mountRoot string) {
	log := logger()
	if out, err := exec.Command("lsof", "+D", mountRoot).CombinedOutput(); err == nil && len(out) > 0 {
		log.Warn().Str("mountRoot", mountRoot).Str("lsof", string(out)).
			Msg("umount EBUSY diagnostic: open files under mountRoot")
	}
	if out, err := exec.Command("fuser", "-vm", mountRoot).CombinedOutput(); err == nil && len(out) > 0 {
		log.Warn().Str("mountRoot", mountRoot).Str("fuser", string(out)).
			Msg("umount EBUSY diagnostic: processes using mountRoot")
	}
	data, _ := os.ReadFile("/proc/mounts")
	log.Warn().Str("proc_mounts", string(data)).Msg("umount EBUSY diagnostic: current mounts")
}

// unmountAll unmounts everything under mountRoot.
// Before unmounting, it freezes-then-thaws every XFS filesystem under mountRoot
// via xfs_freeze to force the XFS log to fully commit — this is the reliable fix
// for EBUSY after large tar extractions on XFS. sync(2) alone is insufficient
// because it flushes the page cache but does not drain XFS's internal log buffer.
//
// After the XFS freeze/thaw, clean unmount should succeed immediately. We keep
// three retries as a safety margin for non-XFS cases. Only falls back to lazy
// detach if all retries fail. Lazy detach (umount -l) is intentionally a last
// resort because it leaves the block device marked "in use" by the kernel, which
// causes the subsequent Finalize remount to fail with EBUSY.
func (d *FilesystemDeployer) unmountAll(mountRoot string) {
	log := logger()

	// Force XFS log commit before unmounting. This drains the async journal
	// that keeps the filesystem busy after large writes.
	xfsSyncMountRoot(mountRoot)

	// After xfs_freeze the filesystem should unmount cleanly on the first try.
	// Keep three retries for safety (non-XFS filesystems, ext4 writeback, etc).
	const maxUmountRetries = 3
	for attempt := 1; attempt <= maxUmountRetries; attempt++ {
		err := exec.Command("umount", "-R", mountRoot).Run()
		if err == nil {
			if attempt > 1 {
				log.Info().Str("mountRoot", mountRoot).Int("attempt", attempt).
					Msg("umount -R succeeded after retries")
			}
			return
		}
		if attempt < maxUmountRetries {
			log.Debug().Str("mountRoot", mountRoot).Err(err).Int("attempt", attempt).
				Msg("umount -R not ready yet — retrying in 1s")
			// Re-run xfs_freeze in case a retry is needed — there may be a second
			// partition (e.g. /boot) that was not yet drained on the first pass.
			xfsSyncMountRoot(mountRoot)
			time.Sleep(time.Second)
		} else {
			log.Warn().Str("mountRoot", mountRoot).Err(err).
				Msg("umount -R failed after all retries — collecting diagnostics then falling back to lazy detach")
			logMountBusyDiagnostics(mountRoot)
			// umount -l: lazy detach. The kernel detaches the mount from the filesystem
			// namespace immediately but keeps the mount alive until all open file
			// descriptors on it are closed. The block device is released when the last
			// fd closes. In practice this means Finalize's re-mount attempt may still
			// see EBUSY for a short window — mountPartitions already retries 5 times
			// to handle this.
			if lazyErr := exec.Command("umount", "-l", "-R", mountRoot).Run(); lazyErr != nil {
				log.Error().Str("mountRoot", mountRoot).Err(lazyErr).
					Msg("lazy umount also failed — filesystem may remain mounted; Finalize will retry mount")
			} else {
				log.Info().Str("mountRoot", mountRoot).
					Msg("lazy umount succeeded — mount detached; block device will release when all fds close")
			}
		}
	}
}

// maxDownloadAttempts is the number of download attempts before giving up.
const maxDownloadAttempts = 3

// downloadVerifyAndExtract downloads the image blob from opts.ImageURL, verifies
// its sha256 checksum if opts.ExpectedChecksum is set, then extracts it into
// opts.MountRoot. Retries up to maxDownloadAttempts with exponential backoff.
func (d *FilesystemDeployer) downloadVerifyAndExtract(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	log := logger()
	var lastErr error
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		if attempt > 1 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Warn().Dur("backoff", backoff).Int("attempt", attempt).Int("max", maxDownloadAttempts).
				Msg("network error downloading image blob — retrying")
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
		log.Warn().Int("attempt", attempt).Int("max", maxDownloadAttempts).Err(err).Msg("download attempt failed")
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

	log := logger()
	log.Info().Str("url", opts.ImageURL).Msg("downloading image blob")
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
		log.Info().Str("size", humanReadableBytes(totalBytes)).Msg("image blob size")
	}

	needsVerify := !opts.SkipVerify && opts.ExpectedChecksum != ""
	if opts.SkipVerify && opts.ExpectedChecksum != "" {
		log.Warn().Msg("checksum verification skipped for image download (--skip-verify set)")
	}

	return d.streamExtract(ctx, resp.Body, totalBytes, opts, needsVerify, progress)
}

// streamExtract streams the HTTP body directly into tar, computing the sha256
// concurrently via a pipe so we never buffer the full blob to disk. This uses
// constant memory regardless of image size.
//
// Compression detection is done by peeking at the first 4 magic bytes:
//   - gzip: 0x1f 0x8b → decompressed via pigz (parallel, all cores) if available,
//     otherwise via Go's compress/gzip.
//   - zstd: 0x28 0xb5 0x2f 0xfd → decompressed via external "zstd -dc" pipe.
//   - plain tar: no magic match → passed directly to tar.
//
// When needsVerify is true, the hasher result is checked after tar exits — if
// checksum mismatches, an error is returned (but extraction has already run, so
// the data on disk may be corrupt; the caller's retry loop will re-run from scratch).
func (d *FilesystemDeployer) streamExtract(ctx context.Context, body io.Reader, totalBytes int64, opts DeployOpts, needsVerify bool, progress ProgressFunc) error {
	log := logger()
	log.Info().Str("mount_root", opts.MountRoot).Msg("starting stream-extract")

	// Set up the reader chain: body → [hasher tee] → progress → decompressor? → tar stdin.
	var hasher hash.Hash
	var reader io.Reader = body

	if needsVerify {
		hasher = sha256.New()
		reader = io.TeeReader(body, hasher)
	}

	pr := &progressReader{r: reader, total: totalBytes, fn: progress, phase: "downloading+extracting", reporter: opts.Reporter}

	// Peek at the first 4 bytes to detect compression format.
	peeked := bufio.NewReaderSize(pr, 512)
	magic, peekErr := peeked.Peek(4)

	isGzip := peekErr == nil && len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b
	isZstd := peekErr == nil && len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd

	var tarSrc io.Reader = peeked
	var decompCmd *exec.Cmd

	switch {
	case isZstd:
		// zstd: pipe through external "zstd -dc". 3-5x faster decompression than gzip.
		log.Info().Msgf("detected zstd compression (magic %02x%02x%02x%02x) — decompressing via zstd -dc", magic[0], magic[1], magic[2], magic[3])
		zstdPath, zstdErr := exec.LookPath("zstd")
		if zstdErr != nil {
			return fmt.Errorf("zstd binary not found in PATH — cannot decompress .tar.zst image")
		}
		decompCmd = exec.CommandContext(ctx, zstdPath, "-dc", "--no-progress")
		pr2, pw2 := io.Pipe()
		decompCmd.Stdin = peeked
		decompCmd.Stdout = pw2
		if err := decompCmd.Start(); err != nil {
			return fmt.Errorf("zstd start: %w", err)
		}
		// Close pw2 when zstd exits (success or error) so tar sees EOF.
		go func() {
			_ = decompCmd.Wait()
			pw2.Close()
		}()
		tarSrc = pr2

	case isGzip:
		// gzip: prefer pigz (parallel gzip, uses all CPU cores) over in-process gzip.
		log.Info().Msgf("detected gzip compression (magic %02x%02x)", magic[0], magic[1])
		if pigzPath, pigzErr := exec.LookPath("pigz"); pigzErr == nil {
			log.Info().Str("pigz", pigzPath).Msg("decompressing via pigz (parallel gzip)")
			decompCmd = exec.CommandContext(ctx, pigzPath, "-dc")
			pr2, pw2 := io.Pipe()
			decompCmd.Stdin = peeked
			decompCmd.Stdout = pw2
			if err := decompCmd.Start(); err != nil {
				return fmt.Errorf("pigz start: %w", err)
			}
			go func() {
				_ = decompCmd.Wait()
				pw2.Close()
			}()
			tarSrc = pr2
		} else {
			// Fallback: Go's in-process gzip reader.
			log.Info().Msg("pigz not found — decompressing via Go gzip (single-core)")
			gz, err := gzip.NewReader(peeked)
			if err != nil {
				return fmt.Errorf("gzip.NewReader: %w", err)
			}
			defer gz.Close()
			tarSrc = gz
		}

	default:
		log.Info().Msg("no compression magic detected — treating stream as uncompressed tar")
	}

	// tar -xvf - streams each extracted filename to stdout, which runAndLog pipes
	// through the logger at Info level. This gives per-file visibility during
	// extraction without any extra logic.
	//
	// Flags used:
	//   --numeric-owner        preserve UID/GID as numbers (no user DB needed in initramfs)
	//   --xattrs               restore extended attributes from the archive
	//   --xattrs-include='*'   include all xattr namespaces (selinux, security.*, etc)
	//   --selinux              restore SELinux contexts
	//   --acls                 restore POSIX ACLs
	//   --ignore-failed-read   don't abort on read failures in weird files
	//   --warning=no-xattr-write --warning=no-unknown-keyword
	//                          demote xattr-write and unknown-pax-keyword warnings to info
	//                          (these cause exit code 2 on some files without being real failures)
	//   --warning=no-timestamp don't fail on files with timestamps in the future
	tarCmd := exec.CommandContext(ctx, "tar",
		"--numeric-owner",
		"--xattrs",
		"--xattrs-include=*",
		"--selinux",
		"--acls",
		"--ignore-failed-read",
		"--warning=no-xattr-write",
		"--warning=no-unknown-keyword",
		"--warning=no-timestamp",
		"-xvf", "-",
		"-C", opts.MountRoot,
	)
	tarCmd.Stdin = tarSrc

	if err := runAndLog(ctx, "tar", tarCmd); err != nil {
		// tar exit code 2 on extraction often means a handful of files failed but
		// most of the archive was written successfully. Log the error but don't
		// fail the deployment if the critical system files are present.
		// Check if /etc and /usr were extracted — if so, the failure was in something
		// non-critical and we can continue.
		if _, etcErr := os.Stat(filepath.Join(opts.MountRoot, "etc", "os-release")); etcErr == nil {
			if _, usrErr := os.Stat(filepath.Join(opts.MountRoot, "usr", "bin")); usrErr == nil {
				log.Warn().Err(err).Msg("tar extract reported errors but /etc and /usr are present — continuing")
			} else {
				return fmt.Errorf("tar extract failed: %w", err)
			}
		} else {
			return fmt.Errorf("tar extract failed: %w", err)
		}
	}

	log.Info().Str("read", humanReadableBytes(pr.written)).Msg("stream-extract complete")

	// Post-extraction checksum verification. If this fails the data is on disk
	// but potentially corrupt — the caller's retry loop will restart the download.
	if needsVerify && hasher != nil {
		gotChecksum := hex.EncodeToString(hasher.Sum(nil))
		if gotChecksum != opts.ExpectedChecksum {
			return fmt.Errorf("image integrity check failed: downloaded blob sha256=%s does not match "+
				"expected=%s — the image may be corrupt or the server checksum is stale; "+
				"use --skip-verify to deploy anyway", gotChecksum, opts.ExpectedChecksum)
		}
		log.Info().Str("sha256", gotChecksum).Msg("image checksum verified")
	}

	if progress != nil {
		progress(pr.written, totalBytes, "extract complete")
	}
	return nil
}

// progressReader wraps an io.Reader and calls a ProgressFunc on each read.
// It also calls ProgressReporter.Update when reporter is set, enabling real-time
// byte-level structured progress events to be sent to the server.
type progressReader struct {
	r        io.Reader
	total    int64
	written  int64
	fn       ProgressFunc
	phase    string
	reporter ProgressReporter
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.written += int64(n)
	if p.fn != nil {
		p.fn(p.written, p.total, p.phase)
	}
	if p.reporter != nil {
		p.reporter.Update(p.written)
	}
	return n, err
}
