package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// ImagesHandler handles all /api/v1/images routes.
type ImagesHandler struct {
	DB       *db.DB
	ImageDir string
	// Progress is used by DeleteImage to check for active deploys.
	// It is optional — when nil, the active-deploy guard is skipped.
	Progress ProgressStoreIface
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

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, filepath.Base(blobPath), stat.ModTime(), f)
}

// streamFilesystemBlob tars the rootfs/ directory of a filesystem-format image
// and streams it directly to the response writer as an uncompressed tar archive.
// No Content-Length is set — the response is streamed.
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

	log.Info().
		Str("image_id", img.ID).
		Str("format", string(img.Format)).
		Str("client", r.RemoteAddr).
		Msg("image blob streamed")

	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.tar"`, img.ID))
	w.WriteHeader(http.StatusOK)

	cmd := exec.CommandContext(r.Context(), "tar",
		"-C", rootfsPath,
		"--exclude=./proc/*",
		"--exclude=./sys/*",
		"--exclude=./dev/*",
		"--exclude=./.clonr-state",
		"-cf", "-",
		".",
	)
	cmd.Stdout = w

	var bytesWritten int64
	// Wrap the response writer to count bytes streamed.
	cw := &countWriter{w: w}
	cmd.Stdout = cw

	if err := cmd.Run(); err != nil {
		bytesWritten = cw.n
		log.Error().
			Err(err).
			Str("image_id", img.ID).
			Int64("bytes_written", bytesWritten).
			Msg("blob stream: tar exited non-zero — response may be truncated")
		return
	}

	bytesWritten = cw.n
	log.Info().
		Str("image_id", img.ID).
		Str("client", r.RemoteAddr).
		Int64("bytes_written", bytesWritten).
		Msg("blob stream: tar complete")
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
