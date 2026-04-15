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
	"sync"
	"sync/atomic"
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

// ResolvedDisk returns the target disk path resolved by Preflight.
// Returns "" if Preflight has not been called yet.
func (d *FilesystemDeployer) ResolvedDisk() string { return d.targetDisk }

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
		resp             *http.Response
		totalBytes       int64
		serverChecksum   string // X-Clonr-Blob-SHA256 response header, if present
		err              error
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
		serverChecksum := resp.Header.Get("X-Clonr-Blob-SHA256")
		blobCh <- blobResult{resp: resp, totalBytes: resp.ContentLength, serverChecksum: serverChecksum}
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

	// Determine RAID topology: if any RAID array has members that look like
	// partition names (e.g. "sda2", "nvme0n1p3") rather than whole disks
	// ("sda", "sdb"), the arrays must be created AFTER partitioning — the
	// partition devices don't exist yet. For RAID-on-whole-disk (members are
	// raw disks), create arrays BEFORE partitioning so the md device can be
	// partitioned by partitionDisk.
	rainOnPartitions := raidMembersArePartitions(d.layout)
	if len(d.layout.RAIDArrays) > 0 {
		logger().Info().
			Bool("raid_on_partitions", rainOnPartitions).
			Int("raid_array_count", len(d.layout.RAIDArrays)).
			Msg("RAID topology detected")
	}

	// Create RAID arrays before partitioning for RAID-on-whole-disk topology.
	if len(d.layout.RAIDArrays) > 0 && !rainOnPartitions {
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

	// Create RAID arrays after partitioning for md-on-partitions topology.
	// The partition devices (sda2, sdb2, etc.) now exist on the raw disks.
	if len(d.layout.RAIDArrays) > 0 && rainOnPartitions {
		logger().Info().Int("count", len(d.layout.RAIDArrays)).Msg("creating RAID arrays (md-on-partitions: after partitioning)")
		if err := CreateRAIDArrays(ctx, d.layout, hardware.SystemInfo{}); err != nil {
			doRollback("RAID array creation failed")
			if opts.Reporter != nil {
				opts.Reporter.EndPhase(err.Error())
			}
			return fmt.Errorf("deploy: create raid arrays: %w", err)
		}
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

	// Prefer the server-advertised tar checksum (X-Clonr-Blob-SHA256) over the
	// image-record checksum. For filesystem-format images the image record stores a
	// directory-level hash (sha256 of the rootfs tree), NOT a tar stream hash, so it
	// cannot be used to verify the tar blob. Only use it when the server confirms it
	// is a tar stream hash via X-Clonr-Blob-SHA256. Without the header, skip
	// verification entirely so the first-stream case (before the server caches the
	// tar checksum sidecar) does not falsely reject a correct download.
	expectedChecksum := opts.ExpectedChecksum
	if blob.serverChecksum != "" {
		if expectedChecksum != "" && expectedChecksum != blob.serverChecksum {
			logger().Warn().
				Str("img_checksum", expectedChecksum).
				Str("server_header", blob.serverChecksum).
				Msg("X-Clonr-Blob-SHA256 header differs from image record checksum — using header value (tar stream hash)")
		}
		expectedChecksum = blob.serverChecksum
		logger().Info().Str("sha256", expectedChecksum).
			Msg("using server-advertised tar checksum for integrity verification")
	} else {
		// No X-Clonr-Blob-SHA256 header: the image record checksum is a directory
		// hash, not a tar stream hash — using it would cause a false mismatch.
		// Skip integrity verification; the server will cache the tar checksum after
		// the first successful stream and advertise it on subsequent downloads.
		if expectedChecksum != "" {
			logger().Warn().Msg("server did not advertise X-Clonr-Blob-SHA256; image record checksum is a directory hash and cannot verify the tar stream — skipping integrity verification for this stream")
			expectedChecksum = ""
		} else {
			logger().Warn().Msg("server did not advertise X-Clonr-Blob-SHA256 and no checksum in image record — skipping integrity verification")
		}
	}

	// Update opts with the resolved checksum for streamExtract.
	verifyOpts := opts
	verifyOpts.ExpectedChecksum = expectedChecksum

	// Signal downloading phase with byte total for real-time UI progress.
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("downloading", blob.totalBytes)
	}

	needsVerify := !opts.SkipVerify && expectedChecksum != ""
	if opts.SkipVerify && expectedChecksum != "" {
		logger().Warn().Msg("checksum verification skipped for image download (--skip-verify set)")
	}

	// Attempt extraction with retries. On a watchdog-triggered stall or a
	// truncated stream, close the current response, wait briefly, then re-issue
	// a fresh HTTP request rather than retrying from the same dead TCP connection.
	var extractErr error
	for attempt := 1; attempt <= maxDownloadAttempts; attempt++ {
		var body io.Reader
		var bodyClose func()
		if attempt == 1 {
			// First attempt uses the pre-fetched connection (TCP already established).
			body = blob.resp.Body
			bodyClose = func() {} // closed by the outer defer blob.resp.Body.Close()
		} else {
			if ctx.Err() != nil {
				// Parent context cancelled — do not retry.
				break
			}
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			logger().Warn().
				Int("attempt", attempt).Int("max", maxDownloadAttempts).
				Dur("backoff", backoff).
				Msg("blob stream failed — re-issuing HTTP request for retry")
			select {
			case <-ctx.Done():
				break
			case <-time.After(backoff):
			}
			retryReq, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
			if reqErr != nil {
				extractErr = fmt.Errorf("build retry request: %w", reqErr)
				break
			}
			if opts.AuthToken != "" {
				retryReq.Header.Set("Authorization", "Bearer "+opts.AuthToken)
			}
			retryResp, httpErr := http.DefaultClient.Do(retryReq)
			if httpErr != nil {
				extractErr = fmt.Errorf("retry HTTP request failed: %w", httpErr)
				continue
			}
			if retryResp.StatusCode != http.StatusOK && retryResp.StatusCode != http.StatusPartialContent {
				retryResp.Body.Close()
				extractErr = fmt.Errorf("retry HTTP %d from %s", retryResp.StatusCode, opts.ImageURL)
				continue
			}
			body = retryResp.Body
			bodyClose = func() { retryResp.Body.Close() }
		}

		extractErr = d.streamExtract(ctx, body, blob.totalBytes, verifyOpts, needsVerify, progress)
		bodyClose()
		if extractErr == nil {
			break
		}
		logger().Error().Err(extractErr).Int("attempt", attempt).Int("max", maxDownloadAttempts).
			Msg("stream-extract attempt failed")
	}

	if extractErr != nil {
		logger().Error().Err(extractErr).Msg("image download/extract failed after all attempts")
		doRollback("image download/extract failed")
		if opts.Reporter != nil {
			opts.Reporter.EndPhase(extractErr.Error())
		}
		return fmt.Errorf("deploy: extract: %w", extractErr)
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

	// Re-create the partition device list from the stored layout, using the same
	// per-target counter logic as createFilesystems so partition numbers are
	// consistent across both phases.
	partDevs := partitionDevices(d.targetDisk, d.layout)

	// Re-mount all partitions so applyNodeConfig can write into the filesystem.
	if err := d.mountPartitions(ctx, partDevs, mountRoot); err != nil {
		return fmt.Errorf("deploy: finalize: re-mount partitions: %w", err)
	}
	defer d.unmountAll(mountRoot)

	if err := applyNodeConfig(ctx, cfg, mountRoot); err != nil {
		return err
	}

	// Install the GRUB bootloader if the layout includes a bios_grub partition.
	// For RAID1 nodes, grub2-install must run on EVERY raw member disk so the
	// node can boot from either disk if one fails. We collect grub install targets
	// from two sources:
	//   1. d.targetDisk (the raw disk picked by selectTargetDisk for single-disk layouts).
	//   2. Every member disk in every RAIDSpec (for RAID-on-whole-disk layouts).
	// For a RAID1 layout with two members both disks get a bootloader; for a
	// single-disk layout only that disk does. Duplicates are de-duped.
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

		// Determine whether this is a RAID-on-whole-disk layout (all non-biosboot
		// partitions live on md devices). grubInstallTargets() returns the raw member
		// disks in all RAID topologies because GRUB's diskfilter driver is read-only
		// and cannot write through md virtual devices. For RAID-on-whole-disk, the raw
		// member disks have no standalone partition table, so --force is required to
		// skip GRUB's "no filesystem found" safety check. We also bake mdraid1x and
		// diskfilter into core.img so GRUB can find /boot on the md array at runtime.
		raidOnWholeDisk := false
		for _, p := range d.layout.Partitions {
			if p.Device == "" {
				continue
			}
			isBIOSBoot := false
			for _, flag := range p.Flags {
				if flag == "bios_grub" || flag == "biosboot" {
					isBIOSBoot = true
					break
				}
			}
			if !isBIOSBoot {
				base := strings.TrimPrefix(p.Device, "/dev/")
				if strings.HasPrefix(base, "md") {
					raidOnWholeDisk = true
					break
				}
			}
		}

		// Collect all raw disks that need a bootloader.
		grubTargets := grubInstallTargets(d.targetDisk, d.layout)
		log.Info().Strs("disks", grubTargets).Bool("raid_on_whole_disk", raidOnWholeDisk).
			Msg("finalize: installing GRUB bootloader on all RAID member disks (BIOS/GPT)")

		var grubSucceeded int
		var grubLastErr error
		for _, grubDisk := range grubTargets {
			log.Info().Str("disk", grubDisk).Str("bootDir", bootDir).
				Msg("finalize: running grub2-install")
			grubArgs := []string{
				"--target=i386-pc",
				"--boot-directory=" + bootDir,
				"--recheck",
			}
			if len(d.layout.RAIDArrays) > 0 {
				// For any RAID topology, grub2-install detects md superblock metadata
				// on the target disk's partitions and refuses to embed without --force.
				// --force bypasses the "embedding is not possible, but this is required
				// for RAID and LVM install" error that grub2-install emits when it
				// detects RAID membership on the disk.
				grubArgs = append(grubArgs, "--force")
			}
			if raidOnWholeDisk {
				// RAID-on-whole-disk additionally requires:
				//
				// --skip-fs-probe: The raw member disk has no partition table of its
				//   own (it was handed directly to mdadm), so grub2-probe cannot find
				//   any filesystem. This flag tells GRUB to write directly to the raw
				//   disk's MBR boot sector without probing.
				//
				// --modules: bake mdraid1x, diskfilter, and the /boot filesystem
				//   driver into core.img so GRUB can locate and READ /boot on the md
				//   array at runtime before loading any module files from a partition.
				//   Without the filesystem module (xfs, ext2, etc.), GRUB can assemble
				//   the md array but cannot read grub.cfg from it and drops to rescue.
				//
				//   part_gpt is required to read GPT partition tables on the raw disks.
				//   xfs and ext2 cover the two common /boot filesystems on RHEL/Rocky.
				grubArgs = append(grubArgs,
					"--skip-fs-probe",
					"--modules=mdraid1x diskfilter part_gpt xfs ext2",
				)
			}
			grubArgs = append(grubArgs, grubDisk)
			if err := runAndLog(ctx, "grub2-install", exec.CommandContext(ctx, "grub2-install", grubArgs...)); err != nil {
				grubLastErr = err
				log.Warn().Err(err).Str("disk", grubDisk).
					Msg("WARNING: finalize: grub2-install failed on this disk — will continue if other member disks succeed")
			} else {
				grubSucceeded++
				log.Info().Str("disk", grubDisk).Msg("finalize: GRUB bootloader installed")
			}
		}

		// For BIOS deploys, a failed grub2-install means the node cannot boot.
		// If ALL target disks failed, the deployment is fatal — return an error so
		// the deploy-complete callback is never fired and deploy-failed is sent instead.
		// If at least one raw RAID member disk succeeded, the node is bootable (RAID1
		// redundancy means one good disk is enough), so log a warning and continue.
		if grubSucceeded == 0 && len(grubTargets) > 0 {
			return &BootloaderError{
				Targets: grubTargets,
				Cause:   grubLastErr,
			}
		}
		if grubLastErr != nil {
			log.Warn().Int("succeeded", grubSucceeded).Int("total", len(grubTargets)).
				Msg("WARNING: finalize: grub2-install failed on some disks but at least one succeeded — node is bootable (degraded RAID)")
		}
	}

	// If the layout includes RAID arrays, write /etc/mdadm.conf BEFORE running
	// applyBootConfig so that the single dracut invocation in applyBootConfig
	// picks up the conf and bakes it into the initramfs. GenerateMdadmConf no
	// longer triggers its own dracut run — the one in applyBootConfig is
	// sufficient and is built with the correct RAID module flags.
	if len(d.layout.RAIDArrays) > 0 {
		if err := GenerateMdadmConf(ctx, mountRoot); err != nil {
			// Non-fatal: log and continue. The node may still boot if the kernel
			// auto-assembles the RAID arrays via superblock scanning.
			logger().Warn().Err(err).Msg("generate mdadm.conf failed (non-fatal)")
		}
	}

	// Apply boot-critical post-install steps: grub2-mkconfig, dracut, fstab
	// UUID regen, machine-id truncation, and SSH host key removal.
	// These steps are unconditional — they must run on every filesystem
	// deployment regardless of whether grub2-install succeeded above.
	// grub2-install writes the bootloader binary; applyBootConfig makes it
	// functional by regenerating the config with the target's actual kernel,
	// initramfs, and partition UUIDs.
	//
	// For applyBootConfig's targetDisk argument (used for kernel-install fallback
	// only), pass the first grub install target so it references a real raw disk.
	bootConfigDisk := d.targetDisk
	if targets := grubInstallTargets(d.targetDisk, d.layout); len(targets) > 0 {
		bootConfigDisk = targets[0]
	}
	if err := applyBootConfig(ctx, mountRoot, bootConfigDisk, d.layout, partDevs); err != nil {
		return fmt.Errorf("deploy: finalize: boot config: %w", err)
	}

	// Append shared-storage fstab entries (NFS, Lustre, BeeGFS, CIFS, etc.)
	// and auto-create mount point directories in the deployed filesystem.
	// Called after applyBootConfig so the base fstab already exists.
	if err := applyExtraMounts(ctx, mountRoot, cfg.ExtraMounts); err != nil {
		// Non-fatal: a bad shared-storage config must not prevent the node from
		// booting. The operator can correct and redeploy.
		logger().Warn().Err(err).Msg("WARNING: finalize: extra mounts failed (non-fatal)")
	}

	return nil
}

// raidMembersArePartitions returns true when at least one RAID array has members
// that refer to partition devices (e.g. "sda2", "nvme0n1p3") rather than whole
// disks ("sda", "sdb"). This determines deploy ordering:
//   - false (whole-disk members): create RAID arrays BEFORE partitioning so the
//     md virtual device can be partitioned by partitionDisk.
//   - true (partition members): partition raw disks FIRST so the partition devices
//     exist, then create RAID arrays from those partition slices.
func raidMembersArePartitions(layout api.DiskLayout) bool {
	for _, raid := range layout.RAIDArrays {
		for _, member := range raid.Members {
			// A partition member has trailing digits after a non-digit character.
			// Examples: "sda2", "sdb3", "nvme0n1p2".
			// Whole-disk members: "sda", "sdb", "nvme0n1".
			// Size selectors: "smallest-2" -- not a partition.
			if strings.HasPrefix(member, "smallest-") {
				continue
			}
			// Strip /dev/ prefix if present.
			base := strings.TrimPrefix(member, "/dev/")
			// Check if the name ends in a digit AND contains a non-digit before it.
			// This distinguishes "sda2" (partition) from "sda" (disk) or "md0" (md device).
			if len(base) > 0 && base[len(base)-1] >= '0' && base[len(base)-1] <= '9' {
				// Ensure there's a non-digit somewhere in the name (ruling out pure numeric names).
				hasNonDigit := false
				for _, c := range base {
					if c < '0' || c > '9' {
						hasNonDigit = true
						break
					}
				}
				if hasNonDigit {
					return true
				}
			}
		}
	}
	return false
}

// partitionDevices reconstructs the partition device path slice for a given layout,
// using the same per-target counter logic as createFilesystems. This must be kept
// in sync with createFilesystems so Finalize remounts the correct devices.
func partitionDevices(defaultDisk string, layout api.DiskLayout) []string {
	devs := make([]string, len(layout.Partitions))
	targetCount := make(map[string]int)
	mdOnPartitions := raidMembersArePartitions(layout)
	for i, p := range layout.Partitions {
		target := resolvePartitionDisk(defaultDisk, p)
		targetCount[target]++
		devs[i] = resolveFormatTargetForLayout(defaultDisk, p, targetCount[target], mdOnPartitions)
	}
	return devs
}

// grubInstallTargets returns the ordered, de-duped list of disk devices that
// grub2-install must target for i386-pc (BIOS) installs. For single-disk
// layouts this is just [defaultDisk].
//
// BIOS RAID rule (both RAID-on-whole-disk and md-on-partitions):
// grub2-install must ALWAYS target the raw member disks, never the md virtual
// device. GRUB's diskfilter driver is read-only -- "diskfilter writes are not
// supported" -- so grub2-install /dev/md0 always fails. Each raw member disk
// has its own biosboot partition created during partitioning; GRUB writes
// core.img to each member independently so both disks are independently
// bootable, matching RAID1's redundancy guarantee.
//
// UEFI RAID: this function is only called from the hasBIOSGrub block; the
// UEFI path uses efibootmgr and does not go through this function.
func grubInstallTargets(defaultDisk string, layout api.DiskLayout) []string {
	if len(layout.RAIDArrays) == 0 {
		return []string{defaultDisk}
	}

	// For any RAID topology (RAID-on-whole-disk or md-on-partitions), target the
	// raw member disks. GRUB cannot write through the md virtual device regardless
	// of topology -- the diskfilter driver is strictly read-only at install time.
	seen := make(map[string]bool)
	var targets []string
	add := func(dev string) {
		if !seen[dev] {
			seen[dev] = true
			targets = append(targets, dev)
		}
	}

	for _, raid := range layout.RAIDArrays {
		for _, member := range raid.Members {
			// Skip size-based selectors -- concrete device names only.
			if strings.HasPrefix(member, "smallest-") {
				continue
			}
			var dev string
			if strings.HasPrefix(member, "/dev/") {
				dev = member
			} else {
				dev = "/dev/" + member
			}
			// For md-on-partitions BIOS RAID, members are partition devices
			// (e.g. "sda2", "sdb2"). grub2-install must target the raw parent
			// disk ("/dev/sda", "/dev/sdb") — not the partition — so that GRUB
			// can write core.img into the biosboot partition (sda1/sdb1).
			// Strip trailing partition number digits to recover the disk path.
			// This is safe for whole-disk members ("sda", "sdb") because they
			// have no trailing digits to strip.
			dev = rawDiskFromDevice(dev)
			add(dev)
		}
	}

	// If no concrete targets found (all selectors), fall back to defaultDisk.
	if len(targets) == 0 {
		return []string{defaultDisk}
	}
	return targets
}

// rawDiskFromDevice strips trailing partition number digits from a block device
// path to recover the parent disk. For example:
//
//	/dev/sda2      → /dev/sda
//	/dev/sdb3      → /dev/sdb
//	/dev/nvme0n1p2 → /dev/nvme0n1
//	/dev/sda       → /dev/sda    (whole disk — no change)
//	/dev/nvme0n1   → /dev/nvme0n1 (whole NVMe disk — no change)
//
// Two partition naming conventions are handled:
//   - NVMe: "p" separator before the partition number (nvme0n1p2 → nvme0n1).
//     NVMe whole-disk names end in a digit (nvme0n1) so the "p" separator is
//     the only safe way to detect partition devices for this class.
//   - Traditional (sd/hd/vd etc.): partition number appended directly (sda2 →
//     sda). These whole-disk names end in a letter, so stripping trailing
//     digits is safe.
func rawDiskFromDevice(dev string) string {
	base := filepath.Base(dev)
	dir := filepath.Dir(dev)

	// NVMe devices: only use the "p"-separator method. NVMe whole-disk names
	// end in a digit (e.g. nvme0n1) so we must NOT fall through to the generic
	// digit-strip below — that would wrongly turn "nvme0n1" into "nvme0n".
	if strings.HasPrefix(base, "nvme") {
		// Partition devices look like nvme0n1p2. Find the last "p" preceded by
		// a digit (the namespace-to-partition separator) and everything after
		// it must be digits.
		for i := len(base) - 1; i > 0; i-- {
			if base[i] == 'p' && base[i-1] >= '0' && base[i-1] <= '9' {
				suffix := base[i+1:]
				allDigits := len(suffix) > 0
				for _, c := range suffix {
					if c < '0' || c > '9' {
						allDigits = false
						break
					}
				}
				if allDigits {
					return filepath.Join(dir, base[:i])
				}
				break
			}
		}
		// No partition suffix found — already a whole-disk name; return as-is.
		return dev
	}

	// Traditional devices (sda, sdb, hda, vda, …): partition numbers are
	// appended as trailing digits. Whole-disk names end in a letter, so
	// stripping trailing digits is safe.
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	if i < len(base) {
		// Only strip if the non-digit prefix is non-empty (safety check).
		if i > 0 {
			return filepath.Join(dir, base[:i])
		}
	}
	return dev
}

// partitionDisk wipes and repartitions the target disk(s) according to the layout.
//
// For single-disk layouts all partitions land on `disk`. For RAID-on-whole-disk
// layouts (where every PartitionSpec.Device == "md0" etc.) the target is the md
// device, NOT the raw member disks. The member disks were handed to mdadm --create
// already and are now part of the running array; partitioning them directly would
// fail with EBUSY. Instead we partition the md device and let mdadm propagate the
// layout to both members via its partition-table-sync feature.
//
// Uses sgdisk for GPT (standard for BIOS+GPT and EFI systems).
func (d *FilesystemDeployer) partitionDisk(ctx context.Context, disk string) error {
	log := logger()

	// Build a map of target-disk → list of (originalIndex, PartitionSpec) so we
	// can issue one sgdisk call per unique target disk. In single-disk layouts
	// there is only one target. In RAID-on-whole-disk layouts all partitions target
	// the md device (e.g. /dev/md0) and `disk` is unused for partitioning.
	type partEntry struct {
		origIdx int
		spec    api.PartitionSpec
	}
	targetMap := make(map[string][]partEntry) // target disk path → partitions
	for i, p := range d.layout.Partitions {
		target := resolvePartitionDisk(disk, p)
		// Skip md virtual devices — they are assembled AFTER partitioning raw disks
		// and do not need to be wiped or partitioned by sgdisk. Their filesystems
		// are created by createFilesystems once the md arrays are online.
		if isMdDevice(target) {
			continue
		}
		targetMap[target] = append(targetMap[target], partEntry{i, p})
	}

	for target, parts := range targetMap {
		log.Info().Str("disk", target).Msg("wiping existing partition table")
		if err := runCmd(ctx, "sgdisk", "--zap-all", target); err != nil {
			log.Warn().Str("disk", target).Err(err).Msg("sgdisk --zap-all failed, trying wipefs")
			if err2 := runCmd(ctx, "wipefs", "-a", target); err2 != nil {
				return fmt.Errorf("wipe disk %s: sgdisk failed (%v) and wipefs also failed (%v) — "+
					"check if the disk has an active RAID superblock (wipefs -a %s)", target, err, err2, target)
			}
		}
		log.Info().Str("disk", target).Msg("disk wiped")

		// Build sgdisk partition arguments. Partition numbers are re-indexed per
		// target disk starting at 1 so each target gets a self-consistent GPT.
		// d.layout.Partitions is ordered; for targets with all partitions (the
		// common case) this preserves the original order.
		args := []string{}
		for localNum, pe := range parts {
			num := localNum + 1 // 1-based partition number on this target
			p := pe.spec
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
			for _, flag := range p.Flags {
				switch flag {
				case "esp", "boot":
					args = append(args, fmt.Sprintf("--typecode=%d:ef00", num))
				case "bios_grub":
					args = append(args, fmt.Sprintf("--typecode=%d:ef02", num))
				}
			}

			sizeStr := "fill"
			if p.SizeBytes > 0 {
				sizeStr = humanReadableBytes(p.SizeBytes)
			}
			log.Info().Int("partition", num).Str("disk", target).
				Str("mountpoint", p.MountPoint).Str("filesystem", p.Filesystem).
				Str("size", sizeStr).Str("flags", strings.Join(p.Flags, ",")).
				Msg("defining partition")
		}
		args = append(args, target)

		log.Info().Int("count", len(parts)).Str("disk", target).Msg("running sgdisk to create partitions")
		if err := runAndLog(ctx, "sgdisk", exec.CommandContext(ctx, "sgdisk", args...)); err != nil {
			return fmt.Errorf("failed to create partitions on %s — "+
				"check if the disk has an existing RAID superblock (wipefs -a %s): %w",
				target, target, err)
		}
		log.Info().Str("disk", target).Msg("sgdisk partition creation succeeded")

		// Allow kernel to re-read the new partition table.
		log.Info().Str("disk", target).Msg("running partprobe to re-read partition table")
		_ = runCmd(ctx, "partprobe", target)
		_ = runCmd(ctx, "udevadm", "settle")
		log.Info().Msg("triggering device node creation for new partitions")
		_ = exec.CommandContext(ctx, "mdev", "-s").Run()
		_ = exec.CommandContext(ctx, "blockdev", "--rereadpt", target).Run()
		if err := waitForPartitions(ctx, target, len(parts), 15); err != nil {
			log.Warn().Err(err).Str("disk", target).
				Msg("waitForPartitions timed out — attempting manual device node creation")
			ensurePartitionNodes(target, len(parts))
		}
		log.Info().Str("disk", target).Msg("partition table re-read complete")
	}

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
// md devices (md0, md1, etc.) use the "p" separator just like nvme: /dev/md0p1.
func partitionDevice(disk string, num int) string {
	base := filepath.Base(disk)
	if strings.Contains(disk, "nvme") || strings.Contains(disk, "mmcblk") || strings.HasPrefix(base, "md") {
		return fmt.Sprintf("%sp%d", disk, num)
	}
	return fmt.Sprintf("%s%d", disk, num)
}

// isMdDevice returns true if the device path refers to a Linux software RAID
// (md) device, e.g. "/dev/md0", "/dev/md127". These devices are assembled by
// mdadm AFTER raw disk partitioning and must not be treated as sgdisk targets.
func isMdDevice(dev string) bool {
	base := strings.TrimPrefix(dev, "/dev/")
	if !strings.HasPrefix(base, "md") {
		return false
	}
	// Must be "md" followed only by digits (e.g. "md0", "md1") not "mda" or "mdp"
	rest := base[2:]
	if len(rest) == 0 {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// resolvePartitionDisk returns the block device that a PartitionSpec should be
// created on. When spec.Device is set (e.g. "md0" or "/dev/md0"), that device
// is used — this covers RAID-on-whole-disk layouts where the layout recommender
// places all partitions on top of an md array. When spec.Device is empty the
// caller-supplied defaultDisk is used (single-disk or raw-member layouts).
func resolvePartitionDisk(defaultDisk string, spec api.PartitionSpec) string {
	if spec.Device == "" {
		return defaultDisk
	}
	if strings.HasPrefix(spec.Device, "/dev/") {
		return spec.Device
	}
	return "/dev/" + spec.Device
}

// resolveFormatTarget returns the block device path that mkfs should format for
// partition number num (1-based) of the given spec. It calls resolvePartitionDisk
// to pick the right parent disk then appends the partition number suffix.
//
// For RAID-on-whole-disk layouts (spec.Device == "md0") the md device IS
// partitioned by sgdisk so the format target is "/dev/md0p1" etc.
// For single-disk layouts it returns "/dev/sda1" etc.
// For md-on-partitions layouts the md device represents an assembled array
// that should NOT have a partition suffix — use resolveFormatTargetForLayout.
func resolveFormatTarget(defaultDisk string, spec api.PartitionSpec, num int) string {
	disk := resolvePartitionDisk(defaultDisk, spec)
	return partitionDevice(disk, num)
}

// resolveFormatTargetForLayout returns the format target path, taking topology
// into account. For md-on-partitions layouts, md devices (md0, md1, md2) are
// assembled RAID arrays whose filesystems land on the device itself, not on a
// partition. For RAID-on-whole-disk, the md device is partitioned by sgdisk so
// the partition number suffix applies normally.
func resolveFormatTargetForLayout(defaultDisk string, spec api.PartitionSpec, num int, mdOnPartitions bool) string {
	disk := resolvePartitionDisk(defaultDisk, spec)
	if mdOnPartitions && isMdDevice(disk) {
		// md device is the assembled RAID array — format it directly.
		return disk
	}
	return partitionDevice(disk, num)
}

// createFilesystems creates the appropriate filesystem on each partition.
// Returns a slice of resolved partition device paths in layout order.
//
// For md-on-partitions layouts the Partition entries for md0/md1/md2 represent
// the assembled md arrays themselves. For md-on-partitions layouts the md device
// entries (md0, md1, md2) are formatted directly without a partition suffix.
// For RAID-on-whole-disk layouts the md device is partitioned by sgdisk and
// filesystems land on /dev/md0p1, /dev/md0p2, etc.
func (d *FilesystemDeployer) createFilesystems(ctx context.Context, disk string) ([]string, error) {
	log := logger()
	devs := make([]string, len(d.layout.Partitions))

	// Detect md-on-partitions topology so resolveFormatTargetForLayout can
	// return the md device path directly (no partition suffix).
	mdOnPartitions := raidMembersArePartitions(d.layout)

	// Compute per-target local partition numbers. sgdisk numbers partitions
	// starting from 1 per target disk. When all partitions share the same target
	// (single-disk or all-on-md0 layouts) localNum == i+1. When partitions are
	// spread across multiple targets we need independent counters per target.
	targetCount := make(map[string]int) // target disk path → running count
	for i, p := range d.layout.Partitions {
		target := resolvePartitionDisk(disk, p)
		targetCount[target]++
		localNum := targetCount[target]
		dev := resolveFormatTargetForLayout(disk, p, localNum, mdOnPartitions)
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
		case "", "raw", "biosboot", "bios_grub":
			// No filesystem — raw partition for BIOS boot (GPT BIOS boot partition),
			// an explicitly unformatted partition, or a RAID member slice that will be
			// assembled into an md array. sgdisk already set the type GUID.
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
// Partitions are sorted by mountpoint depth (shortest first) so that parent
// mounts (e.g. /) are established before child mounts (e.g. /boot). Relying
// on layout order is not safe — RAID layouts interleave raw-disk entries with
// md-device entries and the md entries may arrive in any order.
// mountEntry is a resolved partition-to-mountpoint mapping used by
// mountPartitions and sortMountEntries.
type mountEntry struct {
	dev   string
	mount string
	fs    string
}

// sortMountEntries sorts a slice of mountEntry values by mountpoint depth
// (number of "/" characters) so that parent mountpoints are established before
// child mountpoints. "/" (depth 1) sorts before "/boot" (depth 2), which sorts
// before "/boot/efi" (depth 3). When two mountpoints have the same depth,
// lexicographic order is used for determinism.
//
// This must run before any mount(8) calls: Linux refuses to mount a filesystem
// at a child path if the parent filesystem has not been mounted yet, and
// mounting in the wrong order silently hides content behind a later mount
// (e.g. mounting /boot before / means /boot's content goes to the in-memory
// rootfs, then the empty /boot partition shadows it).
func sortMountEntries(mps []mountEntry) {
	for i := 0; i < len(mps)-1; i++ {
		for j := i + 1; j < len(mps); j++ {
			li := strings.Count(mps[i].mount, "/")
			lj := strings.Count(mps[j].mount, "/")
			if lj < li || (lj == li && mps[j].mount < mps[i].mount) {
				mps[i], mps[j] = mps[j], mps[i]
			}
		}
	}
}

func (d *FilesystemDeployer) mountPartitions(ctx context.Context, devs []string, mountRoot string) error {
	var mps []mountEntry
	for i, p := range d.layout.Partitions {
		if p.MountPoint == "" || p.Filesystem == "swap" {
			continue
		}
		mps = append(mps, mountEntry{dev: devs[i], mount: p.MountPoint, fs: p.Filesystem})
	}

	// Sort by mountpoint depth so parent paths are mounted before children.
	// This guarantees / is mounted before /boot, and /boot before /boot/efi,
	// regardless of the order partitions appear in the layout definition.
	// Without this sort, a tar extract would land /boot content on the root
	// filesystem's /boot directory, then the empty /boot partition would be
	// mounted on top, hiding the kernel from GRUB.
	sortMountEntries(mps)

	mountOrder := make([]string, len(mps))
	for i, m := range mps {
		mountOrder[i] = m.mount + "=" + m.dev
	}
	logger().Info().Strs("order", mountOrder).Msg("mountPartitions: mount order after depth sort")

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

// hasAnymountUnder reports whether any filesystem is currently mounted at prefix
// or under it (prefix + "/"), by reading /proc/mounts. This is the authoritative
// check for whether a lazy umount actually detached — unlike umount exit codes,
// which are unreliable on shared mounts.
func hasAnymountUnder(prefix string) bool {
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		target := fields[1]
		if target == prefix || strings.HasPrefix(target, prefix+"/") {
			return true
		}
	}
	return false
}

// fsSyncMountRoot freezes then immediately thaws every XFS filesystem mounted
// under mountRoot using fsfreeze(8) (util-linux). freeze(-f) forces the XFS log
// to fully commit all in-memory journal entries to disk and quiesces I/O;
// thaw(-u) resumes normal operation. After this cycle the filesystem has no
// pending dirty log entries and is safe to unmount without EBUSY.
//
// XFS uses an asynchronous delayed-logging model: sync(2) flushes the page cache
// but does NOT wait for the XFS circular log buffer to commit. On a large tar
// extraction that writes thousands of inodes, the log can remain active for
// several seconds after sync returns, causing umount to fail with EBUSY.
// fsfreeze is the only reliable way to drain it without a fixed-duration sleep.
//
// Note: we use fsfreeze(8) from util-linux, NOT xfs_freeze(8) from xfsprogs.
// On Rocky/RHEL 9, xfs_freeze is a shell script wrapper that calls xfs_io —
// it cannot execute in the initramfs environment where /usr/bin/sh does not
// exist and xfs_io is not available. fsfreeze is a standalone binary that
// requires only libc.
//
// Mounts are processed deepest-first so nested mounts (e.g. /boot nested inside /)
// are frozen before their parent.
func fsSyncMountRoot(mountRoot string) {
	log := logger()

	// Read /proc/mounts and find all XFS mounts under mountRoot (or exactly mountRoot).
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		log.Warn().Err(err).Msg("fsSyncMountRoot: cannot read /proc/mounts — skipping fsfreeze")
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
		if out, err := exec.Command("fsfreeze", "-f", mp).CombinedOutput(); err != nil {
			log.Warn().Str("mountpoint", mp).Err(err).Str("output", string(out)).
				Msg("fsfreeze -f failed — XFS log may not be fully committed")
			continue
		}
		log.Info().Str("mountpoint", mp).Msg("fsfreeze: XFS log committed and filesystem frozen")
		// Immediately thaw — we only needed the freeze to drain the log.
		if out, err := exec.Command("fsfreeze", "-u", mp).CombinedOutput(); err != nil {
			log.Warn().Str("mountpoint", mp).Err(err).Str("output", string(out)).
				Msg("fsfreeze -u failed — filesystem may remain frozen; unmount will proceed")
		} else {
			log.Info().Str("mountpoint", mp).Msg("fsfreeze: filesystem thawed")
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
//
// Three-step strategy:
//
//  1. fsfreeze/thaw — forces XFS delayed-log commit. sync(2) alone does not drain
//     XFS's in-memory circular log buffer, which keeps the filesystem busy after
//     large tar extractions. fsfreeze(8) is the only reliable way to drain it.
//
//  2. mount --make-rprivate — cuts shared mount propagation. In the initramfs the
//     rootfs is typically a "shared" mount; filesystems mounted inside it inherit
//     that propagation peer relationship. Linux refuses umount(2) and MNT_DETACH on
//     shared mounts that have propagation peers, returning EBUSY even with no open
//     fds. Making the subtree rprivate severs those peers, enabling clean unmount.
//
//  3. Recursive unmount with retries, falling back to lazy detach. Lazy detach
//     success is verified via /proc/mounts rather than exit code — umount -l can
//     return non-zero even when MNT_DETACH succeeded at the kernel level.
func (d *FilesystemDeployer) unmountAll(mountRoot string) {
	log := logger()

	// Step 1: Force XFS log commit before unmounting.
	fsSyncMountRoot(mountRoot)

	// Step 2: Break shared-mount propagation so umount(2) does not get EBUSY
	// from propagation peers inherited from the initramfs rootfs.
	if out, err := exec.Command("mount", "--make-rprivate", mountRoot).CombinedOutput(); err != nil {
		log.Warn().Str("mountRoot", mountRoot).Err(err).Str("output", string(out)).
			Msg("mount --make-rprivate failed — umount may still see EBUSY on shared mounts")
	} else {
		log.Info().Str("mountRoot", mountRoot).
			Msg("mount --make-rprivate: shared propagation cut; subtree is now private")
	}

	// Step 3: Clean unmount with retries.
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
			fsSyncMountRoot(mountRoot)
			time.Sleep(time.Second)
		} else {
			log.Warn().Str("mountRoot", mountRoot).Err(err).
				Msg("umount -R failed after all retries — collecting diagnostics then falling back to lazy detach")
			logMountBusyDiagnostics(mountRoot)
			// Lazy detach: removes the mount from the VFS namespace immediately.
			// Do NOT trust the exit code — on some kernel/mount combinations
			// MNT_DETACH succeeds but the syscall wrapper returns EINVAL or EBUSY.
			// Use /proc/mounts as ground truth instead.
			_ = exec.Command("umount", "-l", "-R", mountRoot).Run()
			if hasAnymountUnder(mountRoot) {
				log.Error().Str("mountRoot", mountRoot).
					Msg("lazy umount: mount still visible in /proc/mounts — Finalize remount will likely fail")
			} else {
				log.Info().Str("mountRoot", mountRoot).
					Msg("lazy umount: mount detached from namespace (verified via /proc/mounts)")
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
	startTime := time.Now()

	// Wrap the raw body in a throughput watchdog before any other readers.
	// This cancels the context (killing tar via exec.CommandContext) if the
	// blob stream delivers less than 1KB/s for 5 consecutive seconds.
	// The watchdog is stopped when streamExtract returns regardless of outcome.
	//
	// minBytesPerSec = 1024 (1 KB/s) — even a 1-Gbps LAN degraded to a crawl
	// should sustain this; anything slower is a stall or dead connection.
	// stallTimeout  = 120s — long enough to tolerate XFS metadata stalls during
	// large-directory extraction on LVM thin-provisioned VM disks. The kernel
	// module directory (~4700 files, 199MB) can cause XFS to stall for 30-60s
	// during journal commits on thin-provision volumes, producing zero bytes
	// read from the HTTP body for that window. 30s was too aggressive and fired
	// spuriously on this workload; 120s gives a safe margin while still
	// detecting genuinely dead connections (true network failures stall forever).
	tw, watchCtx := newThroughputWatchdog(ctx, body, 1024, 120*time.Second)
	defer tw.stopWatchdog() // always stop the goroutine on return

	// Set up the reader chain: body(watchdog) → [hasher tee] → progress → decompressor? → tar stdin.
	var hasher hash.Hash
	var reader io.Reader = tw

	if needsVerify {
		hasher = sha256.New()
		reader = io.TeeReader(tw, hasher)
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
		// CloseWithError propagates any decompressor failure to tar's stdin pipe.
		// Using pw2.Close() here would send a clean EOF even when zstd exits
		// non-zero (e.g. truncated HTTP body), causing tar to silently extract a
		// partial archive and exit 0. CloseWithError instead sends an error to
		// the pipe reader so tar receives a read error and exits non-zero.
		go func() {
			if err := decompCmd.Wait(); err != nil {
				log.Error().Err(err).Msg("zstd decompressor exited with error — stream may be truncated")
				pw2.CloseWithError(fmt.Errorf("zstd decompressor failed: %w", err))
			} else {
				pw2.Close()
			}
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
			// Same CloseWithError pattern as zstd: propagate decompressor failure
			// to tar's stdin so a truncated stream causes tar to exit non-zero.
			go func() {
				if err := decompCmd.Wait(); err != nil {
					log.Error().Err(err).Msg("pigz decompressor exited with error — stream may be truncated")
					pw2.CloseWithError(fmt.Errorf("pigz decompressor failed: %w", err))
				} else {
					pw2.Close()
				}
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

	// Tune kernel VM parameters to prevent OOM kill of tar during large extracts.
	// By default, Linux allows dirty page cache to grow to 20% of RAM (dirty_ratio)
	// before blocking writers. On a 4 GB VM with no swap, this means ~800 MB of
	// dirty XFS pages can accumulate before the kernel flushes — sufficient to
	// trigger the OOM killer on the tar process when combined with initramfs
	// overhead. Lowering dirty_ratio to 5% (200 MB) and dirty_background_ratio to
	// 3% (120 MB) forces more frequent writeback, keeping memory pressure low
	// during the entire 1.4 GB streaming extract.
	//
	// These are best-effort: if /proc/sys is not writable (non-initramfs env,
	// permission denied) we log a warning and continue — the deploy will still work
	// on hosts with sufficient RAM or swap, just may OOM on resource-constrained
	// initramfs environments.
	for _, kv := range []struct{ path, value string }{
		{"/proc/sys/vm/dirty_ratio", "5"},
		{"/proc/sys/vm/dirty_background_ratio", "3"},
		{"/proc/sys/vm/dirty_writeback_centisecs", "100"},
	} {
		if err := os.WriteFile(kv.path, []byte(kv.value), 0644); err != nil {
			log.Warn().Str("path", kv.path).Err(err).Msg("vm tuning: could not write sysctl (non-fatal)")
		} else {
			log.Info().Str("path", kv.path).Str("value", kv.value).Msg("vm tuning: sysctl applied")
		}
	}

	// tar -xvf - streams each extracted filename to stdout, which we scan and log
	// at Info level. Stderr is captured separately so we can inspect it for
	// truncation indicators (e.g. "Unexpected EOF") before deciding whether a
	// non-zero exit code is a tolerable warning or a hard failure.
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
	// Use watchCtx (derived from ctx + watchdog cancel) so that tar is killed
	// when either the parent context is cancelled OR the throughput watchdog
	// fires. Both cases are treated as a stream failure and will trigger a retry
	// at the caller level.
	tarCmd := exec.CommandContext(watchCtx, "tar",
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

	// Capture tar stderr so we can detect truncation-specific errors. Stdout (the
	// per-file list) is streamed to the logger as before.
	tarStdoutPipe, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout pipe: %w", err)
	}
	tarStderrPipe, err := tarCmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("tar stderr pipe: %w", err)
	}

	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("tar start: %w", err)
	}

	// Stream tar stdout (filenames) to the logger and tar stderr to a buffer.
	var (
		tarWg         sync.WaitGroup
		tarStderrBuf  strings.Builder
		tarStderrMu   sync.Mutex
	)
	tarWg.Add(2)
	go func() {
		defer tarWg.Done()
		scanner := bufio.NewScanner(tarStdoutPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				log.Info().Str("cmd", "tar").Str("stream", "stdout").Msg(line)
			}
		}
	}()
	go func() {
		defer tarWg.Done()
		scanner := bufio.NewScanner(tarStderrPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				continue
			}
			log.Warn().Str("cmd", "tar").Str("stream", "stderr").Msg(line)
			tarStderrMu.Lock()
			tarStderrBuf.WriteString(line)
			tarStderrBuf.WriteByte('\n')
			tarStderrMu.Unlock()
		}
	}()
	tarWg.Wait()

	tarErr := tarCmd.Wait()
	tarStderr := tarStderrBuf.String()

	if tarErr != nil {
		// Truncation indicators in tar stderr always mean a hard failure — never
		// mask them with the /etc+/usr presence check. A partially extracted archive
		// will be missing /boot (and other tail content) even when /etc and /usr are
		// present, which is exactly the failure mode we are fixing here.
		truncationIndicators := []string{
			"Unexpected EOF in archive",
			"Unexpected EOF on archive file",
			"Error is not recoverable",
			"Cannot read: Is a directory",
			"Archive is incomplete",
		}
		for _, indicator := range truncationIndicators {
			if strings.Contains(tarStderr, indicator) {
				return fmt.Errorf("tar extract failed: stream was truncated (%q detected in tar stderr) — "+
					"the HTTP body was cut short before the archive end; stderr:\n%s", indicator, tarStderr)
			}
		}

		// Non-truncation tar failures (exit code 2, xattr warnings, etc.): tolerate
		// only if /etc and /usr are present AND the byte count matches expectations.
		if _, etcErr := os.Stat(filepath.Join(opts.MountRoot, "etc", "os-release")); etcErr == nil {
			if _, usrErr := os.Stat(filepath.Join(opts.MountRoot, "usr", "bin")); usrErr == nil {
				log.Warn().Err(tarErr).Msg("tar extract reported non-truncation errors but /etc and /usr are present — continuing")
			} else {
				return fmt.Errorf("tar extract failed: %w", tarErr)
			}
		} else {
			return fmt.Errorf("tar extract failed: %w", tarErr)
		}
	}

	// ── Byte-count integrity check ────────────────────────────────────────────
	// pr.written counts bytes read from the HTTP body (compressed). Compare
	// against the Content-Length from the server. A mismatch means the
	// connection was cut before the full blob was received — fail loudly even
	// if tar exited 0 (which can happen when the decompressor swallowed the
	// truncation and closed its stdout pipe cleanly before we fixed the bug).
	if totalBytes > 0 && pr.written != totalBytes {
		return fmt.Errorf("stream truncated: HTTP body delivered %d bytes but Content-Length was %d "+
			"(missing %d bytes) — the deploy is incomplete; re-run to retry",
			pr.written, totalBytes, totalBytes-pr.written)
	}

	elapsed := time.Since(startTime)
	var throughputMBps float64
	if elapsed.Seconds() > 0 {
		throughputMBps = float64(pr.written) / elapsed.Seconds() / (1 << 20)
	}
	log.Info().
		Str("bytes_received", humanReadableBytes(pr.written)).
		Str("content_length", humanReadableBytes(totalBytes)).
		Str("elapsed", elapsed.Round(time.Millisecond).String()).
		Str("throughput", fmt.Sprintf("%.2f MB/s", throughputMBps)).
		Msg("stream-extract complete")

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

// throughputWatchdogReader wraps an io.Reader and cancels the provided context
// if throughput drops below minBytesPerSec for longer than stallTimeout.
// This lets us detect a stalled TCP connection (server paused, network
// blip, kernel socket timeout) without killing a healthy slow-but-steady read.
//
// The watchdog goroutine samples bytesRead every second. If the delta is below
// minBytesPerSec for stallTimeout consecutive seconds, it cancels the context
// and the next Read() call on the underlying reader will fail, propagating the
// cancel up through io.Copy → tar stdin → tar exit → streamExtract error return.
//
// The watchdog goroutine is stopped when stopWatchdog is called or when the
// context is already cancelled. Call stopWatchdog() in a defer after io.Copy
// to prevent a goroutine leak.
type throughputWatchdogReader struct {
	r             io.Reader
	bytesRead     atomic.Int64
	stopWatchdog  context.CancelFunc
}

// newThroughputWatchdog wraps r with a stall watchdog. minBytesPerSec is the
// minimum sustained throughput required; stallTimeout is how long below that
// threshold triggers a cancel. Typical values: 1024 bytes/s, 5s.
func newThroughputWatchdog(ctx context.Context, r io.Reader, minBytesPerSec int64, stallTimeout time.Duration) (*throughputWatchdogReader, context.Context) {
	watchCtx, cancelWatch := context.WithCancel(ctx)
	tw := &throughputWatchdogReader{r: r, stopWatchdog: cancelWatch}

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		var stallSecs int
		var lastBytes int64
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				cur := tw.bytesRead.Load()
				delta := cur - lastBytes
				lastBytes = cur
				if delta < minBytesPerSec {
					stallSecs++
					if stallSecs >= int(stallTimeout.Seconds()) {
						logger().Warn().
							Int64("bytes_per_sec", delta).
							Int64("min_bytes_per_sec", minBytesPerSec).
							Int("stall_secs", stallSecs).
							Msg("throughput watchdog: blob stream stalled — cancelling download context")
						cancelWatch()
						return
					}
				} else {
					stallSecs = 0
				}
			}
		}
	}()

	return tw, watchCtx
}

func (tw *throughputWatchdogReader) Read(b []byte) (int, error) {
	n, err := tw.r.Read(b)
	tw.bytesRead.Add(int64(n))
	return n, err
}
