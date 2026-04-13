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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/image/isoinstaller"
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

// Factory turns raw inputs into finalized BaseImages stored under ImageDir.
type Factory struct {
	Store         *db.DB
	ImageDir      string
	Logger        zerolog.Logger
	// BuildProgress, when non-nil, receives phase/progress/serial events during
	// ISO builds so the admin can monitor the VM installer in real time.
	// Wire in *server.BuildProgressStore via an adapter (see server.go).
	BuildProgress BuildProgressReporter
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
	case ext == ".tar.zst" || ext == ".tzst":
		err = extractTarZst(ctx, tmpFile.Name(), rootfsPath)
	case ext == ".iso":
		// Installer ISOs cannot be directly mounted as a deployable rootfs.
		// Route to BuildFromISO which runs the installer in a temp QEMU VM.
		// We pass a zero BuildFromISORequest so the method uses defaults; the
		// distro is auto-detected from the URL.
		return f.buildFromISOFile(ctx, imageID, url, tmpFile.Name(), imageRoot, api.BuildFromISORequest{})
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

	// Clean up the source file (may be a browser-upload temp) after we are done
	// with it regardless of success or failure.
	defer func() {
		if err := os.Remove(isoPath); err != nil && !os.IsNotExist(err) {
			f.Logger.Warn().Err(err).Str("path", isoPath).Msg("factory: cleanup iso temp file")
		}
	}()

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
	ctx := context.Background()

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

	// Auto-detect disk layout from the captured rootfs before scrubbing (EFI check).
	diskLayout := f.detectDiskLayout(rootfs)
	if err := f.Store.UpdateDiskLayout(ctx, imageID, diskLayout); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: set disk layout (non-fatal)")
	}

	// Scrub identity AFTER rsync completes — never during, or we remove files mid-transfer.
	f.Logger.Info().Str("image_id", imageID).Msg("factory: scrubbing node identity")
	if err := ScrubNodeIdentity(rootfs); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
	}

	f.Logger.Info().Str("image_id", imageID).Msg("factory: computing rootfs checksum")
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

	success = true
	f.Logger.Info().
		Str("image_id", imageID).
		Int64("size_bytes", size).
		Msg("factory: capture complete -- image is ready")
}

// detectDiskLayout inspects the captured rootfs to infer a disk layout.
// Checks for /boot/efi (UEFI) or falls back to BIOS/MBR. Admin should review before deploy.
func (f *Factory) detectDiskLayout(rootfs string) api.DiskLayout {
	efiDir := filepath.Join(rootfs, "boot", "efi")
	efiEntries, err := os.ReadDir(efiDir)
	if err == nil && len(efiEntries) > 0 {
		return api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "esp", SizeBytes: 512 * 1024 * 1024, Filesystem: "vfat", MountPoint: "/boot/efi", Flags: []string{"boot", "esp"}},
				{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
				{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
			},
			Bootloader: api.Bootloader{Type: "grub2", Target: "x86_64-efi"},
		}
	}
	return api.DiskLayout{
		Partitions: []api.PartitionSpec{
			{Label: "biosboot", SizeBytes: 1 * 1024 * 1024, Filesystem: "", MountPoint: "", Flags: []string{"bios_grub"}},
			{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
			{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
		},
		Bootloader: api.Bootloader{Type: "grub2", Target: "i386-pc"},
	}
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

// downloadURLWithProgress streams url into dst, calling onProgress(done,total)
// periodically during the download. When Content-Length is not available,
// total is -1. Throttled to at most one callback per second.
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

	// Pre-flight: check required host binaries before creating the DB record.
	missing := isoinstaller.CheckDependencies()
	if len(missing) > 0 {
		return nil, fmt.Errorf("factory: ISO build requires missing host tools: %s — "+
			"install them on the clonr-server host (e.g. dnf install qemu-kvm qemu-img genisoimage)",
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

	img := api.BaseImage{
		ID:        id,
		Name:      req.Name,
		Version:   req.Version,
		OS:        os_,
		Arch:      req.Arch,
		Status:    api.ImageStatusBuilding,
		Format:    api.ImageFormatFilesystem,
		Tags:      req.Tags,
		Notes:     req.Notes,
		SourceURL: req.URL,
		CreatedAt: time.Now().UTC(),
		// BuiltForRoles records which HPC role presets were selected at build time.
		// The node-assignment UI uses this to warn when an admin assigns a gpu-compute
		// image to a node tagged as plain "compute" (or vice versa).
		BuiltForRoles: req.RoleIDs,
		// Disk layout is determined from what the installer actually creates.
		// We set a bios-boot default here; it gets overwritten after extraction.
		DiskLayout: api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", SizeBytes: 1 * 1024 * 1024, Filesystem: "", MountPoint: "", Flags: []string{"bios_grub"}},
				{Label: "boot", SizeBytes: 1 * 1024 * 1024 * 1024, Filesystem: "xfs", MountPoint: "/boot"},
				{Label: "root", SizeBytes: 0, Filesystem: "xfs", MountPoint: "/"},
			},
			Bootloader: api.Bootloader{Type: "grub2", Target: "i386-pc"},
		},
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
	ctx := context.Background()

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

	// ── Download ISO ──────────────────────────────────────────────────────
	f.Logger.Info().Str("image_id", imageID).Str("url", req.URL).Msg("factory: downloading installer ISO")
	ph.SetPhase("downloading_iso")

	tmpISO, err := os.CreateTemp("", "clonr-iso-*.iso")
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: create temp iso file")
		failBuild("create temp iso file", err)
		return
	}
	tmpISO.Close()
	defer os.Remove(tmpISO.Name())

	// Re-open for writing with a counting writer so we can report download progress.
	isoFile, err := os.OpenFile(tmpISO.Name(), os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: open temp iso file")
		failBuild("open temp iso file", err)
		return
	}
	if err := downloadURLWithProgress(ctx, req.URL, isoFile, func(done, total int64) {
		ph.SetProgress(done, total)
	}); err != nil {
		isoFile.Close()
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: download ISO failed")
		failBuild("download ISO", err)
		return
	}
	isoFile.Close()

	// If distro was unknown at request time, try detecting from the file now.
	if distro == isoinstaller.DistroUnknown {
		if detected, _ := isoinstaller.DetectDistro(req.URL, tmpISO.Name()); detected != isoinstaller.DistroUnknown {
			distro = detected
			f.Logger.Info().Str("image_id", imageID).Str("distro", string(distro)).
				Msg("factory: distro detected from ISO volume label")
		}
	}

	// ── Generating config ────────────────────────────────────────────────
	ph.SetPhase("generating_config")

	// ── Create work directory ─────────────────────────────────────────────
	workDir, err := os.MkdirTemp("", "clonr-iso-build-*")
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
	installTimeout := 30 * time.Minute
	if req.InstallUpdates {
		installTimeout = 60 * time.Minute
	}

	buildOpts := isoinstaller.BuildOptions{
		ISOPath:         tmpISO.Name(),
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
		// Progress callbacks — feed events into the build handle.
		OnPhase:       ph.SetPhase,
		OnSerialLine:  ph.AddSerialLine,
		OnStderrLine:  ph.AddStderrLine,
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
	if err := isoinstaller.ExtractRootfs(extractOpts); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: rootfs extraction failed")
		failBuild("extract rootfs", err)
		return
	}

	// ── Detect disk layout from extracted rootfs ──────────────────────────
	diskLayout := f.detectDiskLayout(rootfsPath)
	if err := f.Store.UpdateDiskLayout(ctx, imageID, diskLayout); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: update disk layout (non-fatal)")
	}

	// ── Scrub identity ────────────────────────────────────────────────────
	ph.SetPhase("scrubbing")
	f.Logger.Info().Str("image_id", imageID).Msg("factory: scrubbing node identity")
	if err := ScrubNodeIdentity(rootfsPath); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
	}

	// ── Checksum ──────────────────────────────────────────────────────────
	ph.SetPhase("finalizing")
	f.Logger.Info().Str("image_id", imageID).Msg("factory: computing rootfs checksum")
	size, checksum, err := checksumDir(rootfsPath)
	if err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: checksum failed")
		failBuild("checksum rootfs", err)
		return
	}

	if err := f.Store.SetBlobPath(ctx, imageID, rootfsPath); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: set blob path")
		failBuild("set blob path", err)
		return
	}

	if err := f.Store.FinalizeBaseImage(ctx, imageID, size, checksum); err != nil {
		f.Logger.Error().Err(err).Str("image_id", imageID).Msg("factory: finalize failed")
		failBuild("finalize image", err)
		return
	}

	// ── Persist build manifest ────────────────────────────────────────────
	_ = writeBuildManifest(imageRoot, imageID, string(distro), req.RoleIDs, size, checksum, result.ElapsedTime)

	ph.Complete()
	f.Logger.Info().
		Str("image_id", imageID).
		Int64("size_bytes", size).
		Str("checksum", checksum).
		Str("distro", string(distro)).
		Dur("install_time", result.ElapsedTime.Round(time.Second)).
		Msg("factory: ISO build complete — image is ready")
}

// buildFromISOFile is called from pullAndExtract when PullImage downloads an
// .iso URL. It hands off to the full installer pipeline using the already-
// downloaded temp file, avoiding a second download.
//
// Returns the rootfs path, size, and checksum just like the other extract paths.
func (f *Factory) buildFromISOFile(
	ctx context.Context,
	imageID string,
	isoURL string,
	tmpISOPath string,
	imageRoot string,
	req api.BuildFromISORequest,
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

	workDir, err := os.MkdirTemp("", "clonr-iso-build-*")
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
	}

	result, err := isoinstaller.Build(ctx, buildOpts)
	if err != nil {
		return "", 0, "", fmt.Errorf("installer VM: %w", err)
	}

	extractOpts := isoinstaller.ExtractOptions{
		RawDiskPath:   result.RawDiskPath,
		RootfsDestDir: rootfsPath,
	}
	if err := isoinstaller.ExtractRootfs(extractOpts); err != nil {
		return "", 0, "", fmt.Errorf("extract rootfs: %w", err)
	}

	diskLayout := f.detectDiskLayout(rootfsPath)
	if dbErr := f.Store.UpdateDiskLayout(ctx, imageID, diskLayout); dbErr != nil {
		f.Logger.Warn().Err(dbErr).Str("image_id", imageID).Msg("factory: update disk layout (non-fatal)")
	}

	if err := ScrubNodeIdentity(rootfsPath); err != nil {
		f.Logger.Warn().Err(err).Str("image_id", imageID).Msg("factory: scrub had warnings (continuing)")
	}

	sizeBytes, checksum, err = checksumDir(rootfsPath)
	if err != nil {
		return "", 0, "", fmt.Errorf("checksum rootfs: %w", err)
	}

	return rootfsPath, sizeBytes, checksum, nil
}

