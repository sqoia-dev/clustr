package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// BuildProgressStoreIface is the interface the handler needs from BuildProgressStore.
// Defined here (using api types only) to avoid an import cycle with pkg/server.
type BuildProgressStoreIface interface {
	Get(imageID string) (api.BuildState, bool)
	Subscribe() (<-chan api.BuildEvent, func())
}

// BuildProgressHandler handles all /api/v1/images/:id/build-progress routes.
type BuildProgressHandler struct {
	Store    BuildProgressStoreIface
	ImageDir string
}

// GetBuildProgress handles GET /api/v1/images/:id/build-progress
// Returns the current BuildState snapshot for the given image.
func (h *BuildProgressHandler) GetBuildProgress(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")
	state, ok := h.Store.Get(imageID)
	if !ok {
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "no build progress found for image", Code: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, state)
}

// StreamBuildProgress handles GET /api/v1/images/:id/build-progress/stream
// Streams BuildEvents for the given image as Server-Sent Events.
func (h *BuildProgressHandler) StreamBuildProgress(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	ch, cancel := h.Store.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Send the current snapshot immediately so the browser doesn't need a
	// separate GET before opening the stream.
	if state, ok := h.Store.Get(imageID); ok {
		data, err := json.Marshal(state)
		if err == nil {
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-ch:
			if !open {
				return
			}
			// Filter: only forward events for this image.
			if ev.ImageID != imageID {
				continue
			}
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// GetBuildLog handles GET /api/v1/images/:id/build-log
// Serves the persisted serial console log as text/plain.
func (h *BuildProgressHandler) GetBuildLog(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")
	logPath := filepath.Join(h.ImageDir, imageID, "build.log")

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "build log not available (image may not be an ISO build or build hasn't completed)", Code: "not_found"})
			return
		}
		log.Error().Err(err).Str("path", logPath).Msg("build progress: open build log")
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to open build log", Code: "internal_error"})
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		http.Error(w, "stat failed", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"build-%s.log\"", imageID))
	http.ServeContent(w, r, "build.log", stat.ModTime(), f)
}

// GetBuildManifest handles GET /api/v1/images/:id/build-manifest
// Serves the JSON build summary written at the end of a build.
func (h *BuildProgressHandler) GetBuildManifest(w http.ResponseWriter, r *http.Request) {
	imageID := chi.URLParam(r, "id")
	manifestPath := filepath.Join(h.ImageDir, imageID, "build.json")

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "build manifest not available", Code: "not_found"})
			return
		}
		log.Error().Err(err).Str("path", manifestPath).Msg("build progress: open build manifest")
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to open build manifest", Code: "internal_error"})
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(data)
}
