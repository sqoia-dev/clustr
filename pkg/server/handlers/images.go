package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
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

// ArchiveImage handles DELETE /api/v1/images/:id
func (h *ImagesHandler) ArchiveImage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.DB.ArchiveBaseImage(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
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
// Streams the blob file to the client, supporting range requests via http.ServeContent.
func (h *ImagesHandler) DownloadBlob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

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
