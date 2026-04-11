package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// FactoryHandler handles image ingest operations.
type FactoryHandler struct {
	DB       *db.DB
	ImageDir string
}

// Pull handles POST /api/v1/factory/pull
// Downloads an image from a URL, stores it as a blob, and creates a BaseImage record.
func (h *FactoryHandler) Pull(w http.ResponseWriter, r *http.Request) {
	var req api.PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	if req.URL == "" {
		writeValidationError(w, "url is required")
		return
	}
	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}
	if req.Format == "" {
		req.Format = api.ImageFormatFilesystem
	}

	id := uuid.New().String()
	img := api.BaseImage{
		ID:        id,
		Name:      req.Name,
		Version:   req.Version,
		OS:        req.OS,
		Arch:      req.Arch,
		Status:    api.ImageStatusBuilding,
		Format:    req.Format,
		DiskLayout: req.DiskLayout,
		Tags:      req.Tags,
		SourceURL: req.URL,
		Notes:     req.Notes,
		CreatedAt: time.Now().UTC(),
	}
	if img.Tags == nil {
		img.Tags = []string{}
	}

	if err := h.DB.CreateBaseImage(r.Context(), img); err != nil {
		log.Error().Err(err).Msg("factory pull: create image record")
		writeError(w, err)
		return
	}

	// Pull runs asynchronously so we can return immediately.
	go h.pullAsync(id, req.URL)

	writeJSON(w, http.StatusAccepted, img)
}

// pullAsync downloads the image blob from url and finalizes the DB record.
// Runs in a goroutine; uses a background context since the HTTP request is gone.
func (h *FactoryHandler) pullAsync(imageID, url string) {
	ctx := context.Background()
	log.Info().Str("image_id", imageID).Str("url", url).Msg("factory pull started")

	if err := h.DB.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusBuilding, ""); err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("factory pull: set building status")
		return
	}

	sizeBytes, err := h.downloadBlob(ctx, imageID, url)
	if err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("factory pull: download failed")
		_ = h.DB.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	if err := h.DB.FinalizeBaseImage(ctx, imageID, sizeBytes, ""); err != nil {
		log.Error().Err(err).Str("image_id", imageID).Msg("factory pull: finalize failed")
		_ = h.DB.UpdateBaseImageStatus(ctx, imageID, api.ImageStatusError, err.Error())
		return
	}

	log.Info().Str("image_id", imageID).Int64("size_bytes", sizeBytes).Msg("factory pull complete")
}

// downloadBlob streams url to disk under ImageDir/<imageID>.blob and returns the byte count.
func (h *FactoryHandler) downloadBlob(ctx context.Context, imageID, url string) (int64, error) {
	if err := os.MkdirAll(h.ImageDir, 0o755); err != nil {
		return 0, fmt.Errorf("create image dir: %w", err)
	}

	blobPath := filepath.Join(h.ImageDir, imageID+".blob")
	f, err := os.Create(blobPath)
	if err != nil {
		return 0, fmt.Errorf("create blob file: %w", err)
	}
	defer f.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		_ = os.Remove(blobPath)
		return 0, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = os.Remove(blobPath)
		return 0, fmt.Errorf("download: HTTP %d from %s", resp.StatusCode, url)
	}

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		_ = os.Remove(blobPath)
		return 0, fmt.Errorf("write blob: %w", err)
	}

	if err := h.DB.SetBlobPath(ctx, imageID, blobPath); err != nil {
		return 0, fmt.Errorf("set blob path: %w", err)
	}

	return n, nil
}
