// Package image provides the factory pipeline for building BaseImages from
// raw sources: URLs (qcow2/img/tar.gz/raw), ISOs, and live nodes via rsync.
package image

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/internal/image/isoinstaller"
	"github.com/sqoia-dev/clustr/internal/image/isoinstaller/comps"
	"github.com/sqoia-dev/clustr/pkg/api"
)

// CaptureRequest describes a live-node SSH+rsync capture operation.
type CaptureRequest struct {
	// SourceHost is the SSH-reachable hostname or IP of the node to capture.
	SourceHost   string
	SSHUser      string
	SSHPassword  string // applied at capture time, not stored
	SSHKeyPath   string
	SSHPort      int      // defaults to 22 when zero
	Name         string
	Version      string
	OS           string
	Arch         string
	Tags         []string
	Notes        string
	ExcludePaths []string // rsync --exclude patterns
}

// BuildProgressReporter is the interface Factory uses to report ISO build progress.
// Implemented by *server.BuildProgressStore via *server.BuildHandle; when nil,
// progress reporting is silently skipped (non-ISO builds).
type BuildProgressReporter interface {
	// Start registers a new build and returns a handle for the factory to push
	// phase/progress/serial-line events.
	Start(imageID string) BuildHandle
}

// BuildHandle is the per-build progress handle returned by BuildProgressReporter.Start.
type BuildHandle interface {
	SetPhase(phase string)
	SetProgress(done, total int64)
	AddSerialLine(line string)
	AddStderrLine(line string)
	Fail(msg string)
	Complete()
}

// noopBuildHandle is used when no BuildProgressReporter is wired in.
type noopBuildHandle struct{}

func (noopBuildHandle) SetPhase(_ string)          {}
func (noopBuildHandle) SetProgress(_, _ int64)     {}
func (noopBuildHandle) AddSerialLine(_ string)      {}
func (noopBuildHandle) AddStderrLine(_ string)      {}
func (noopBuildHandle) Fail(_ string)               {}
func (noopBuildHandle) Complete()                   {}

// defaultMaxConcurrentBuilds is used when CLUSTR_MAX_CONCURRENT_BUILDS is unset.
const defaultMaxConcurrentBuilds = 4

// Factory turns raw inputs into finalized BaseImages stored under ImageDir.
type Factory struct {
	Store         *db.DB
	ImageDir      string
	Logger        zerolog.Logger
	// BuildProgress, when non-nil, receives phase/progress/serial events during
	// ISO builds so the admin can monitor the VM installer in real time.
	// Wire in *server.BuildProgressStore via an adapter (see server.go).
	BuildProgress BuildProgressReporter
	// ISOCacheDir is the directory where downloaded ISOs are cached keyed by
	// sha256(url). Defaults to /var/lib/clustr/iso-cache if empty.
	ISOCacheDir string

	// buildSem limits the number of concurrent async builds (pull, importISO,
	// capture, buildFromISO). Capacity is set from CLUSTR_MAX_CONCURRENT_BUILDS
	// (default 4). Initialized by NewFactory or SetContext.
	buildSem chan struct{}
	// ctx is the server-lifetime context. Async methods return early when it is
	// cancelled (e.g. on graceful shutdown). Set via SetContext.
	ctx context.Context
}

func defaultIfEmpty(val, fallback string) string {
	if val == "" {
		return fallback
	}
	return val
}

// NewFactory constructs a Factory with a bounded build semaphore.
// Capacity is read from CLUSTR_MAX_CONCURRENT_BUILDS (default 4).
func NewFactory(store *db.DB, imageDir string, logger zerolog.Logger, progress BuildProgressReporter, isoCacheDir string) *Factory {
	cap_ := defaultMaxConcurrentBuilds
	if v := os.Getenv("CLUSTR_MAX_CONCURRENT_BUILDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cap_ = n
		}
	}
	return &Factory{
		Store:         store,
		ImageDir:      imageDir,
		Logger:        logger,
		BuildProgress: progress,
		ISOCacheDir:   isoCacheDir,
		buildSem:      make(chan struct{}, cap_),
		ctx:           context.Background(),
	}
}

// SetContext wires the server-lifetime context into the Factory so that async
// build goroutines can be cancelled on shutdown.  Call this from
// Server.StartBackgroundWorkers before accepting traffic.
func (f *Factory) SetContext(ctx context.Context) {
	f.ctx = ctx
}

// acquireSem tries to acquire a slot on the build semaphore.  Returns false
// when the factory context is cancelled before a slot becomes available.
func (f *Factory) acquireSem() bool {
	sem := f.buildSem
	if sem == nil {
		// Factory was constructed via struct literal without NewFactory — no
		// concurrency limit, but still respect context cancellation.
		return true
	}
	select {
	case sem <- struct{}{}:
		return true
	case <-f.ctx.Done():
		return false
	}
}

// releaseSem releases one slot back to the build semaphore.
func (f *Factory) releaseSem() {
	if f.buildSem != nil {
		<-f.buildSem
	}
}

const defaultISOCacheDir = "/var/lib/clustr/iso-cache"

// isoCachePath returns the deterministic cache path and partial path for the
// given URL. The directory is created (0o755) if it does not already exist.
func (f *Factory) isoCachePath(rawURL string) (cachePath, partialPath string, err error) {
	cacheDir := f.ISOCacheDir
	if cacheDir == "" {
		cacheDir = defaultISOCacheDir
	}
	if err = os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create iso cache dir: %w", err)
	}
	h := sha256.Sum256([]byte(rawURL))
	key := hex.EncodeToString(h[:])
	cachePath = filepath.Join(cacheDir, key+".iso")
	partialPath = cachePath + ".partial"
	return cachePath, partialPath, nil
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
	if !f.acquireSem() {
		f.Logger.Warn().Str("image_id", imageID).Msg("factory: pullAsync cancelled — server shutting down")
		_ = f.Store.UpdateBaseImageStatus(f.ctx, imageID, api.ImageStatusError, "server shutting down")
		return
	}
	defer f.releaseSem()

	ctx := f.ctx

	// Acquire a progress handle if a reporter is wired in. pullAsync calls
	// pullAndExtract which may route to buildFromISOFile (for .iso URLs); both
	// paths emit progress events through this handle.
	var ph BuildHandle
	if f.BuildProgress != nil {
		ph = f.BuildProgress.Start(imageID)
	} else {
		ph = noopBuildHandle{}
	}

	rootfs, _, _, err := f.pullAndExtract(ctx, imageID, url, ph)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: pull failed")
		ph.Fail(err.Error())
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	_ = f.finalizeImageFromRootfs(ctx, imageID, rootfs, finalizeSourceMetadata{
		BuildMethod: "pull",
		Scrub:       false,
		Progress:    ph,
	})
}

// pullAndExtract downloads url, detects its format, extracts the root
// filesystem into <ImageDir>/<imageID>/rootfs/, and returns the rootfs path,
// the total byte-count of the rootfs tree, and a sha256 of that tree.
//
// ph receives phase and progress events for the download and any subsequent
// extraction. For .iso URLs the ISO build pipeline is used and ph is wired
// into OnPhase/OnSerialLine/OnStderrLine callbacks in the QEMU VM.
func (f *Factory) pullAndExtract(ctx context.Context, imageID, url string, ph BuildHandle) (rootfsPath string, sizeBytes int64, checksum string, err error) {
	imageRoot := filepath.Join(f.ImageDir, imageID)
	rootfsPath = filepath.Join(imageRoot, "rootfs")
	if err = os.MkdirAll(rootfsPath, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("create rootfs dir: %w", err)
	}

	// Download to a temp file.
	tmpFile, err := os.CreateTemp("", "clustr-pull-*"+urlExt(url))
	if err != nil {
		return "", 0, "", fmt.Errorf("create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	f.Logger.Info().Str("image_id", imageID).Str("url", url).Msg("factory: downloading blob")
	ph.SetPhase("downloading_iso")
	if err = downloadURLWithResume(ctx, url, tmpFile, 0, func(done, total int64) {
		ph.SetProgress(done, total)
	}); err != nil {
		tmpFile.Close()
		return "", 0, "", fmt.Errorf("download %s: %w", url, err)
	}
	tmpFile.Close()

	ext := strings.ToLower(urlExt(url))
	f.Logger.Info().Str("image_id", imageID).Str("ext", ext).Msg("factory: extracting image")

	switch {
	case ext == ".qcow2":
		ph.SetPhase("extracting")
		err = f.extractQcow2(ctx, imageID, tmpFile.Name(), rootfsPath)
	case ext == ".img" || ext == ".raw":
		ph.SetPhase("extracting")
		err = f.extractRaw(ctx, imageID, tmpFile.Name(), rootfsPath)
	case ext == ".tar.gz" || ext == ".tgz" || ext == ".tar":
		ph.SetPhase("extracting")
		err = extractTar(tmpFile.Name(), rootfsPath)
	case ext == ".tar.zst" || ext == ".tzst":
		ph.SetPhase("extracting")
		err = extractTarZst(ctx, tmpFile.Name(), rootfsPath)
	case ext == ".iso":
		// Installer ISOs cannot be directly mounted as a deployable rootfs.
		// Route to buildFromISOFile which runs the installer in a temp QEMU VM.
		// We pass a zero BuildFromISORequest so the method uses defaults; the
		// distro is auto-detected from the URL.
		return f.buildFromISOFile(ctx, imageID, url, tmpFile.Name(), imageRoot, api.BuildFromISORequest{}, ph)
	default:
		// Try treating unknown extensions as raw block images.
		ph.SetPhase("extracting")
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
	mnt, err := os.MkdirTemp("", "clustr-mount-*")
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
	if !f.acquireSem() {
		f.Logger.Warn().Str("image_id", imageID).Msg("factory: importISOAsync cancelled — server shutting down")
		_ = f.Store.UpdateBaseImageStatus(f.ctx, imageID, api.ImageStatusError, "server shutting down")
		return
	}
	defer f.releaseSem()

	ctx := f.ctx

	// Clean up the source file (may be a browser-upload temp) after we are done
	// with it regardless of success or failure.
	defer func() {
		if err := os.Remove(isoPath); err != nil && !os.IsNotExist(err) {
			f.Logger.Warn().Err(err).Str("path", isoPath).Msg("factory: cleanup iso temp file")
		}
	}()

	rootfs, _, _, err := f.extractISO(ctx, imageID, isoPath)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: import ISO failed")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	_ = f.finalizeImageFromRootfs(ctx, imageID, rootfs, finalizeSourceMetadata{
		BuildMethod: "import",
		Scrub:       false,
	})
}

func (f *Factory) extractISO(ctx context.Context, imageID, isoPath string) (rootfsPath string, sizeBytes int64, checksum string, err error) {
	imageRoot := filepath.Join(f.ImageDir, imageID)
	rootfsPath = filepath.Join(imageRoot, "rootfs")
	if err = os.MkdirAll(rootfsPath, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("create rootfs dir: %w", err)
	}

	// Mount the ISO.
	isomnt, err := os.MkdirTemp("", "clustr-iso-*")
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
	squashMnt, err := os.MkdirTemp("", "clustr-squash-*")
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

	rootfsMnt, err := os.MkdirTemp("", "clustr-rootfs-*")
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

// defaultCaptureExcludes are the rsync --exclude patterns always applied during capture.
// These paths are volatile, virtual, or identity-bearing and must never be captured.
var defaultCaptureExcludes = []string{
	"/proc/*",
	"/sys/*",
	"/dev/*",
	"/run/*",
	"/tmp/*",
	"/var/tmp/*",
	"/var/cache/*",
	"/var/log/*",
	"/boot/efi/*",
	"/lost+found",
	"/mnt/*",
	"/media/*",
	"/root/.bash_history",
	"/home/*/.bash_history",
	"/home/*/.ssh/authorized_keys",
}

// CaptureNode rsyncs a live server's filesystem into ImageDir/<id>/rootfs/ via SSH,
// scrubs node-specific identity data, auto-detects the disk layout, and finalizes
// the BaseImage record. Returns immediately with a "building" image; capture runs async.
// Poll GET /api/v1/images/:id to track status transitions: building -> ready | error.
//
// SSH host key verification is intentionally disabled (StrictHostKeyChecking=no) because
// capture targets are administrator-controlled golden nodes on a trusted management network.
// sshpass must be installed on the server host when SSHPassword is used.
func (f *Factory) CaptureNode(ctx context.Context, req CaptureRequest) (*api.BaseImage, error) {
	if req.SourceHost == "" {
		return nil, fmt.Errorf("factory: source_host is required")
	}

	// Reject capturing ourselves to prevent circular rsync + scrub of live system.
	if err := f.rejectSelfCapture(req.SourceHost); err != nil {
		return nil, err
	}

	sshUser := req.SSHUser
	if sshUser == "" {
		sshUser = "root"
	}
	sshPort := req.SSHPort
	if sshPort == 0 {
		sshPort = 22
	}

	id := uuid.New().String()
	if req.Tags == nil {
		req.Tags = []string{}
	}

	notes := req.Notes
	if notes != "" {
		notes += "\n"
	}
	notes += "Captured via SSH rsync (StrictHostKeyChecking=no -- trusted golden node)."

	img := api.BaseImage{
		ID:        id,
		Name:      req.Name,
		Version:   req.Version,
		OS:        req.OS,
		Arch:      req.Arch,
		Status:    api.ImageStatusBuilding,
		Format:    api.ImageFormatFilesystem,
		Tags:      req.Tags,
		Notes:     notes,
		CreatedAt: time.Now().UTC(),
	}

	f.Logger.Info().
		Str("image_id", id).
		Str("source_host", req.SourceHost).
		Str("ssh_user", sshUser).
		Int("ssh_port", sshPort).
		Msg("factory: capture node started")

	if err := f.Store.CreateBaseImage(ctx, img); err != nil {
		return nil, fmt.Errorf("factory: create image record: %w", err)
	}

	go f.captureAsync(id, req, sshUser, sshPort)

	return &img, nil
}

// rejectSelfCapture returns an error if host resolves to one of our own IPs or hostnames.
func (f *Factory) rejectSelfCapture(sourceHost string) error {
	host := sourceHost
	if idx := strings.Index(host, "@"); idx != -1 {
		host = host[idx+1:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	selfHostname, _ := os.Hostname()
	if selfHostname != "" && (host == selfHostname || strings.HasPrefix(host, selfHostname+".")) {
		return fmt.Errorf("factory: refusing to capture our own host (%s)", host)
	}

	sourceIPs, err := net.LookupHost(host)
	if err != nil {
		return nil // DNS failure — let rsync surface the error
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			for _, sip := range sourceIPs {
				if sip == ip.String() {
					return fmt.Errorf("factory: refusing to capture our own host (source IP %s matches local interface)", sip)
				}
			}
		}
	}
	return nil
}

func (f *Factory) captureAsync(imageID string, req CaptureRequest, sshUser string, sshPort int) {
	if !f.acquireSem() {
		f.Logger.Warn().Str("image_id", imageID).Msg("factory: captureAsync cancelled — server shutting down")
		_ = f.Store.UpdateBaseImageStatus(f.ctx, imageID, api.ImageStatusError, "server shutting down")
		return
	}
	defer f.releaseSem()

	ctx := f.ctx

	rootfs := filepath.Join(f.ImageDir, imageID, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: create rootfs dir")
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	// Cleanup partial rootfs on any failure path.
	success := false
	defer func() {
		if !success {
			if err := os.RemoveAll(filepath.Join(f.ImageDir, imageID, "rootfs")); err != nil && !os.IsNotExist(err) {
				f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: cleanup partial rootfs")
			}
		}
	}()

	// SSH transport options: always disable host key checking for golden nodes.
	sshOpts := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p %d", sshPort)
	if req.SSHKeyPath != "" {
		sshOpts += " -i " + req.SSHKeyPath
	}

	// Strip any embedded user@ from SourceHost — we control the user via sshUser.
	srcHost := req.SourceHost
	if idx := strings.Index(srcHost, "@"); idx != -1 {
		srcHost = srcHost[idx+1:]
	}
	rsyncSrc := fmt.Sprintf("%s@%s:/", sshUser, srcHost)

	// Build exclude list: built-in defaults + caller-supplied extras.
	var excludeArgs []string
	for _, p := range defaultCaptureExcludes {
		excludeArgs = append(excludeArgs, "--exclude="+p)
	}
	for _, p := range req.ExcludePaths {
		if p != "" {
			excludeArgs = append(excludeArgs, "--exclude="+p)
		}
	}

	// --one-file-system was intentionally removed: it prevented rsync from
	// crossing mount boundaries (e.g. /boot on a separate partition), which caused
	// deployed images to have an empty /boot. We now traverse all mounts under /.
	// Pseudo-filesystems (/proc, /sys, /dev) are excluded via defaultCaptureExcludes.
	rsyncArgs := []string{"-aAXvH", "--numeric-ids", "-e", sshOpts}
	rsyncArgs = append(rsyncArgs, excludeArgs...)
	rsyncArgs = append(rsyncArgs, rsyncSrc, rootfs+"/")

	var cmd *exec.Cmd
	if req.SSHPassword != "" {
		// sshpass wraps rsync to inject the password; requires: apt install sshpass
		args := append([]string{"-p", req.SSHPassword, "rsync"}, rsyncArgs...)
		cmd = exec.CommandContext(ctx, "sshpass", args...)
	} else {
		cmd = exec.CommandContext(ctx, "rsync", rsyncArgs...)
	}

	f.Logger.Info().
		Str("image_id", imageID).
		Str("src", rsyncSrc).
		Msg("factory: rsync started -- this may take several minutes for a full OS image")

	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := fmt.Sprintf("rsync failed: %v", err)
		if len(out) > 0 {
			tail := out
			if len(tail) > 1024 {
				tail = tail[len(tail)-1024:]
			}
			msg += "\nrsync output (tail):\n" + string(tail)
		}
		f.Logger.Error().Str("image_id", imageID).Msg(msg)
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, msg)
		return
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	f.Logger.Info().
		Str("image_id", imageID).
		Int("file_lines", len(lines)).
		Msg("factory: rsync complete")

	// Finalize: disk layout detection, scrub, tar, DB record, metadata sidecar.
	// Scrub is applied inside finalizeImageFromRootfs (Scrub: true).
	// Disk layout is detected from the rootfs heuristic (firmware="").
	if err := f.finalizeImageFromRootfs(ctx, imageID, rootfs, finalizeSourceMetadata{
		BuildMethod: "capture",
		Firmware:    "", // fall back to rootfs heuristic
		Scrub:       true,
	}); err == nil {
		success = true
	}
}

// detectDiskLayout inspects the captured rootfs and the image's declared firmware
// to determine the correct default disk layout. The firmware parameter takes
// precedence over rootfs inspection: if firmware=="bios", a biosboot layout is
// always emitted regardless of whether /boot/efi exists in the rootfs (BIOS images
// built from Rocky/RHEL ISOs install EFI shimx64.efi even in legacy-BIOS mode,
// which would otherwise trick the UEFI heuristic into emitting the wrong layout).
//
// firmware should be string(api.FirmwareBIOS) or string(api.FirmwareUEFI).
// An empty string falls back to rootfs-based detection (legacy behaviour for
// capture/pull paths that predate the firmware field).
func (f *Factory) detectDiskLayout(rootfs string, firmware string) api.DiskLayout {
	biosLayout := api.DiskLayout{
		Partitions: []api.PartitionSpec{
			{Label: "biosboot", SizeBytes: 1 * 1024 * 1024, Filesystem: "biosboot", MountPoint: "", Flags: []string{"bios_grub"}},
			{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
			{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
		},
		Bootloader: api.Bootloader{Type: "grub2", Target: "i386-pc"},
	}
	efiLayout := api.DiskLayout{
		Partitions: []api.PartitionSpec{
			{Label: "esp", SizeBytes: 512 * 1024 * 1024, Filesystem: "vfat", MountPoint: "/boot/efi", Flags: []string{"boot", "esp"}},
			{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
			{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
		},
		Bootloader: api.Bootloader{Type: "grub2", Target: "x86_64-efi"},
	}

	// Firmware field wins — don't trust rootfs heuristics when the caller knows
	// which firmware interface the image was built for.
	switch strings.ToLower(firmware) {
	case string(api.FirmwareBIOS):
		return biosLayout
	case string(api.FirmwareUEFI):
		return efiLayout
	}

	// No firmware declared — fall back to rootfs heuristic (capture/pull paths).
	efiDir := filepath.Join(rootfs, "boot", "efi")
	efiEntries, err := os.ReadDir(efiDir)
	if err == nil && len(efiEntries) > 0 {
		return efiLayout
	}
	return biosLayout
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
// variable CLUSTR_ALLOW_PRIVATE_URLS=true to bypass this check in lab environments.
func validatePullURL(rawURL string) error {
	if os.Getenv("CLUSTR_ALLOW_PRIVATE_URLS") == "true" {
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
					"set CLUSTR_ALLOW_PRIVATE_URLS=true to allow this in lab environments", ipStr)
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

// downloadURLWithProgress streams url into dst, calling onProgress(done,total)
// periodically during the download. When Content-Length is not available,
// total is -1. Throttled to at most one callback per second.
// downloadURLWithResume downloads rawURL into dst, resuming from resumeOffset
// bytes if the server supports HTTP Range requests. If the server returns 200
// (instead of 206) in response to a Range request, the existing partial file is
// truncated and the download starts from the beginning. resumeOffset == 0 means
// a plain GET with no Range header.
func downloadURLWithResume(ctx context.Context, rawURL string, dst *os.File, resumeOffset int64, onProgress func(done, total int64)) error {
	if err := validatePullURL(rawURL); err != nil {
		return fmt.Errorf("URL validation: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if resumeOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", resumeOffset))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	var startBytes int64
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		// Server honoured the Range request; continue appending.
		startBytes = resumeOffset
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Server ignored Range (returned 200); start over.
		if err := dst.Truncate(0); err != nil {
			return fmt.Errorf("truncate partial for restart: %w", err)
		}
		if _, err := dst.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("seek for restart: %w", err)
		}
		startBytes = 0
	default:
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, rawURL)
	}

	total := resp.ContentLength // -1 when unknown; for 206 this is the remaining bytes
	var done int64
	lastReport := time.Now()

	buf := make([]byte, 256*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			done += int64(n)
			if onProgress != nil && time.Since(lastReport) >= time.Second {
				onProgress(startBytes+done, startBytes+total)
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}
	if onProgress != nil {
		onProgress(startBytes+done, startBytes+total)
	}
	return nil
}

func downloadURLWithProgress(ctx context.Context, rawURL string, dst *os.File, onProgress func(done, total int64)) error {
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

	total := resp.ContentLength // -1 when unknown
	var done int64
	lastReport := time.Now()

	buf := make([]byte, 256*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write: %w", writeErr)
			}
			done += int64(n)
			if onProgress != nil && time.Since(lastReport) >= time.Second {
				onProgress(done, total)
				lastReport = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}
	if onProgress != nil {
		onProgress(done, total)
	}
	return nil
}

// copyFile copies src to dst, creating dst if it does not exist.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// buildManifest is the JSON structure persisted as build.json.
type buildManifest struct {
	ImageID     string        `json:"image_id"`
	Distro      string        `json:"distro"`
	RoleIDs     []string      `json:"role_ids,omitempty"`
	SizeBytes   int64         `json:"size_bytes"`
	Checksum    string        `json:"checksum"`
	ElapsedSecs float64       `json:"elapsed_secs"`
	BuiltAt     time.Time     `json:"built_at"`
}

// writeBuildManifest persists a build.json summary in imageRoot.
func writeBuildManifest(imageRoot, imageID, distro string, roleIDs []string, sizeBytes int64, checksum string, elapsed time.Duration) error {
	m := buildManifest{
		ImageID:     imageID,
		Distro:      distro,
		RoleIDs:     roleIDs,
		SizeBytes:   sizeBytes,
		Checksum:    checksum,
		ElapsedSecs: elapsed.Seconds(),
		BuiltAt:     time.Now().UTC(),
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(imageRoot, "build.json"), data, 0o644)
}

// extractTar extracts a .tar.gz / .tgz / .tar into dst using the system tar.
func extractTar(srcPath, dstPath string) error {
	cmd := exec.Command("tar", "-xf", srcPath, "-C", dstPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar extract: %w\noutput: %s", err, string(out))
	}
	return nil
}

// extractTarZst extracts a .tar.zst archive by piping zstdcat into tar.
// Falls back to "zstd -dc | tar -xf -" if zstdcat is not found.
func extractTarZst(ctx context.Context, srcPath, dstPath string) error {
	// Try zstdcat first (part of zstd package).
	if path, err := exec.LookPath("zstdcat"); err == nil {
		zstd := exec.CommandContext(ctx, path, srcPath)
		tarCmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dstPath)
		r, w := io.Pipe()
		zstd.Stdout = w
		tarCmd.Stdin = r
		var zstdErr, tarErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer w.Close()
			if out, err := zstd.CombinedOutput(); err != nil {
				zstdErr = fmt.Errorf("zstdcat: %w\noutput: %s", err, string(out))
			}
		}()
		if out, err := tarCmd.CombinedOutput(); err != nil {
			tarErr = fmt.Errorf("tar (from zstdcat): %w\noutput: %s", err, string(out))
		}
		<-done
		if zstdErr != nil {
			return zstdErr
		}
		return tarErr
	}

	// Fallback: use "zstd -dc" piped into tar.
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open zst: %w", err)
	}
	defer f.Close()

	zstdCmd := exec.CommandContext(ctx, "zstd", "-dc")
	zstdCmd.Stdin = f
	tarCmd := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", dstPath)
	pr, pw := io.Pipe()
	zstdCmd.Stdout = pw
	tarCmd.Stdin = pr

	if err := zstdCmd.Start(); err != nil {
		return fmt.Errorf("zstd start: %w", err)
	}
	if err := tarCmd.Start(); err != nil {
		zstdCmd.Process.Kill()
		return fmt.Errorf("tar start: %w", err)
	}

	zstdRunErr := zstdCmd.Wait()
	pw.Close()
	tarRunErr := tarCmd.Wait()

	if zstdRunErr != nil {
		return fmt.Errorf("zstd decompress: %w", zstdRunErr)
	}
	return tarRunErr
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

// bakeDeterministicTar builds a deterministic uncompressed tar of rootfsDir and
// writes it to <imageRoot>/rootfs.tar. It returns the tar file path, its
// sha256 hex string, and its size in bytes.
//
// Determinism flags passed to GNU tar:
//
//	--sort=name          — lexical entry order (same across kernel readdir orderings)
//	--mtime=@0           — normalise all timestamps to UNIX epoch 0
//	--owner=0 --group=0  — zero ownership numerically
//	--numeric-owner      — suppress name lookups (host-independent)
//	--pax-option=...     — strip pax extended atime/ctime headers (timestamp leakage)
//
// Preservation flags (symmetric with streamExtract in rsync.go):
//
//	--xattrs --xattrs-include='*'  — capture all extended attributes
//	--selinux                      — capture SELinux security contexts
//	--acls                         — capture POSIX ACL entries
//
// The tar excludes the same runtime/security paths that streamFilesystemBlob
// excludes so the sha256 matches what the deploy agent would verify.
//
// Requires GNU tar on the host (gtar or tar with --sort and --xattrs support).
func (f *Factory) bakeDeterministicTar(ctx context.Context, imageID, imageRoot, rootfsDir string) (tarPath string, sha256hex string, sizeBytes int64, err error) {
	tarPath = filepath.Join(imageRoot, "rootfs.tar")

	f.Logger.Info().Str("image_id", imageID).Str("rootfs", rootfsDir).Str("tar", tarPath).
		Msg("factory: baking deterministic rootfs tar")

	cmd := exec.CommandContext(ctx, "tar", //nolint:gosec
		"--sort=name",
		"--mtime=@0",
		"--owner=0", "--group=0", "--numeric-owner",
		"--pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime",
		// Preserve extended attributes, SELinux contexts, and POSIX ACLs so
		// that capture → deploy → re-capture round-trips are lossless.
		// These flags mirror the extract flags in internal/deploy/rsync.go
		// streamExtract, ensuring symmetric preservation.
		"--xattrs",
		"--xattrs-include=*",
		"--selinux",
		"--acls",
		// Exclude the same runtime/security paths as streamFilesystemBlob so
		// the tar sha256 is stable and matches what deploy agents verify.
		"--exclude=./proc/*",
		"--exclude=./sys/*",
		"--exclude=./dev/*",
		"--exclude=./.clustr-state",
		"--exclude=./etc/shadow",
		"--exclude=./etc/shadow-",
		"--exclude=./etc/gshadow",
		"--exclude=./etc/gshadow-",
		"--exclude=./etc/security/opasswd",
		"--exclude=./var/lib/sss",
		"--exclude=./var/lib/nslcd",
		"--exclude=./var/lib/chrony",
		"--exclude=./var/log/sssd",
		"--exclude=./var/log/chrony",
		"--exclude=./usr/bin/sudo",
		"--exclude=./usr/bin/sudoreplay",
		"--exclude=./usr/libexec/sudo/sesh",
		"--exclude=./etc/sssd",
		"--exclude=./etc/polkit-1/rules.d",
		"--exclude=./home/clustr",
		"--exclude=./usr/bin/staprun",
		"-C", rootfsDir,
		"-cf", tarPath,
		".",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", 0, fmt.Errorf("factory: bake tar: %w\noutput: %s", err, string(out))
	}

	sha256hex, sizeBytes, err = sha256OfFile(tarPath)
	if err != nil {
		return "", "", 0, fmt.Errorf("factory: sha256 tar: %w", err)
	}

	f.Logger.Info().Str("image_id", imageID).Str("tar", tarPath).
		Str("sha256", sha256hex).Int64("size_bytes", sizeBytes).
		Msg("factory: deterministic tar baked")

	return tarPath, sha256hex, sizeBytes, nil
}

// sha256OfFile reads the file at path, computes its SHA-256, and returns the
// hex-encoded digest along with the file size in bytes.
func sha256OfFile(path string) (hexDigest string, sizeBytes int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("sha256: open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("sha256: read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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
	if strings.HasSuffix(strings.ToLower(base), ".tar.zst") {
		return ".tar.zst"
	}
	return filepath.Ext(base)
}

// ─── ISO installer pipeline ───────────────────────────────────────────────────

// BuildFromISO downloads an installer ISO from a URL, runs it inside a
// temporary QEMU VM with an auto-generated kickstart/autoinstall config,
// extracts the installed root filesystem, and finalizes a BaseImage record.
//
// Returns immediately (202) with a "building" record; the install runs async.
// Poll GET /api/v1/images/:id to track: building → ready | error.
//
// The build can take 5-30 minutes depending on the distro, disk size, and
// whether KVM acceleration is available.
func (f *Factory) BuildFromISO(ctx context.Context, req api.BuildFromISORequest) (*api.BaseImage, error) {
	id := uuid.New().String()

	if req.Tags == nil {
		req.Tags = []string{}
	}
	if req.DiskSizeGB == 0 {
		req.DiskSizeGB = 20
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = 2048
	}
	if req.CPUs == 0 {
		req.CPUs = 2
	}

	// Pre-flight: ensure required host binaries are present, auto-installing
	// any missing packages before creating the DB record.
	missing := isoinstaller.EnsureDependencies()
	if len(missing) > 0 {
		return nil, fmt.Errorf("factory: ISO build requires missing host tools: %s — "+
			"install them on the clustr-server host (e.g. dnf install qemu-kvm qemu-img genisoimage)",
			strings.Join(missing, ", "))
	}

	// Auto-detect distro from URL when not explicitly supplied.
	distro := isoinstaller.Distro(req.Distro)
	if distro == "" || distro == isoinstaller.DistroUnknown {
		detected, _ := isoinstaller.DetectDistro(req.URL, "")
		if detected != isoinstaller.DistroUnknown {
			distro = detected
		} else {
			// We can still proceed — the kickstart fallback is RHEL-style which
			// works for Rocky/Alma/CentOS. Flag this in notes so the admin knows.
			distro = isoinstaller.DistroUnknown
		}
	}

	os_ := req.OS
	if os_ == "" {
		os_ = string(distro)
	}

	// Validate firmware field — only "bios" and "uefi" (case-insensitive) are accepted.
	// Silently defaulting to uefi on unknown values would hide typos; fail fast instead.
	firmware := api.ImageFirmware(strings.ToLower(string(req.Firmware)))
	if req.Firmware == "" {
		firmware = api.FirmwareUEFI // safe default when omitted
	} else if firmware != api.FirmwareBIOS && firmware != api.FirmwareUEFI {
		return nil, fmt.Errorf("factory: unknown firmware %q -- accepted values are bios and uefi", req.Firmware)
	}

	// Build the placeholder disk layout that matches the requested firmware.
	// This is replaced after rootfs extraction with the detected actual layout.
	var placeholderLayout api.DiskLayout
	if firmware == api.FirmwareBIOS {
		placeholderLayout = api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", SizeBytes: 1 * 1024 * 1024, Filesystem: "biosboot", MountPoint: "", Flags: []string{"bios_grub"}},
				{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
				{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
			},
			Bootloader: api.Bootloader{Type: "grub2", Target: "i386-pc"},
		}
	} else {
		placeholderLayout = api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "esp", SizeBytes: 512 * 1024 * 1024, Filesystem: "vfat", MountPoint: "/boot/efi", Flags: []string{"esp", "boot"}},
				{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
				{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
			},
			Bootloader: api.Bootloader{Type: "grub2", Target: "x86_64-efi"},
		}
	}

	img := api.BaseImage{
		ID:        id,
		Name:      req.Name,
		Version:   req.Version,
		OS:        os_,
		Arch:      req.Arch,
		Status:    api.ImageStatusBuilding,
		Format:    api.ImageFormatFilesystem,
		Firmware:  firmware,
		Tags:      req.Tags,
		Notes:     req.Notes,
		SourceURL: req.URL,
		CreatedAt: time.Now().UTC(),
		// BuiltForRoles records which HPC role presets were selected at build time.
		// The node-assignment UI uses this to warn when an admin assigns a gpu-compute
		// image to a node tagged as plain "compute" (or vice versa).
		BuiltForRoles: req.RoleIDs,
		// BuildMethod marks this image as built from an ISO installer. The web UI
		// checks this field to know when to show the live build progress panel
		// (with SSE serial console streaming) instead of the static image detail view.
		BuildMethod: "iso",
		DiskLayout:  placeholderLayout,
	}

	f.Logger.Info().
		Str("image_id", id).
		Str("url", req.URL).
		Str("distro", string(distro)).
		Strs("role_ids", req.RoleIDs).
		Bool("install_updates", req.InstallUpdates).
		Bool("kvm_available", isoinstaller.HasKVM()).
		Msg("factory: ISO build started")

	if err := f.Store.CreateBaseImage(ctx, img); err != nil {
		return nil, fmt.Errorf("factory: create image record: %w", err)
	}

	go f.buildISOAsync(id, req, distro)

	return &img, nil
}

func (f *Factory) buildISOAsync(imageID string, req api.BuildFromISORequest, distro isoinstaller.Distro) {
	if !f.acquireSem() {
		f.Logger.Warn().Str("image_id", imageID).Msg("factory: buildISOAsync cancelled — server shutting down")
		_ = f.Store.UpdateBaseImageStatus(f.ctx, imageID, api.ImageStatusError, "server shutting down")
		return
	}
	defer f.releaseSem()

	ctx := f.ctx

	// ── Progress handle ───────────────────────────────────────────────────
	// Acquire a progress handle if a reporter is wired in; otherwise use the
	// no-op handle so the rest of the function doesn't need nil checks.
	var ph BuildHandle
	if f.BuildProgress != nil {
		ph = f.BuildProgress.Start(imageID)
	} else {
		ph = noopBuildHandle{}
	}

	// failBuild is a helper that marks the progress handle and DB record failed.
	failBuild := func(msg string, err error) {
		fullMsg := msg
		if err != nil {
			fullMsg = fmt.Sprintf("%s: %v", msg, err)
		}
		ph.Fail(fullMsg)
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, fullMsg)
	}

	// ── Download ISO (with persistent cache) ─────────────────────────────
	f.Logger.Info().Str("image_id", imageID).Str("url", req.URL).Msg("factory: downloading installer ISO")
	ph.SetPhase("downloading_iso")

	isoPath, partialPath, err := f.isoCachePath(req.URL)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: resolve iso cache path")
		failBuild("resolve iso cache path", err)
		return
	}

	// Check if a complete cached ISO already exists and is non-zero.
	if fi, statErr := os.Stat(isoPath); statErr == nil && fi.Size() > 0 {
		f.Logger.Info().Str("image_id", imageID).Str("path", isoPath).
			Int64("bytes", fi.Size()).Msg("factory: using cached ISO, skipping download")
		ph.SetProgress(fi.Size(), fi.Size())
	} else {
		// Determine byte offset for resume: use size of any existing .partial file.
		var resumeOffset int64
		if pfi, pErr := os.Stat(partialPath); pErr == nil && pfi.Size() > 0 {
			resumeOffset = pfi.Size()
			f.Logger.Info().Str("image_id", imageID).Int64("resume_from", resumeOffset).
				Msg("factory: resuming partial ISO download")
		}

		isoFile, openErr := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			f.Logger.Error().Err(openErr).Str("image_id", imageID).Msg("factory: open partial iso file")
			failBuild("open partial iso file", openErr)
			return
		}
		if resumeOffset > 0 {
			if _, seekErr := isoFile.Seek(0, io.SeekEnd); seekErr != nil {
				isoFile.Close()
				failBuild("seek partial iso file", seekErr)
				return
			}
		}

		dlErr := downloadURLWithResume(ctx, req.URL, isoFile, resumeOffset, func(done, total int64) {
			ph.SetProgress(done, total)
		})
		isoFile.Close()
		if dlErr != nil {
			// Leave .partial in place so the next attempt can resume.
			f.Logger.Error().Err(dlErr).Str("image_id", imageID).Msg("factory: download ISO failed")
			failBuild("download ISO", dlErr)
			return
		}

		// Atomically promote .partial → final cache path.
		if renameErr := os.Rename(partialPath, isoPath); renameErr != nil {
			f.Logger.Error().Err(renameErr).Str("image_id", imageID).Msg("factory: rename partial iso")
			failBuild("finalize iso cache", renameErr)
			return
		}
		f.Logger.Info().Str("image_id", imageID).Str("path", isoPath).Msg("factory: ISO download complete, cached")
	}

	// isoPath now points to the complete, cached ISO.
	// If distro was unknown at request time, try detecting from the file now.
	if distro == isoinstaller.DistroUnknown {
		if detected, _ := isoinstaller.DetectDistro(req.URL, isoPath); detected != isoinstaller.DistroUnknown {
			distro = detected
			f.Logger.Info().Str("image_id", imageID).Str("distro", string(distro)).
				Msg("factory: distro detected from ISO volume label")
		}
	}

	// ── Generating config ────────────────────────────────────────────────
	ph.SetPhase("generating_config")

	// ── Create work directory ─────────────────────────────────────────────
	workDir, err := os.MkdirTemp("", "clustr-iso-build-*")
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: create work dir")
		failBuild("create work dir", err)
		return
	}
	defer os.RemoveAll(workDir)

	// ── Run installer VM ─────────────────────────────────────────────────
	f.Logger.Info().
		Str("image_id", imageID).
		Str("distro", string(distro)).
		Int("disk_gb", req.DiskSizeGB).
		Int("memory_mb", req.MemoryMB).
		Int("cpus", req.CPUs).
		Msg("factory: launching installer VM")
	ph.SetPhase("creating_disk")

	// Extend the install timeout when OS updates are requested.
	installTimeout := 45 * time.Minute
	if req.InstallUpdates {
		installTimeout = 90 * time.Minute
	}

	buildOpts := isoinstaller.BuildOptions{
		ISOPath:         isoPath,
		Distro:          distro,
		DiskSizeGB:      req.DiskSizeGB,
		MemoryMB:        req.MemoryMB,
		CPUs:            req.CPUs,
		Timeout:         installTimeout,
		WorkDir:         workDir,
		Logger:          f.Logger,
		CustomKickstart: req.CustomKickstart,
		RoleIDs:         req.RoleIDs,
		InstallUpdates:  req.InstallUpdates,
		DefaultUsername: defaultIfEmpty(req.DefaultUsername, "clustr"),
		DefaultPassword: defaultIfEmpty(req.DefaultPassword, "clustr"),
		Firmware:        req.Firmware,
		SELinuxMode:     defaultIfEmpty(req.SELinuxMode, "disabled"),
		BaseEnvironment: req.BaseEnvironment, // empty → kickstart.go defaults to "minimal-environment"
		BuildID:         imageID,             // used to name the systemd-run scope unit
		// Progress callbacks — feed events into the build handle.
		OnPhase:      ph.SetPhase,
		OnSerialLine: ph.AddSerialLine,
		OnStderrLine: ph.AddStderrLine,
	}

	result, err := isoinstaller.Build(ctx, buildOpts)

	// ── Persist build log regardless of success/failure ───────────────────
	imageRoot := filepath.Join(f.ImageDir, imageID)
	if mkErr := os.MkdirAll(imageRoot, 0o755); mkErr == nil {
		if result != nil && result.SerialLogPath != "" {
			destLog := filepath.Join(imageRoot, "build.log")
			_ = copyFile(result.SerialLogPath, destLog)
		}
	}

	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: installer VM failed")
		failBuild("installer VM", err)
		return
	}

	f.Logger.Info().
		Str("image_id", imageID).
		Dur("elapsed", result.ElapsedTime.Round(time.Second)).
		Str("disk", result.RawDiskPath).
		Msg("factory: installer VM complete — extracting rootfs")

	// ── Extract rootfs from installed disk ────────────────────────────────
	ph.SetPhase("extracting")
	rootfsPath := filepath.Join(imageRoot, "rootfs")
	if err := os.MkdirAll(rootfsPath, 0o755); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: create rootfs dir")
		failBuild("create rootfs dir", err)
		return
	}

	extractOpts := isoinstaller.ExtractOptions{
		RawDiskPath:   result.RawDiskPath,
		RootfsDestDir: rootfsPath,
	}
	if err := isoinstaller.ExtractViaSubprocess(ctx, imageID, extractOpts, ph.AddSerialLine, ph.AddStderrLine); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: rootfs extraction failed")
		failBuild("extract rootfs", err)
		return
	}

	// ── Finalize: disk layout + scrub + tar + DB + metadata sidecar + manifest ──
	_ = f.finalizeImageFromRootfs(ctx, imageID, rootfsPath, finalizeSourceMetadata{
		Name:        req.Name,
		CreatedAt:   time.Now().UTC(),
		Distro:      string(distro),
		BuildMethod: "iso",
		Firmware:    req.Firmware,
		RoleIDs:     req.RoleIDs,
		ElapsedTime: result.ElapsedTime,
		Scrub:       true,
		Progress:    ph,
		FailBuild:   failBuild,
	})

	f.Logger.Info().
		Str("image_id", imageID).
		Str("distro", string(distro)).
		Dur("install_time", result.ElapsedTime.Round(time.Second)).
		Msg("factory: ISO build complete — image is ready")
}

// isoMetaSidecar is the JSON structure persisted as <sha256(url)>.iso.meta.json
// alongside the cached ISO. It caches the result of a probe so subsequent calls
// return immediately without re-extracting the ISO.
type isoMetaSidecar struct {
	ISOURL       string                 `json:"iso_url"`
	ProbedAt     time.Time              `json:"probed_at"`
	Distro       string                 `json:"distro,omitempty"`
	VolumeLabel  string                 `json:"volume_label,omitempty"`
	Environments []api.ISOEnvironmentGroup `json:"environments"`
	NoComps      bool                   `json:"no_comps,omitempty"`
}

// ProbeISO downloads (or cache-hits) an ISO, parses its comps XML, and
// returns available environment groups. If the ISO has no comps data
// (Ubuntu, Debian, minimal ISOs), environments is nil and noComps is true.
//
// Results are cached alongside the ISO as <sha256(url)>.iso.meta.json.
// Subsequent calls with the same URL return immediately from cache.
func (f *Factory) ProbeISO(ctx context.Context, rawURL string) (environments []api.ISOEnvironmentGroup, distro string, volumeLabel string, noComps bool, err error) {
	// Resolve paths.
	isoPath, partialPath, err := f.isoCachePath(rawURL)
	if err != nil {
		return nil, "", "", false, fmt.Errorf("probe iso: resolve cache path: %w", err)
	}
	sidecarPath := isoPath + ".meta.json"

	// Cache hit: read sidecar if it exists and matches the URL.
	if data, readErr := os.ReadFile(sidecarPath); readErr == nil {
		var meta isoMetaSidecar
		if jsonErr := json.Unmarshal(data, &meta); jsonErr == nil && meta.ISOURL == rawURL {
			envs := meta.Environments
			if envs == nil {
				envs = []api.ISOEnvironmentGroup{}
			}
			return envs, meta.Distro, meta.VolumeLabel, meta.NoComps, nil
		}
	}

	// Download the ISO if not cached.
	if fi, statErr := os.Stat(isoPath); statErr != nil || fi.Size() == 0 {
		var resumeOffset int64
		if pfi, pErr := os.Stat(partialPath); pErr == nil && pfi.Size() > 0 {
			resumeOffset = pfi.Size()
			f.Logger.Info().Str("url", rawURL).Int64("resume_from", resumeOffset).
				Msg("factory probe: resuming partial ISO download")
		}

		isoFile, openErr := os.OpenFile(partialPath, os.O_CREATE|os.O_WRONLY, 0o644)
		if openErr != nil {
			return nil, "", "", false, fmt.Errorf("probe iso: open partial file: %w", openErr)
		}
		if resumeOffset > 0 {
			if _, seekErr := isoFile.Seek(0, io.SeekEnd); seekErr != nil {
				isoFile.Close()
				return nil, "", "", false, fmt.Errorf("probe iso: seek partial file: %w", seekErr)
			}
		}

		dlErr := downloadURLWithResume(ctx, rawURL, isoFile, resumeOffset, nil)
		isoFile.Close()
		if dlErr != nil {
			return nil, "", "", false, fmt.Errorf("probe iso: download ISO: %w", dlErr)
		}
		if renameErr := os.Rename(partialPath, isoPath); renameErr != nil {
			return nil, "", "", false, fmt.Errorf("probe iso: finalize ISO cache: %w", renameErr)
		}
		f.Logger.Info().Str("url", rawURL).Str("path", isoPath).
			Msg("factory probe: ISO download complete, cached")
	}

	// Detect distro and volume label.
	detectedDistro, _ := isoinstaller.DetectDistro(rawURL, isoPath)
	var label string
	if lbl, lblErr := isoinstaller.ReadISOVolumeLabel(isoPath); lblErr == nil {
		label = lbl
	}
	distroStr := string(detectedDistro)

	// Parse comps XML from the ISO.
	envGroups, compsErr := comps.ProbeComps(isoPath)
	if compsErr != nil {
		return nil, "", "", false, fmt.Errorf("probe iso: parse comps: %w", compsErr)
	}

	// Convert comps.EnvironmentGroup → api.ISOEnvironmentGroup.
	apiEnvs := make([]api.ISOEnvironmentGroup, 0, len(envGroups))
	for _, eg := range envGroups {
		apiEnvs = append(apiEnvs, api.ISOEnvironmentGroup{
			ID:           eg.ID,
			Name:         eg.Name,
			Description:  eg.Description,
			DisplayOrder: eg.DisplayOrder,
			IsDefault:    eg.IsDefault,
		})
	}

	noComps = envGroups == nil
	if apiEnvs == nil {
		apiEnvs = []api.ISOEnvironmentGroup{}
	}

	// Write sidecar cache.
	meta := isoMetaSidecar{
		ISOURL:       rawURL,
		ProbedAt:     time.Now().UTC(),
		Distro:       distroStr,
		VolumeLabel:  label,
		Environments: apiEnvs,
		NoComps:      noComps,
	}
	if metaBytes, jsonErr := json.MarshalIndent(meta, "", "  "); jsonErr == nil {
		// Best-effort: failure to write sidecar is non-fatal.
		_ = os.WriteFile(sidecarPath, metaBytes, 0o644)
	}

	return apiEnvs, distroStr, label, noComps, nil
}

// buildFromISOFile is called from pullAndExtract when PullImage downloads an
// .iso URL. It hands off to the full installer pipeline using the already-
// downloaded temp file, avoiding a second download.
//
// ph receives phase and progress events (generating_config, creating_disk,
// launching_vm, installing, extracting, scrubbing) so the operator can monitor
// the QEMU installer and serial console output in the web UI.
//
// Returns the rootfs path, size, and checksum just like the other extract paths.
func (f *Factory) buildFromISOFile(
	ctx context.Context,
	imageID string,
	isoURL string,
	tmpISOPath string,
	imageRoot string,
	req api.BuildFromISORequest,
	ph BuildHandle,
) (rootfsPath string, sizeBytes int64, checksum string, err error) {
	rootfsPath = filepath.Join(imageRoot, "rootfs")
	if err = os.MkdirAll(rootfsPath, 0o755); err != nil {
		return "", 0, "", fmt.Errorf("create rootfs dir: %w", err)
	}

	// Detect distro from URL (ISO is already downloaded but we have the URL).
	distro := isoinstaller.Distro(req.Distro)
	if distro == "" || distro == isoinstaller.DistroUnknown {
		distro, _ = isoinstaller.DetectDistro(isoURL, tmpISOPath)
	}

	// Set defaults.
	diskSizeGB := req.DiskSizeGB
	if diskSizeGB == 0 {
		diskSizeGB = 20
	}
	memoryMB := req.MemoryMB
	if memoryMB == 0 {
		memoryMB = 2048
	}
	cpus := req.CPUs
	if cpus == 0 {
		cpus = 2
	}

	workDir, err := os.MkdirTemp("", "clustr-iso-build-*")
	if err != nil {
		return "", 0, "", fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(workDir)

	fileInstallTimeout := 30 * time.Minute
	if req.InstallUpdates {
		fileInstallTimeout = 60 * time.Minute
	}

	buildOpts := isoinstaller.BuildOptions{
		ISOPath:         tmpISOPath,
		Distro:          distro,
		DiskSizeGB:      diskSizeGB,
		MemoryMB:        memoryMB,
		CPUs:            cpus,
		Timeout:         fileInstallTimeout,
		WorkDir:         workDir,
		Logger:          f.Logger,
		CustomKickstart: req.CustomKickstart,
		RoleIDs:         req.RoleIDs,
		InstallUpdates:  req.InstallUpdates,
		DefaultUsername: defaultIfEmpty(req.DefaultUsername, "clustr"),
		DefaultPassword: defaultIfEmpty(req.DefaultPassword, "clustr"),
		SELinuxMode:     defaultIfEmpty(req.SELinuxMode, "disabled"),
		BaseEnvironment: req.BaseEnvironment, // empty → kickstart.go defaults to "minimal-environment"
		BuildID:         imageID,             // used to name the systemd-run scope unit
		// Wire progress callbacks into the build handle so the operator sees
		// phase transitions and serial console output in the web UI.
		OnPhase:      ph.SetPhase,
		OnSerialLine: ph.AddSerialLine,
		OnStderrLine: ph.AddStderrLine,
	}

	result, err := isoinstaller.Build(ctx, buildOpts)
	if err != nil {
		return "", 0, "", fmt.Errorf("installer VM: %w", err)
	}

	ph.SetPhase("extracting")
	extractOpts := isoinstaller.ExtractOptions{
		RawDiskPath:   result.RawDiskPath,
		RootfsDestDir: rootfsPath,
	}
	if err := isoinstaller.ExtractViaSubprocess(ctx, imageID, extractOpts, ph.AddSerialLine, ph.AddStderrLine); err != nil {
		return "", 0, "", fmt.Errorf("extract rootfs: %w", err)
	}

	diskLayout := f.detectDiskLayout(rootfsPath, req.Firmware)
	if dbErr := f.Store.UpdateDiskLayout(ctx, imageID, diskLayout); dbErr != nil {
		f.Logger.Warn().Err(dbErr).Str("image_id", imageID).Msg("factory: update disk layout (non-fatal)")
	}

	ph.SetPhase("scrubbing")
	if err := ScrubNodeIdentity(rootfsPath); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
	}

	ph.SetPhase("finalizing")
	sizeBytes, checksum, err = checksumDir(rootfsPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("checksum rootfs: %w", err)
	}

	return rootfsPath, sizeBytes, checksum, nil
}

// ─── Resume support (Feature F2) ─────────────────────────────────────────────

// ResumeFromPhase re-enters the ISO build pipeline at the given phase,
// reusing cached artifacts where possible. Runs asynchronously.
//
// Phase routing:
//   - "extracting"   → re-run ExtractViaSubprocess against existing disk.raw
//   - "finalizing"   → re-run sha256 + FinalizeBaseImage
//   - anything else  → restart full download + install
func (f *Factory) ResumeFromPhase(imageID string, img api.BaseImage, phase string) {
	ctx := context.Background()

	var ph BuildHandle
	if f.BuildProgress != nil {
		ph = f.BuildProgress.Start(imageID)
	} else {
		ph = noopBuildHandle{}
	}

	failResume := func(msg string, err error) {
		fullMsg := msg
		if err != nil {
			fullMsg = fmt.Sprintf("%s: %v", msg, err)
		}
		ph.Fail(fullMsg)
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, fullMsg)
	}

	imageRoot := filepath.Join(f.ImageDir, imageID)
	rootfsPath := filepath.Join(imageRoot, "rootfs")

	f.Logger.Info().
		Str("image_id", imageID).
		Str("phase", phase).
		Msg("factory: resuming build")

	switch phase {
	case "extracting":
		diskRaw := filepath.Join(imageRoot, "disk.raw")
		if _, err := os.Stat(diskRaw); err != nil {
			f.Logger.Warn().Str("image_id", imageID).Msg("factory: resume extracting — disk.raw not found, restarting from top")
			f.resumeFromDownload(ctx, imageID, img, ph, failResume)
			return
		}
		ph.SetPhase("extracting")
		if err := os.MkdirAll(rootfsPath, 0o755); err != nil {
			failResume("create rootfs dir", err)
			return
		}
		extractOpts := isoinstaller.ExtractOptions{
			RawDiskPath:   diskRaw,
			RootfsDestDir: rootfsPath,
		}
		if err := isoinstaller.ExtractViaSubprocess(ctx, imageID, extractOpts, ph.AddSerialLine, ph.AddStderrLine); err != nil {
			failResume("extract rootfs", err)
			return
		}
		f.resumeFinalize(ctx, imageID, imageRoot, rootfsPath, string(img.Firmware), ph, failResume)

	case "finalizing":
		if _, err := os.Stat(rootfsPath); err != nil {
			failResume("rootfs not found for finalizing resume", err)
			return
		}
		f.resumeFinalize(ctx, imageID, imageRoot, rootfsPath, string(img.Firmware), ph, failResume)

	default:
		f.resumeFromDownload(ctx, imageID, img, ph, failResume)
	}
}

// finalizeSourceMetadata carries optional context that post-finalization steps
// (metadata sidecar, build manifest) use. All fields are optional — callers
// leave fields zero when the information is not available.
type finalizeSourceMetadata struct {
	// Name is the human-readable image name (for metadata sidecar).
	Name string
	// CreatedAt is when the image record was created (for metadata sidecar).
	CreatedAt time.Time
	// Distro is the detected OS distro string (e.g. "rocky", "ubuntu").
	Distro string
	// BuildMethod is one of "pull", "import", "capture", "iso".
	BuildMethod string
	// Firmware is the image's declared firmware interface ("bios"/"uefi"/""=detect).
	Firmware string
	// RoleIDs are the HPC role preset IDs selected at build time (ISO path only).
	RoleIDs []string
	// ElapsedTime is the total build duration (ISO path only; 0 = unknown).
	ElapsedTime time.Duration
	// Progress is the build progress handle for phase reporting. When nil, phase
	// updates are silently skipped. The caller must already have called SetPhase
	// for any phases before "scrubbing"; finalizeImageFromRootfs takes over at scrub.
	Progress BuildHandle
	// FailBuild is the caller's error-reporting closure. When nil, errors are
	// returned as a Go error value instead (callers that manage their own fail path).
	FailBuild func(string, error)
	// Scrub controls whether ScrubNodeIdentity is applied before tar baking.
	// Set false for pull/import paths where the source is already a clean rootfs.
	// Set true for capture and ISO paths where the image was extracted from a live node.
	Scrub bool
}

// finalizeImageFromRootfs is the single post-extraction finalization pipeline shared
// by all five async build paths: pullAsync, importISOAsync, captureAsync,
// buildISOAsync, and resumeFinalize. All post-finalization steps land here.
//
// Steps (in order):
//  1. Detect and persist disk layout (from rootfs heuristics or firmware field).
//  2. Optional identity scrub (controlled by meta.Scrub).
//  3. Bake deterministic tar + compute sha256.
//  4. Persist blob path and finalize the DB record (status → ready).
//  5. Write image metadata sidecar (non-fatal).
//  6. Write build manifest (non-fatal).
//
// On success, marks ph.Complete() when a progress handle is present.
// On error, calls meta.FailBuild(msg, err) when provided; otherwise returns the error.
func (f *Factory) finalizeImageFromRootfs(
	ctx context.Context,
	imageID string,
	rootfsPath string,
	meta finalizeSourceMetadata,
) error {
	imageRoot := filepath.Join(f.ImageDir, imageID)

	ph := meta.Progress
	if ph == nil {
		ph = noopBuildHandle{}
	}

	fail := func(msg string, err error) error {
		fullMsg := msg
		if err != nil {
			fullMsg = fmt.Sprintf("%s: %v", msg, err)
		}
		if meta.FailBuild != nil {
			meta.FailBuild(msg, err)
			return nil // caller owns reporting; don't double-report
		}
		_ = f.Store.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, fullMsg)
		return fmt.Errorf("%s", fullMsg)
	}

	// ── 1. Disk layout detection ─────────────────────────────────────────────
	diskLayout := f.detectDiskLayout(rootfsPath, meta.Firmware)
	if err := f.Store.UpdateDiskLayout(ctx, imageID, diskLayout); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: update disk layout (non-fatal)")
	}

	// ── 2. Identity scrub (capture + ISO paths only) ─────────────────────────
	if meta.Scrub {
		ph.SetPhase("scrubbing")
		if err := ScrubNodeIdentity(rootfsPath); err != nil {
			f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
		}
	}

	// ── 3. Bake deterministic tar ─────────────────────────────────────────────
	ph.SetPhase("finalizing")
	tarPath, tarChecksum, tarSize, err := f.bakeDeterministicTar(ctx, imageID, imageRoot, rootfsPath)
	if err != nil {
		return fail("bake tar", err)
	}

	// ── 4. Persist blob path + finalize DB record ────────────────────────────
	if err := f.Store.SetBlobPath(ctx, imageID, tarPath); err != nil {
		return fail("set blob path", err)
	}
	if err := f.Store.FinalizeBaseImage(ctx, imageID, tarSize, tarChecksum); err != nil {
		return fail("finalize image", err)
	}

	// ── 5. Metadata sidecar (non-fatal) ─────────────────────────────────────
	createdAt := meta.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	f.writeImageMetadataSidecar(ctx, imageID, meta.Name, createdAt, rootfsPath,
		meta.Distro, tarChecksum, tarSize, meta.BuildMethod)

	// ── 6. Build manifest (non-fatal) ────────────────────────────────────────
	_ = writeBuildManifest(imageRoot, imageID, meta.Distro, meta.RoleIDs, tarSize, tarChecksum, meta.ElapsedTime)

	ph.Complete()
	f.Logger.Info().
		Str("image_id", imageID).
		Int64("size_bytes", tarSize).
		Str("checksum", tarChecksum).
		Msg("factory: finalize complete — image is ready")

	return nil
}

// resumeFinalize performs scrub → checksum → finalize, shared between resume paths.
// firmware is string(img.Firmware) from the in-progress image record; pass ""
// to fall back to rootfs heuristics (capture paths).
func (f *Factory) resumeFinalize(ctx context.Context, imageID, imageRoot, rootfsPath, firmware string, ph BuildHandle, failBuild func(string, error)) {
	// Delegate to the unified finalize path.
	_ = imageRoot // imageRoot is computed internally from f.ImageDir + imageID
	_ = f.finalizeImageFromRootfs(ctx, imageID, rootfsPath, finalizeSourceMetadata{
		Firmware:  firmware,
		Scrub:     true,
		Progress:  ph,
		FailBuild: failBuild,
	})
}

// ─── Metadata sidecar helpers ─────────────────────────────────────────────────

// extractKernelVersion returns the best kernel version string found by
// globbing /boot/vmlinuz-* inside rootfsPath. Returns "" when none is found.
//
// "Best" means: prefer non-rescue kernels over rescue kernels. Within each
// category, sort lexicographically descending so the newest version wins.
// Rocky/RHEL rescue kernels have version strings starting with "0-rescue-".
func extractKernelVersion(rootfsPath string) string {
	bootDir := filepath.Join(rootfsPath, "boot")
	entries, err := os.ReadDir(bootDir)
	if err != nil {
		return ""
	}
	var regular, rescue []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "vmlinuz-") {
			continue
		}
		kver := strings.TrimPrefix(name, "vmlinuz-")
		if kver == "" {
			continue
		}
		if strings.HasPrefix(kver, "0-rescue-") {
			rescue = append(rescue, kver)
		} else {
			regular = append(regular, kver)
		}
	}
	// Prefer the latest regular kernel; fall back to rescue if nothing else.
	candidates := regular
	if len(candidates) == 0 {
		candidates = rescue
	}
	if len(candidates) == 0 {
		return ""
	}
	// Sort descending so the lexicographically largest (latest) version is first.
	sort.Sort(sort.Reverse(sort.StringSlice(candidates)))
	return candidates[0]
}

// extractOSRelease parses /etc/os-release from the rootfs and returns
// (distro, version) where distro is lowercased ID (e.g. "rocky") and
// version is VERSION_ID (e.g. "10.0"). Returns ("", "") on parse failure.
func extractOSRelease(rootfsPath string) (distro, version string) {
	data, err := os.ReadFile(filepath.Join(rootfsPath, "etc", "os-release"))
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") {
			distro = strings.ToLower(strings.Trim(strings.TrimPrefix(line, "ID="), `"`))
		}
		if strings.HasPrefix(line, "VERSION_ID=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION_ID="), `"`)
		}
	}
	return distro, version
}

// capturePackageManifest shells out to rpm -qa inside the rootfs to get the
// full installed package NVR list. Returns nil (not an error) when rpm is absent
// (non-RPM distros) or when the rootfs is not yet populated with a package DB.
func capturePackageManifest(rootfsPath string) []string {
	// rpm --root allows querying an offline rootfs without chrooting.
	out, err := exec.Command("rpm",
		"--root", rootfsPath,
		"-qa",
		"--queryformat", "%{NAME}-%{VERSION}-%{RELEASE}.%{ARCH}\n",
	).Output()
	if err != nil {
		return nil // non-RPM distro or rpm not installed in the deployed rootfs
	}
	var pkgs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			pkgs = append(pkgs, line)
		}
	}
	sort.Strings(pkgs)
	return pkgs
}

// writeImageMetadataSidecar builds an ImageMetadata struct from a freshly
// extracted rootfs and writes it to the image directory. The checksum and size
// are the values already computed by checksumDir over the rootfs tree.
//
// The sidecar is written AFTER checksumDir so content_sha256 is always
// consistent with the sealed rootfs state.
func (f *Factory) writeImageMetadataSidecar(
	ctx context.Context,
	imageID string,
	imageName string,
	createdAt time.Time,
	rootfsPath string,
	distroName string,
	checksum string,
	sizeBytes int64,
	buildMethod string,
) {
	kver := extractKernelVersion(rootfsPath)
	if kver == "" {
		f.Logger.Warn().Str("image_id", imageID).
			Msg("factory: could not detect kernel version from /boot/vmlinuz-* (sidecar kernel_version will be empty)")
	}

	osDistro, osVersion := extractOSRelease(rootfsPath)
	if osDistro == "" {
		osDistro = distroName // fall back to the factory-detected distro name
	}

	pkgs := capturePackageManifest(rootfsPath)
	if pkgs == nil {
		f.Logger.Debug().Str("image_id", imageID).
			Msg("factory: rpm package manifest not captured (non-RPM or rpm unavailable)")
	}

	meta := ImageMetadata{
		ID:               imageID,
		Name:             imageName,
		Distro:           osDistro,
		DistroVersion:    osVersion,
		KernelVersion:    kver,
		KernelPinned:     false,
		Architecture:     "x86_64",
		FirmwareSupport:  []string{"bios", "uefi"},
		ContentSHA256:    checksum,
		ContentSizeBytes: sizeBytes,
		CreatedAt:        createdAt,
		BuildMethod:      buildMethod,
		PackageManifest:  pkgs,
	}

	if _, err := StoreMetadata(ctx, f.ImageDir, imageID, meta); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).
			Msg("factory: failed to write image metadata sidecar (non-fatal — image is still usable)")
		return
	}
	f.Logger.Info().
		Str("image_id", imageID).
		Str("kernel_version", kver).
		Str("distro", osDistro).
		Str("distro_version", osVersion).
		Int("packages", len(pkgs)).
		Msg("factory: image metadata sidecar written")
}

// resumeFromDownload re-runs the full ISO download + install pipeline.
func (f *Factory) resumeFromDownload(_ context.Context, imageID string, img api.BaseImage, _ BuildHandle, failBuild func(string, error)) {
	if img.SourceURL == "" {
		failBuild("cannot resume: source_url is empty on image record", nil)
		return
	}
	req := api.BuildFromISORequest{
		URL:  img.SourceURL,
		Name: img.Name,
	}
	distro, _ := isoinstaller.DetectDistro(req.URL, "")
	f.buildISOAsync(imageID, req, distro)
}

