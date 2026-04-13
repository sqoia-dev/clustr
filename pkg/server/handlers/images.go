package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// defaultBlobMaxConcurrent is the default maximum number of simultaneous blob
// streams. Each stream can saturate a LAN link and consume significant memory
// for kernel socket buffers + the tar subprocess; limit to prevent OOM when an
// entire fleet reimages simultaneously.
const defaultBlobMaxConcurrent = 8

// ImagesHandler handles all /api/v1/images routes.
type ImagesHandler struct {
	DB       *db.DB
	ImageDir string
	// Progress is used by DeleteImage to check for active deploys.
	// It is optional — when nil, the active-deploy guard is skipped.
	Progress ProgressStoreIface
	// blobSem is the semaphore controlling max concurrent blob streams.
	// Initialised lazily on first use via blobSemaphore().
	blobSem chan struct{}
	// activeBlobStreams tracks the current count for metrics/logging.
	activeBlobStreams atomic.Int64
}

// blobSemaphore returns the blob concurrency semaphore, reading
// CLONR_BLOB_MAX_CONCURRENT from the environment on first call.
// The channel capacity is the configured limit; acquiring a slot is a
// non-blocking send (if full → 503); releasing is a receive.
func (h *ImagesHandler) blobSemaphore() chan struct{} {
	if h.blobSem != nil {
		return h.blobSem
	}
	cap := defaultBlobMaxConcurrent
	if v := os.Getenv("CLONR_BLOB_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cap = n
		}
	}
	h.blobSem = make(chan struct{}, cap)
	return h.blobSem
}

// ListImages handles GET /api/v1/images
func (h *ImagesHandler) ListImages(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	images, err := h.DB.ListBaseImages(r.Context(), status)
	if err != nil {
		log.Error().Err(err).Msg("list images")
		writeError(w, err)
		return
	}
	if images == nil {
		images = []api.BaseImage{}
	}
	writeJSON(w, http.StatusOK, api.ListImagesResponse{Images: images, Total: len(images)})
}

// CreateImage handles POST /api/v1/images
func (h *ImagesHandler) CreateImage(w http.ResponseWriter, r *http.Request) {
	var req api.CreateImageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}
	if req.Format == "" {
		writeValidationError(w, "format is required")
		return
	}

	img := api.BaseImage{
		ID:         uuid.New().String(),
		Name:       req.Name,
		Version:    req.Version,
		OS:         req.OS,
		Arch:       req.Arch,
		Status:     api.ImageStatusBuilding,
		Format:     req.Format,
		DiskLayout: req.DiskLayout,
		Tags:       req.Tags,
		SourceURL:  req.SourceURL,
		Notes:      req.Notes,
	}
	if img.Tags == nil {
		img.Tags = []string{}
	}

	if err := h.DB.CreateBaseImage(r.Context(), img); err != nil {
		log.Error().Err(err).Msg("create image")
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, img)
}

// GetImage handles GET /api/v1/images/:id
func (h *ImagesHandler) GetImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, img)
}

// ArchiveImage handles DELETE /api/v1/images/:id (legacy — kept for back-compat).
func (h *ImagesHandler) ArchiveImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.DB.ArchiveBaseImage(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteImage handles DELETE /api/v1/images/:id (real delete).
//
// Query params:
//
//	?force=true — unassign all nodes using the image and delete anyway.
//
// Rejection rules (in order):
//  1. Image not found → 404.
//  2. Active deployment in progress (any phase != "complete"/"error") → 409.
//  3. Nodes referencing the image (without force) → 409 with node list.
//
// On success:
//   - Removes the blob directory from disk.
//   - Deletes the DB record.
//   - Returns 204 No Content.
func (h *ImagesHandler) DeleteImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	force := r.URL.Query().Get("force") == "true"
	ctx := r.Context()

	// Confirm the image exists.
	img, err := h.DB.GetBaseImage(ctx, id)
	if err != nil {
		writeError(w, err)
		return
	}

	// Guard: reject if the image is currently being deployed (even with force).
	// We check the progress store for any non-terminal deploy phases.
	if h.Progress != nil {
		for _, p := range h.Progress.List() {
			phase := p.Phase
			if phase == "complete" || phase == "error" || phase == "" {
				continue
			}
			// Determine which image is being deployed by checking the log fields.
			// The progress entry only has node_mac; we fetch the node's assigned image.
			node, lookupErr := h.DB.GetNodeConfigByMAC(ctx, p.NodeMAC)
			if lookupErr != nil {
				continue
			}
			if node.BaseImageID == id {
				writeJSON(w, http.StatusConflict, api.ErrorResponse{
					Error: "image is currently being deployed to one or more nodes — wait for the deployment to complete or fail before deleting",
					Code:  "image_deploying",
				})
				return
			}
		}
	}

	// Guard: reject if nodes reference this image (unless force).
	nodes, err := h.DB.ListNodesByBaseImageID(ctx, id)
	if err != nil {
		log.Error().Err(err).Str("image_id", id).Msg("delete image: list nodes")
		writeError(w, err)
		return
	}
	if len(nodes) > 0 && !force {
		writeJSON(w, http.StatusConflict, api.ImageInUseResponse{
			Error: "image is assigned to one or more nodes; use ?force=true to unassign and delete",
			Code:  "image_in_use",
			Nodes: nodes,
		})
		return
	}

	// Force path: unassign all nodes first.
	if len(nodes) > 0 && force {
		if err := h.DB.ClearBaseImageOnNodes(ctx, id); err != nil {
			log.Error().Err(err).Str("image_id", id).Msg("delete image: clear nodes")
			writeError(w, err)
			return
		}
		log.Warn().Str("image_id", id).Int("node_count", len(nodes)).
			Msg("delete image: force-unassigned nodes")
	}

	// Remove blob directory from disk (best-effort — don't fail if missing).
	blobDir := filepath.Join(h.ImageDir, id)
	if rmErr := os.RemoveAll(blobDir); rmErr != nil && !os.IsNotExist(rmErr) {
		log.Error().Err(rmErr).Str("path", blobDir).Msg("delete image: remove blob dir")
		// Non-fatal: continue to remove the DB record so the image isn't
		// permanently undeletable due to a missing directory.
	}
	// Also remove legacy flat blob file (uploaded via /blob endpoint).
	legacyBlob := filepath.Join(h.ImageDir, id+".blob")
	_ = os.Remove(legacyBlob)

	// Delete the DB record.
	if err := h.DB.DeleteBaseImage(ctx, id); err != nil {
		log.Error().Err(err).Str("image_id", id).Msg("delete image: db delete")
		writeError(w, err)
		return
	}

	log.Warn().
		Str("image_id", id).
		Str("image_name", img.Name).
		Bool("force", force).
		Int("nodes_unassigned", len(nodes)).
		Msg("image deleted")

	w.WriteHeader(http.StatusNoContent)
}

// GetImageStatus handles GET /api/v1/images/:id/status
func (h *ImagesHandler) GetImageStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            img.ID,
		"status":        img.Status,
		"error_message": img.ErrorMessage,
	})
}

// GetDiskLayout handles GET /api/v1/images/:id/disklayout
func (h *ImagesHandler) GetDiskLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, img.DiskLayout)
}

// PutDiskLayout handles PUT /api/v1/images/:id/disklayout
func (h *ImagesHandler) PutDiskLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var layout api.DiskLayout
	if err := json.NewDecoder(r.Body).Decode(&layout); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// Fetch existing image to confirm it exists and is mutable.
	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status == api.ImageStatusArchived {
		writeValidationError(w, "cannot modify an archived image")
		return
	}

	img.DiskLayout = layout
	// Re-create by updating: simplest path since layout is stored as JSON column.
	// We use UpdateBaseImageStatus as a no-op to keep status, then update via SetBlobPath workaround.
	// Instead, we run a targeted update directly.
	if err := h.DB.UpdateDiskLayout(r.Context(), id, layout); err != nil {
		log.Error().Err(err).Msg("update disk layout")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, layout)
}

// UploadBlob handles POST /api/v1/images/:id/blob
// Streams the request body to disk and finalizes the image record.
func (h *ImagesHandler) UploadBlob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if img.Status == api.ImageStatusArchived {
		writeValidationError(w, "cannot upload blob for an archived image")
		return
	}

	if err := os.MkdirAll(h.ImageDir, 0o755); err != nil {
		log.Error().Err(err).Msg("create image dir")
		writeError(w, err)
		return
	}

	blobPath := filepath.Join(h.ImageDir, id+".blob")
	f, err := os.Create(blobPath)
	if err != nil {
		log.Error().Err(err).Str("path", blobPath).Msg("create blob file")
		writeError(w, err)
		return
	}
	defer f.Close()

	n, err := io.Copy(f, r.Body)
	if err != nil {
		log.Error().Err(err).Msg("write blob")
		_ = os.Remove(blobPath)
		writeError(w, err)
		return
	}

	checksum := r.Header.Get("X-Checksum-SHA256")

	if err := h.DB.SetBlobPath(r.Context(), id, blobPath); err != nil {
		writeError(w, err)
		return
	}
	if err := h.DB.FinalizeBaseImage(r.Context(), id, n, checksum); err != nil {
		writeError(w, err)
		return
	}

	updated, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// DownloadBlob handles GET /api/v1/images/:id/blob
//
// For "block" format images: streams the pre-packed blob file from disk.
// For "filesystem" format images: streams an uncompressed tar of rootfs/ on the fly.
func (h *ImagesHandler) DownloadBlob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Concurrency cap: limit simultaneous blob streams to prevent OOM under
	// fleet-wide reimage bursts. Return 503 with Retry-After when at capacity.
	sem := h.blobSemaphore()
	select {
	case sem <- struct{}{}:
		// Acquired a slot. Release it when the handler returns.
		active := h.activeBlobStreams.Add(1)
		defer func() {
			<-sem
			h.activeBlobStreams.Add(-1)
		}()
		log.Info().Int64("active_streams", active).Int("cap", cap(sem)).Msg("blob stream: acquired slot")
	default:
		active := h.activeBlobStreams.Load()
		log.Warn().Int64("active_streams", active).Int("cap", cap(sem)).
			Msg("blob stream: semaphore full — returning 503 to client")
		w.Header().Set("Retry-After", "10")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"error":"too many concurrent blob streams (max %d) — retry in 10s"}`, cap(sem))
		return
	}

	img, err := h.DB.GetBaseImage(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	if img.Format == api.ImageFormatFilesystem {
		h.streamFilesystemBlob(w, r, img)
		return
	}

	// Default: serve the pre-packed blob file (block images, legacy uploads).
	blobPath, err := h.DB.GetBlobPath(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	if blobPath == "" {
		writeValidationError(w, "blob not yet uploaded for this image")
		return
	}

	f, err := os.Open(blobPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, api.ErrNotFound)
			return
		}
		log.Error().Err(err).Str("path", blobPath).Msg("open blob")
		writeError(w, err)
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeError(w, err)
		return
	}

	// For block images, the DB checksum covers the blob file bytes directly —
	// safe to advertise to the client for end-to-end integrity verification.
	if img.Checksum != "" {
		w.Header().Set("X-Clonr-Blob-SHA256", img.Checksum)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(blobPath), stat.ModTime(), f)
}

// tarChecksumPath returns the path of the tar-checksum sidecar file for a
// filesystem-format image. The sidecar stores the sha256 of the tar stream
// produced by streamFilesystemBlob. It is computed on the first successful
// stream and reused on all subsequent streams.
//
// Sprint 1 compromise: the DB `checksum` column for filesystem images holds a
// directory-level hash (file-by-file sha256 XOR), which does not match the tar
// stream bytes. Until the image finalization step computes a canonical tar
// checksum during build, we compute and cache it on first-stream here.
func tarChecksumPath(imageDir, imageID string) string {
	return filepath.Join(imageDir, imageID, "tar-sha256")
}

// loadTarChecksum reads the cached tar sha256 for imageID, returning "" if not
// yet computed.
func loadTarChecksum(imageDir, imageID string) string {
	data, err := os.ReadFile(tarChecksumPath(imageDir, imageID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// saveTarChecksum persists the tar sha256 to the sidecar file.
func saveTarChecksum(imageDir, imageID, checksum string) {
	path := tarChecksumPath(imageDir, imageID)
	if err := os.WriteFile(path, []byte(checksum+"\n"), 0o644); err != nil {
		log.Warn().Err(err).Str("image_id", imageID).Str("path", path).
			Msg("blob stream: failed to persist tar checksum sidecar — next stream will recompute")
	}
}

// streamFilesystemBlob tars the rootfs/ directory of a filesystem-format image
// and streams it directly to the response writer as an uncompressed tar archive.
// No Content-Length is set — the response is streamed.
//
// The tar subprocess is NOT bound to the request context. Binding it caused
// SIGKILL mid-stream under concurrent load when the http.Server or an upstream
// component cancelled the context (e.g. write deadline, client-side timeout).
// Instead, we run tar freely and handle context cancellation manually: on client
// disconnect we give tar 2 seconds to flush remaining buffered output, then kill.
// A client disconnect is treated as informational, not a server error.
func (h *ImagesHandler) streamFilesystemBlob(w http.ResponseWriter, r *http.Request, img api.BaseImage) {
	rootfsPath := filepath.Join(h.ImageDir, img.ID, "rootfs")

	if _, err := os.Stat(rootfsPath); err != nil {
		if os.IsNotExist(err) {
			log.Error().Str("image_id", img.ID).Str("path", rootfsPath).Msg("blob stream: rootfs dir not found")
			writeError(w, api.ErrNotFound)
			return
		}
		log.Error().Err(err).Str("image_id", img.ID).Str("path", rootfsPath).Msg("blob stream: stat rootfs")
		writeError(w, err)
		return
	}

	// If we have a cached tar checksum from a prior stream, advertise it so the
	// deploy agent can verify end-to-end integrity. On the first stream the header
	// is absent — the client treats a missing header as "no verification possible"
	// and skips the check (with a warning). The sidecar is written after a
	// successful stream so subsequent downloads get the header.
	//
	// Sprint 1 compromise: for filesystem images the DB `checksum` column is a
	// directory-level hash (not a tar stream hash), so we maintain a separate
	// sidecar file. See tarChecksumPath for details.
	cachedTarChecksum := loadTarChecksum(h.ImageDir, img.ID)
	computeTarChecksum := cachedTarChecksum == "" // only compute on first stream

	log.Info().
		Str("image_id", img.ID).
		Str("format", string(img.Format)).
		Str("client", r.RemoteAddr).
		Bool("has_tar_checksum", cachedTarChecksum != "").
		Msg("blob stream: starting tar")

	// Wrap the response writer to count bytes streamed.
	// If this is the first stream, also tee through a sha256 hasher so we can
	// cache the tar checksum for all subsequent streams.
	// Headers are written AFTER the probe buffer fills (fail-fast path below).
	baseWriter := &countWriter{w: w}
	var tarHasher = sha256.New() // always created; only used when computeTarChecksum
	var cw io.Writer
	if computeTarChecksum {
		cw = io.MultiWriter(baseWriter, tarHasher)
	} else {
		cw = baseWriter
	}

	// Use exec.Command (no context) so the tar subprocess lifetime is NOT bound
	// to the HTTP request context. Context cancellation (client disconnect, proxy
	// timeout) was SIGKILL'ing tar mid-stream under concurrent load, delivering
	// truncated archives to the deploy agents.
	//
	// Excluded paths fall into two categories:
	//   1. Virtual/pseudo filesystems that are empty or populated at runtime
	//      (proc, sys, dev, selinux) — no useful data, and tar errors on them.
	//   2. Security-sensitive or restricted files that are mode 000 or SUID root
	//      on a locked-down system: tar exits 2 under NoNewPrivileges=yes because
	//      it cannot open them. These are intentionally excluded — the deployed
	//      node regenerates them on first boot from PAM/sssd/shadow-utils.
	cmd := exec.Command("tar", //nolint:gosec
		"-C", rootfsPath,
		// Virtual/runtime filesystems
		"--exclude=./proc/*",
		"--exclude=./sys/*",
		"--exclude=./dev/*",
		"--exclude=./.clonr-state",
		// Shadow / credential files — intentionally 000 or 640 root:shadow.
		// tar exits 2 trying to read these under NoNewPrivileges=yes.
		// The deployed node regenerates shadow from passwd + firstboot config.
		"--exclude=./etc/shadow",
		"--exclude=./etc/shadow-",
		"--exclude=./etc/gshadow",
		"--exclude=./etc/gshadow-",
		"--exclude=./etc/security/opasswd",
		// sssd / nslcd runtime state — empty directories, rebuilt on boot.
		"--exclude=./var/lib/sss/*",
		"--exclude=./var/lib/nslcd/*",
		"--exclude=./var/log/sssd/*",
		// chrony log — world-unreadable on hardened installs.
		"--exclude=./var/log/chrony/*",
		// sudo binaries and helpers — SUID root, cannot be read by non-root tar
		// process running under NoNewPrivileges=yes.
		"--exclude=./usr/bin/sudo",
		"--exclude=./usr/bin/sudoreplay",
		"--exclude=./usr/libexec/sudo/sesh",
		// chrony state — world-unreadable on hardened installs.
		"--exclude=./var/lib/chrony/*",
		// Deterministic output flags — required for stable sha256 across repeated
		// streams of the same image content. Without these, entry order and embedded
		// timestamps vary between runs (directory readdir order, ctime drift),
		// producing different byte streams and therefore different hashes.
		//
		// --sort=name: emit entries in alphabetic order (POSIX-stable across runs).
		// --mtime: normalize all file timestamps to the UNIX epoch so a file whose
		//          mtime changed between streams doesn't invalidate the cached hash.
		//
		// Note: the sidecar hash is computed on the first stream and reused for all
		// subsequent streams. On server restart the sidecar file persists so the hash
		// survives across restarts without requiring a re-stream. If the sidecar is
		// missing, the next stream recomputes it.
		"--sort=name",
		"--mtime=1970-01-01 00:00:00",
		"-cf", "-",
		".",
	)
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = stderrBuf

	// Fail-fast: start tar and buffer the first 64 KB before writing HTTP
	// headers. If tar exits before filling the buffer (i.e. it failed
	// immediately with a fatal error), we can still return HTTP 500 rather
	// than committing to a 200 with a truncated body.
	// Once 64 KB is buffered we are confident tar is running correctly and we
	// commit to streaming: write headers and flush the buffer, then pipe the
	// remainder directly. After that point we cannot retroactively signal
	// failure to the client — but all the "known-bad" paths are excluded above.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Error().Err(err).Str("image_id", img.ID).Msg("blob stream: tar stdout pipe failed")
		writeError(w, err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Error().Err(err).Str("image_id", img.ID).Msg("blob stream: tar start failed")
		writeError(w, err)
		return
	}

	// Read up to 64 KB into a probe buffer. io.ReadFull returns io.EOF or
	// io.ErrUnexpectedEOF when fewer than 64 KB are available (i.e. tar exited
	// early). In that case check the process exit status; if non-zero, return
	// HTTP 500 — headers have not been written yet.
	const probeSize = 64 * 1024
	probeBuf := make([]byte, probeSize)
	probeN, probeErr := io.ReadFull(stdout, probeBuf)

	if probeErr == io.EOF || probeErr == io.ErrUnexpectedEOF {
		// Tar produced less than probeSize bytes — it may have finished cleanly
		// (tiny image) or died early. Check exit status.
		waitErr := cmd.Wait()
		if waitErr != nil {
			// Tar failed before streaming any meaningful data — safe to 500.
			log.Error().
				Err(waitErr).
				Str("image_id", img.ID).
				Str("stderr", stderrBuf.String()).
				Int("probe_bytes", probeN).
				Msg("blob stream: tar exited non-zero before headers were sent — returning HTTP 500")
			http.Error(w, "tar failed: "+stderrBuf.String(), http.StatusInternalServerError)
			return
		}
		// Tar finished cleanly with a small output — write headers and flush.
		if cachedTarChecksum != "" {
			w.Header().Set("X-Clonr-Blob-SHA256", cachedTarChecksum)
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetWriteDeadline(time.Time{})
		}
		w.WriteHeader(http.StatusOK)
		if _, writeErr := cw.Write(probeBuf[:probeN]); writeErr != nil {
			log.Warn().Err(writeErr).Str("image_id", img.ID).Msg("blob stream: write probe buffer (small tar)")
		}
		if computeTarChecksum {
			checksum := hex.EncodeToString(tarHasher.Sum(nil))
			saveTarChecksum(h.ImageDir, img.ID, checksum)
			log.Info().Str("image_id", img.ID).Str("tar_sha256", checksum).
				Msg("blob stream: tar checksum computed and cached (small tar, sidecar written)")
		}
		log.Info().Str("image_id", img.ID).Int64("bytes_written", baseWriter.n).
			Msg("blob stream: tar complete (small tar, headers sent after wait)")
		return
	}

	if probeErr != nil {
		// Unexpected read error — kill tar and return 500.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Error().Err(probeErr).Str("image_id", img.ID).Msg("blob stream: probe read error — aborting")
		http.Error(w, "blob stream probe failed", http.StatusInternalServerError)
		return
	}

	// We have a full probe buffer — tar is running. Commit to streaming now.
	// Write HTTP headers before flushing the probe buffer.
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
	if cachedTarChecksum != "" {
		w.Header().Set("X-Clonr-Blob-SHA256", cachedTarChecksum)
	}
	w.WriteHeader(http.StatusOK)

	// Disable the per-request write deadline for this handler — the response is a
	// large streaming tar archive and a global WriteTimeout would kill it mid-stream
	// on slow links. http.ResponseController is available since Go 1.20.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.SetWriteDeadline(time.Time{})
	}

	// Flush probe buffer through the counting/hashing writer.
	if _, writeErr := cw.Write(probeBuf[:probeN]); writeErr != nil {
		// Client disconnected immediately after headers — treat as disconnect.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		log.Info().Err(writeErr).Str("image_id", img.ID).Msg("blob stream: client disconnected after headers")
		return
	}

	// Stream the remainder of tar's stdout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Pipe remaining stdout through the counting/hashing writer.
	pipeRemaining := func() {
		if _, err := io.Copy(cw, stdout); err != nil {
			log.Warn().Err(err).Str("image_id", img.ID).Msg("blob stream: io.Copy interrupted")
		}
	}

	select {
	case <-r.Context().Done():
		// Client disconnected mid-stream — this is normal (e.g. agent restart,
		// network blip). Give tar 2 seconds to flush buffered output, then kill.
		timer := time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
		defer timer.Stop()
		<-done
		log.Info().
			Str("image_id", img.ID).
			Str("client", r.RemoteAddr).
			Int64("bytes_written", baseWriter.n).
			Msg("blob stream: client disconnected — cleanup complete")
		return
	default:
	}

	// Normal path: pipe remaining bytes synchronously, then wait.
	pipeRemaining()
	tarErr := <-done

	if tarErr != nil {
		// Real tar failure after headers committed — log stderr for diagnosis.
		// Client already received partial data; we cannot retroactively 500.
		log.Error().
			Err(tarErr).
			Str("image_id", img.ID).
			Str("stderr", stderrBuf.String()).
			Int64("bytes_written", baseWriter.n).
			Msg("blob stream: tar exited non-zero after streaming began — response may be truncated")
		return
	}

	log.Info().
		Str("image_id", img.ID).
		Str("client", r.RemoteAddr).
		Int64("bytes_written", baseWriter.n).
		Msg("blob stream: tar complete")

	// On the first successful stream, persist the tar checksum sidecar so
	// subsequent downloads can serve X-Clonr-Blob-SHA256 and clients can
	// verify end-to-end integrity.
	if computeTarChecksum {
		checksum := hex.EncodeToString(tarHasher.Sum(nil))
		saveTarChecksum(h.ImageDir, img.ID, checksum)
		log.Info().Str("image_id", img.ID).Str("tar_sha256", checksum).
			Msg("blob stream: tar checksum computed and cached (sidecar written)")
	}
}

// countWriter wraps an http.ResponseWriter and counts bytes written.
type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}
