// Package image provides the factory pipeline for building BaseImages from
// raw sources: URLs (qcow2/img/tar.gz/raw), ISOs, and live nodes via rsync.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// CaptureRequest describes a live-node rsync capture operation.
type CaptureRequest struct {
	// Source is the rsync source spec: "user@host:/" or a local path.
	Source  string
	Name    string
	Version string
	OS      string
	Arch    string
	Tags    []string
	Notes   string
}

// Factory turns raw inputs into finalized BaseImages stored under ImageDir.
type Factory struct {
	Store    *db.DB
	ImageDir string
	Logger   zerolog.Logger
}

// PullImage downloads a cloud image from a URL, extracts/mounts it, copies
// the root filesystem into ImageDir/<id>/rootfs/, computes a sha256 checksum
// over the rootfs tree, and finalizes the BaseImage record as "ready".
//
// Supported formats (detected by URL extension):
//
//	.qcow2              — converted to raw then mounted via loop device
//	.img / .raw         — mounted via loop device (kpartx for partition tables)
//	.tar.gz / .tgz      — extracted directly
func (f *Factory) PullImage(ctx context.Context, req api.PullRequest) (*api.BaseImage, error) {
	id := uuid.New().String()

	if req.Format == "" {
		req.Format = api.ImageFormatFilesystem
	}
	if req.Tags == nil {
		req.Tags = []string{}
	}

	img := api.BaseImage{
		ID:         id,
		Name:       req.Name,
		Version:    req.Version,
		OS:         req.OS,
		Arch:       req.Arch,
		Status:     api.ImageStatusBuilding,
		Format:     req.Format,
		DiskLayout: req.DiskLayout,
		Tags:       req.Tags,
		SourceURL:  req.URL,
		Notes:      req.Notes,
		CreatedAt:  time.Now().UTC(),
	}

	f.Logger.Info().Str("image_id", id).Str("url", req.URL).Msg("factory: pull image started")

	if err := f.Store.CreateBaseImage(ctx, img); err != nil {
		return nil, fmt.Errorf("factory: create image record: %w", err)
	}

	// All remaining work is async so the caller gets an immediate response.
	go f.pullAsync(id, req.URL)

	return &img, nil
}

func (f *Factory) pullAsync(imageID, url string) {
	ctx := context.Background()

	rootfs, size, checksum, err := f.pullAndExtract(ctx, imageID, url)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: pull failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.SetBlobPath(ctx, imageID, rootfs); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: set blob path")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.FinalizeBaseImage(ctx, imageID, size, checksum); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: finalize failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	f.Logger.Info().Str("image_id", imageID).Int64("size_bytes", size).
		Str("checksum", checksum).Msg("factory: pull complete")
}

// pullAndExtract downloads url, detects its format, extracts the root
// filesystem into <ImageDir>/<imageID>/rootfs/, and returns the rootfs path,
// the total byte-count of the rootfs tree, and a sha256 of that tree.
func (f *Factory) pullAndExtract(ctx context.Context, imageID, url string) (rootfsPath string, sizeBytes int64, checksum string, err error) {
	imageRoot := filepath.Join(f.ImageDir, imageID)
	rootfsPath = filepath.Join(imageRoot, "rootfs")
	if err = os.MkdirAll(rootfsPath, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("create rootfs dir: %w", err)
	}

	// Download to a temp file.
	tmpFile, err := os.CreateTemp("", "clonr-pull-*"+urlExt(url))
	if err != nil {
		return "", 0, "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	f.Logger.Info().Str("image_id", imageID).Str("url", url).Msg("factory: downloading blob")
	if err = downloadURL(ctx, url, tmpFile); err != nil {
		tmpFile.Close()
		return "", 0, "", fmt.Errorf("download %s: %w", url, err)
	}
	tmpFile.Close()

	ext := strings.ToLower(urlExt(url))
	f.Logger.Info().Str("image_id", imageID).Str("ext", ext).Msg("factory: extracting image")

	switch {
	case ext == ".qcow2":
		err = f.extractQcow2(ctx, imageID, tmpFile.Name(), rootfsPath)
	case ext == ".img" || ext == ".raw":
		err = f.extractRaw(ctx, imageID, tmpFile.Name(), rootfsPath)
	case ext == ".tar.gz" || ext == ".tgz" || ext == ".tar":
		err = extractTar(tmpFile.Name(), rootfsPath)
	default:
		// Try treating unknown extensions as raw block images.
		err = f.extractRaw(ctx, imageID, tmpFile.Name(), rootfsPath)
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("extract image (%s): %w", ext, err)
	}

	sizeBytes, checksum, err = checksumDir(rootfsPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("checksum rootfs: %w", err)
	}

	return rootfsPath, sizeBytes, checksum, nil
}

// extractQcow2 converts a qcow2 image to raw then delegates to extractRaw.
func (f *Factory) extractQcow2(ctx context.Context, imageID, srcPath, rootfsPath string) error {
	rawPath := srcPath + ".raw"
	defer os.Remove(rawPath)

	f.Logger.Info().Str("image_id", imageID).Msg("factory: converting qcow2 to raw")
	cmd := exec.CommandContext(ctx, "qemu-img", "convert", "-f", "qcow2", "-O", "raw", srcPath, rawPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("qemu-img convert: %w\noutput: %s", err, string(out))
	}
	return f.extractRaw(ctx, imageID, rawPath, rootfsPath)
}

// extractRaw mounts a raw disk image (via losetup + kpartx if partitioned)
// and rsyncs the first data partition's filesystem into rootfsPath.
// If mounting fails (e.g. not running as root or no loop support), it falls
// back to treating the file as a flat tar archive.
func (f *Factory) extractRaw(ctx context.Context, imageID, rawPath, rootfsPath string) error {
	// Set up loop device.
	loopOut, err := exec.CommandContext(ctx, "losetup", "--find", "--partscan", "--show", rawPath).CombinedOutput()
	if err != nil {
		// Fallback: try treating as tar.
		f.Logger.Warn().Str("image_id", imageID).
			Msgf("factory: losetup failed (%s), attempting tar fallback", strings.TrimSpace(string(loopOut)))
		return extractTar(rawPath, rootfsPath)
	}
	loopDev := strings.TrimSpace(string(loopOut))
	defer func() {
		_ = exec.Command("losetup", "-d", loopDev).Run()
	}()

	// Give the kernel a moment to populate partition devices.
	_ = exec.Command("udevadm", "settle", "--timeout=5").Run()

	// Find the first partition.
	mountDev := loopDev
	partOut, err := exec.CommandContext(ctx, "lsblk", "-lno", "NAME", loopDev).CombinedOutput()
	if err == nil {
		parts := strings.Split(strings.TrimSpace(string(partOut)), "\n")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" || p == filepath.Base(loopDev) {
				continue
			}
			// Use the first partition that is not the loop device itself.
			candidate := "/dev/" + p
			if _, statErr := os.Stat(candidate); statErr == nil {
				mountDev = candidate
				break
			}
		}
	}

	// Mount the partition.
	mnt, err := os.MkdirTemp("", "clonr-mount-*")
	if err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer os.RemoveAll(mnt)

	f.Logger.Info().Str("image_id", imageID).Str("dev", mountDev).Str("mnt", mnt).Msg("factory: mounting partition")
	if out, err := exec.CommandContext(ctx, "mount", "-o", "ro", mountDev, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount %s: %w\noutput: %s", mountDev, err, string(out))
	}
	defer func() {
		_ = exec.Command("umount", "-l", mnt).Run()
	}()

	// rsync into rootfs.
	return rsyncDir(ctx, mnt+"/", rootfsPath)
}

// ImportISO mounts an ISO, extracts the root filesystem (handling Rocky/RHEL
// LiveOS squashfs if present), copies it to ImageDir/<id>/rootfs/, and
// finalizes the BaseImage record.
func (f *Factory) ImportISO(ctx context.Context, isoPath, name, version string) (*api.BaseImage, error) {
	if _, err := os.Stat(isoPath); err != nil {
		return nil, fmt.Errorf("factory: iso not found: %w", err)
	}

	id := uuid.New().String()
	img := api.BaseImage{
		ID:        id,
		Name:      name,
		Version:   version,
		Status:    api.ImageStatusBuilding,
		Format:    api.ImageFormatFilesystem,
		Tags:      []string{},
		CreatedAt: time.Now().UTC(),
		DiskLayout: api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "esp", SizeBytes: 512 * 1024 * 1024, Filesystem: "vfat", MountPoint: "/boot/efi", Flags: []string{"boot", "esp"}},
				{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
			},
			Bootloader: api.Bootloader{Type: "grub2", Target: "x86_64-efi"},
		},
	}

	f.Logger.Info().Str("image_id", id).Str("iso", isoPath).Msg("factory: import ISO started")

	if err := f.Store.CreateBaseImage(ctx, img); err != nil {
		return nil, fmt.Errorf("factory: create image record: %w", err)
	}

	go f.importISOAsync(id, isoPath)

	return &img, nil
}

func (f *Factory) importISOAsync(imageID, isoPath string) {
	ctx := context.Background()

	rootfs, size, checksum, err := f.extractISO(ctx, imageID, isoPath)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: import ISO failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.SetBlobPath(ctx, imageID, rootfs); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: set blob path")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.FinalizeBaseImage(ctx, imageID, size, checksum); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: finalize ISO failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	f.Logger.Info().Str("image_id", imageID).Int64("size_bytes", size).Msg("factory: import ISO complete")
}

func (f *Factory) extractISO(ctx context.Context, imageID, isoPath string) (rootfsPath string, sizeBytes int64, checksum string, err error) {
	imageRoot := filepath.Join(f.ImageDir, imageID)
	rootfsPath = filepath.Join(imageRoot, "rootfs")
	if err = os.MkdirAll(rootfsPath, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("create rootfs dir: %w", err)
	}

	// Mount the ISO.
	isomnt, err := os.MkdirTemp("", "clonr-iso-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("create iso mount: %w", err)
	}
	defer os.RemoveAll(isomnt)

	f.Logger.Info().Str("image_id", imageID).Str("iso", isoPath).Msg("factory: mounting ISO")
	if out, err := exec.CommandContext(ctx, "mount", "-o", "ro,loop", isoPath, isomnt).CombinedOutput(); err != nil {
		return "", 0, "", fmt.Errorf("mount iso: %w\noutput: %s", err, string(out))
	}
	defer func() { _ = exec.Command("umount", "-l", isomnt).Run() }()

	// Rocky/RHEL LiveOS layout: LiveOS/squashfs.img contains a rootfs.img.
	squashPath := filepath.Join(isomnt, "LiveOS", "squashfs.img")
	if _, statErr := os.Stat(squashPath); statErr == nil {
		f.Logger.Info().Str("image_id", imageID).Msg("factory: detected LiveOS squashfs layout")
		err = f.extractLiveOS(ctx, imageID, squashPath, rootfsPath)
	} else {
		// Plain ISO: rsync contents directly.
		err = rsyncDir(ctx, isomnt+"/", rootfsPath)
	}
	if err != nil {
		return "", 0, "", err
	}

	sizeBytes, checksum, err = checksumDir(rootfsPath)
	return rootfsPath, sizeBytes, checksum, err
}

// extractLiveOS handles the Rocky/RHEL LiveOS format:
// squashfs.img → unsquash → rootfs.img → mount → rsync.
func (f *Factory) extractLiveOS(ctx context.Context, imageID, squashPath, rootfsPath string) error {
	squashMnt, err := os.MkdirTemp("", "clonr-squash-*")
	if err != nil {
		return fmt.Errorf("create squashfs mount: %w", err)
	}
	defer os.RemoveAll(squashMnt)

	f.Logger.Info().Str("image_id", imageID).Msg("factory: mounting squashfs")
	if out, err := exec.CommandContext(ctx, "mount", "-o", "ro,loop", squashPath, squashMnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount squashfs: %w\noutput: %s", err, string(out))
	}
	defer func() { _ = exec.Command("umount", "-l", squashMnt).Run() }()

	rootfsImg := filepath.Join(squashMnt, "LiveOS", "rootfs.img")
	if _, err := os.Stat(rootfsImg); err != nil {
		// Fall back to rsyncing the squashfs mount directly.
		f.Logger.Warn().Str("image_id", imageID).Msg("factory: no rootfs.img, rsyncing squashfs contents")
		return rsyncDir(ctx, squashMnt+"/", rootfsPath)
	}

	rootfsMnt, err := os.MkdirTemp("", "clonr-rootfs-*")
	if err != nil {
		return fmt.Errorf("create rootfs mount: %w", err)
	}
	defer os.RemoveAll(rootfsMnt)

	f.Logger.Info().Str("image_id", imageID).Msg("factory: mounting rootfs.img")
	if out, err := exec.CommandContext(ctx, "mount", "-o", "ro,loop", rootfsImg, rootfsMnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs.img: %w\noutput: %s", err, string(out))
	}
	defer func() { _ = exec.Command("umount", "-l", rootfsMnt).Run() }()

	return rsyncDir(ctx, rootfsMnt+"/", rootfsPath)
}

// CaptureNode rsyncs a running node's filesystem into ImageDir/<id>/rootfs/,
// scrubs node-specific identity data, and finalizes the record.
func (f *Factory) CaptureNode(ctx context.Context, req CaptureRequest) (*api.BaseImage, error) {
	if req.Source == "" {
		return nil, fmt.Errorf("factory: capture source is required")
	}

	id := uuid.New().String()
	if req.Tags == nil {
		req.Tags = []string{}
	}

	img := api.BaseImage{
		ID:        id,
		Name:      req.Name,
		Version:   req.Version,
		OS:        req.OS,
		Arch:      req.Arch,
		Status:    api.ImageStatusBuilding,
		Format:    api.ImageFormatFilesystem,
		Tags:      req.Tags,
		Notes:     req.Notes,
		CreatedAt: time.Now().UTC(),
	}

	f.Logger.Info().Str("image_id", id).Str("source", req.Source).Msg("factory: capture node started")

	if err := f.Store.CreateBaseImage(ctx, img); err != nil {
		return nil, fmt.Errorf("factory: create image record: %w", err)
	}

	go f.captureAsync(id, req)

	return &img, nil
}

func (f *Factory) captureAsync(imageID string, req CaptureRequest) {
	ctx := context.Background()

	rootfs := filepath.Join(f.ImageDir, imageID, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: create rootfs dir")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	f.Logger.Info().Str("image_id", imageID).Str("source", req.Source).Msg("factory: rsyncing node")

	excludes := []string{
		"--exclude=/proc/*",
		"--exclude=/sys/*",
		"--exclude=/dev/*",
		"--exclude=/run/*",
		"--exclude=/tmp/*",
		"--exclude=/var/tmp/*",
		"--exclude=/var/cache/*",
	}
	rsyncArgs := append([]string{"-aAXH", "--numeric-ids", "--delete"}, excludes...)
	rsyncArgs = append(rsyncArgs, req.Source, rootfs+"/")

	cmd := exec.CommandContext(ctx, "rsync", rsyncArgs...)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := fmt.Sprintf("rsync failed: %v\noutput: %s", err, string(out))
		f.Logger.Error().Str("image_id", imageID).Msg(msg)
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, msg)
		return
	}

	f.Logger.Info().Str("image_id", imageID).Msg("factory: scrubbing node identity")
	if err := ScrubNodeIdentity(rootfs); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
	}

	size, checksum, err := checksumDir(rootfs)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: checksum failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.SetBlobPath(ctx, imageID, rootfs); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: set blob path")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := f.Store.FinalizeBaseImage(ctx, imageID, size, checksum); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: finalize failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	f.Logger.Info().Str("image_id", imageID).Int64("size_bytes", size).Msg("factory: capture complete")
}

// ─── internal helpers ────────────────────────────────────────────────────────

// privateIPNets lists RFC-1918, link-local, and loopback CIDR ranges that
// should not be reachable via pull URLs to prevent SSRF attacks.
var privateIPNets []*net.IPNet

func init() {
	for _, cidr := range []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16", // link-local
		"127.0.0.0/8",    // loopback
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	} {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil {
			privateIPNets = append(privateIPNets, ipnet)
		}
	}
}

// validatePullURL checks that the URL is safe to fetch: only http/https schemes
// and no private/loopback IP addresses (SSRF prevention). Set the environment
// variable CLONR_ALLOW_PRIVATE_URLS=true to bypass this check in lab environments.
func validatePullURL(rawURL string) error {
	if os.Getenv("CLONR_ALLOW_PRIVATE_URLS") == "true" {
		return nil
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("pull URL must use http or https scheme (got %q)", u.Scheme)
	}

	host := u.Hostname()
	ips, err := net.LookupHost(host)
	if err != nil {
		// If DNS resolution fails, reject the URL to be safe.
		return fmt.Errorf("cannot resolve host %q: %w", host, err)
	}

	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		for _, block := range privateIPNets {
			if block.Contains(ip) {
				return fmt.Errorf("pull URL resolves to private/internal IP %s — "+
					"set CLONR_ALLOW_PRIVATE_URLS=true to allow this in lab environments", ipStr)
			}
		}
	}
	return nil
}

// downloadURL streams url into dst using a context-aware GET request.
func downloadURL(ctx context.Context, rawURL string, dst *os.File) error {
	if err := validatePullURL(rawURL); err != nil {
		return fmt.Errorf("URL validation: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// extractTar extracts a .tar.gz / .tgz / .tar into dst using the system tar.
func extractTar(srcPath, dstPath string) error {
	cmd := exec.Command("tar", "-xf", srcPath, "-C", dstPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w\noutput: %s", err, string(out))
	}
	return nil
}

// rsyncDir rsyncs src into dst, preserving all attributes.
// --copy-unsafe-links causes symlinks that point outside the source tree to be
// copied as regular files rather than preserved as symlinks, preventing
// path traversal attacks via malicious symlinks in untrusted ISO content.
func rsyncDir(ctx context.Context, src, dst string) error {
	cmd := exec.CommandContext(ctx, "rsync", "-aAXH", "--numeric-ids", "--copy-unsafe-links", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync: %w\noutput: %s", err, string(out))
	}
	return nil
}

// checksumDir walks the rootfs directory, accumulates the total size, and
// returns a deterministic sha256 hash over the tree.
//
// Determinism is achieved by:
//  1. Collecting all entries during the walk (filepath.Walk order is not canonical).
//  2. Sorting paths lexicographically.
//  3. For each entry in sorted order, hashing: the relative path (as bytes),
//     the file size as a little-endian int64, and for regular files the full
//     file content. Symlink targets are hashed as their target path string
//     (symlinks are not followed).
//
// This produces the same checksum regardless of filesystem ordering, so the
// same rootfs content always yields the same hash even after a repack.
func checksumDir(root string) (sizeBytes int64, checksum string, err error) {
	type entry struct {
		relPath string
		info    os.FileInfo
		absPath string
	}

	var entries []entry

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // skip the root itself
		}
		entries = append(entries, entry{relPath: rel, info: info, absPath: path})
		return nil
	})
	if walkErr != nil {
		return 0, "", fmt.Errorf("walk rootfs: %w", walkErr)
	}

	// Sort lexicographically by relative path for a canonical ordering.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	h := sha256.New()
	sizeBuf := make([]byte, 8)

	for _, e := range entries {
		// Hash the relative path.
		_, _ = h.Write([]byte(e.relPath))

		mode := e.info.Mode()

		switch {
		case mode.IsRegular():
			sizeBytes += e.info.Size()
			// Hash the file size (little-endian int64) then the file content.
			binary.LittleEndian.PutUint64(sizeBuf, uint64(e.info.Size()))
			_, _ = h.Write(sizeBuf)

			f, err := os.Open(e.absPath)
			if err != nil {
				return 0, "", fmt.Errorf("open %s: %w", e.relPath, err)
			}
			if _, err := io.Copy(h, f); err != nil {
				f.Close()
				return 0, "", fmt.Errorf("read %s: %w", e.relPath, err)
			}
			f.Close()

		case mode&os.ModeSymlink != 0:
			// Hash the symlink target path — do not follow the link.
			target, err := os.Readlink(e.absPath)
			if err != nil {
				return 0, "", fmt.Errorf("readlink %s: %w", e.relPath, err)
			}
			_, _ = h.Write([]byte(target))
		}
		// Directories and other special files contribute only their path to the hash.
	}

	return sizeBytes, hex.EncodeToString(h.Sum(nil)), nil
}

// urlExt returns the file extension from a URL path, lowercased, handling
// compound extensions like ".tar.gz".
func urlExt(rawURL string) string {
	// Strip query string.
	if idx := strings.IndexByte(rawURL, '?'); idx != -1 {
		rawURL = rawURL[:idx]
	}
	base := filepath.Base(rawURL)
	if strings.HasSuffix(strings.ToLower(base), ".tar.gz") {
		return ".tar.gz"
	}
	if strings.HasSuffix(strings.ToLower(base), ".tar.bz2") {
		return ".tar.bz2"
	}
	return filepath.Ext(base)
}

