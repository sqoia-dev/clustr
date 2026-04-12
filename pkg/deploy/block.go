package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// BlockDeployer deploys a raw block image directly to a disk.
// It supports two modes:
//   - streaming: pipes the HTTP download directly into dd (no temp file needed)
//   - verified: downloads to a temp file first to compute sha256, then writes
//
// Streaming is used when SkipVerify is true. Verified mode is the default when
// ExpectedChecksum is provided, to avoid writing corrupt data to disk.
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

	// Validate disk size and produce an actionable error message.
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

// Deploy streams the block image from opts.ImageURL and writes it to the target
// disk. When opts.ExpectedChecksum is set and opts.SkipVerify is false, the blob
// is downloaded to a temp file first for checksum verification before writing.
func (d *BlockDeployer) Deploy(ctx context.Context, opts DeployOpts, progress ProgressFunc) error {
	disk := opts.TargetDisk
	if disk == "" {
		disk = d.targetDisk
	}
	if disk == "" {
		return fmt.Errorf("deploy/block: Preflight must be called before Deploy")
	}

	// ── Rollback setup ────────────────────────────────────────────────────────
	log := logger()
	var rollbackPath string
	if !opts.NoRollback {
		backup, empty, err := backupPartitionTable(disk)
		if err != nil {
			log.Warn().Str("disk", disk).Err(err).Msg("could not back up partition table — proceeding without rollback")
		} else if empty {
			log.Info().Str("disk", disk).Msg("disk has no existing partition table — no rollback possible if deployment fails")
		} else {
			rollbackPath = backup
			log.Info().Str("backup", rollbackPath).Msg("partition table backup saved (will restore on failure)")
		}
	}

	doRollback := func(reason string) {
		if rollbackPath == "" {
			return
		}
		log.Warn().Str("reason", reason).Str("disk", disk).Msg("ROLLBACK triggered — restoring partition table")
		if err := restorePartitionTable(disk, rollbackPath); err != nil {
			log.Error().Err(err).Str("disk", disk).Msg("ROLLBACK FAILED — disk may be in inconsistent state; re-run deployment to recover")
		} else {
			log.Info().Str("disk", disk).Msg("rollback complete — partition table restored to pre-deployment state")
			rollbackPath = ""
		}
	}

	if progress != nil {
		progress(0, 0, "downloading")
	}
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("downloading", 0) // total updated once content-length is known
	}

	var writeErr error
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
				doRollback("context cancelled during retry")
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		writeErr = d.attemptBlockWrite(ctx, disk, opts, progress)
		if writeErr == nil {
			break
		}
		if ctx.Err() != nil {
			doRollback("context cancelled during download")
			return ctx.Err()
		}
		log.Warn().Int("attempt", attempt).Int("max", maxDownloadAttempts).Err(writeErr).Msg("block write attempt failed")
	}

	if writeErr != nil {
		doRollback("block write failed after all retries")
		if opts.Reporter != nil {
			opts.Reporter.EndPhase(writeErr.Error())
		}
		return fmt.Errorf("deploy/block: image write failed after %d attempts: %w", maxDownloadAttempts, writeErr)
	}
	if opts.Reporter != nil {
		opts.Reporter.EndPhase("")
	}

	// Deployment succeeded — remove the rollback backup.
	if rollbackPath != "" {
		os.Remove(rollbackPath)
		log.Info().Msg("deployment succeeded — partition table backup removed")
	}

	// Re-read the partition table after writing.
	_ = runAndLog(ctx, "partprobe", exec.CommandContext(ctx, "partprobe", disk))
	_ = runCmd(ctx, "udevadm", "settle")

	return nil
}

// attemptBlockWrite performs a single attempt at downloading and writing the block image.
func (d *BlockDeployer) attemptBlockWrite(ctx context.Context, disk string, opts DeployOpts, progress ProgressFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.ImageURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
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
	needsVerify := !opts.SkipVerify && opts.ExpectedChecksum != ""

	if needsVerify {
		return d.downloadVerifyAndWrite(ctx, resp.Body, totalBytes, disk, opts, progress)
	}

	if opts.SkipVerify && opts.ExpectedChecksum != "" {
		logger().Warn().Msg("checksum verification skipped (--skip-verify set)")
	}
	return d.streamBlockWrite(ctx, resp.Body, totalBytes, disk, opts, progress)
}

// downloadVerifyAndWrite downloads the block image to a temp file, verifies
// its checksum, then writes the temp file to disk.
func (d *BlockDeployer) downloadVerifyAndWrite(ctx context.Context, body io.Reader, totalBytes int64, disk string, opts DeployOpts, progress ProgressFunc) error {
	tmpFile, err := os.CreateTemp("", "clonr-block-*.img")
	if err != nil {
		return fmt.Errorf("create temp file for checksum verification: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	// Download and hash simultaneously.
	hasher := sha256.New()
	tee := io.TeeReader(body, hasher)
	// Update downloading phase total now that we know the content-length.
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("downloading", totalBytes)
	}
	pr := &progressReader{r: tee, total: totalBytes, fn: progress, phase: "downloading", reporter: opts.Reporter}

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
	logger().Info().Str("sha256", gotChecksum).Msg("image checksum verified")

	// Seek to start for writing.
	if _, err := tmpFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek temp file for block write: %w", err)
	}

	if progress != nil {
		progress(0, totalBytes, "writing")
	}

	// Open the target disk for writing.
	f, err := os.OpenFile(disk, os.O_WRONLY|os.O_SYNC, 0o600)
	if err != nil {
		return fmt.Errorf("open disk %s: %w", disk, err)
	}
	defer f.Close()

	buf := make([]byte, 4*1024*1024)
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("extracting", totalBytes)
	}
	pr2 := &progressReader{r: tmpFile, total: totalBytes, fn: progress, phase: "writing", reporter: opts.Reporter}
	if _, err := io.CopyBuffer(f, pr2, buf); err != nil {
		return fmt.Errorf("write to %s: %w", disk, err)
	}

	return f.Sync()
}

// streamBlockWrite streams the download directly to disk without checksum verification.
func (d *BlockDeployer) streamBlockWrite(ctx context.Context, body io.Reader, totalBytes int64, disk string, opts DeployOpts, progress ProgressFunc) error {
	f, err := os.OpenFile(disk, os.O_WRONLY|os.O_SYNC, 0o600)
	if err != nil {
		return fmt.Errorf("open disk %s: %w", disk, err)
	}
	defer f.Close()

	// Update downloading phase total now that we know the content-length.
	if opts.Reporter != nil {
		opts.Reporter.StartPhase("downloading", totalBytes)
	}
	pr := &progressReader{r: body, total: totalBytes, fn: progress, phase: "writing", reporter: opts.Reporter}
	buf := make([]byte, 4*1024*1024)
	if _, err := io.CopyBuffer(f, pr, buf); err != nil {
		return fmt.Errorf("write to %s: %w", disk, err)
	}

	return f.Sync()
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

// verifyBlockSpotCheck does a basic integrity check on the deployed block image
// by verifying presence of key files in the mounted filesystem.
func verifyBlockSpotCheck(mountRoot string) error {
	criticalPaths := []string{
		"/etc/hostname",
		"/etc/fstab",
		"/sbin/init",
	}
	var missing []string
	for _, p := range criticalPaths {
		if _, err := os.Stat(mountRoot + p); os.IsNotExist(err) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("deployed block image is missing critical files: %v — "+
			"the image may be corrupt or the deployment was incomplete", missing)
	}
	return nil
}
