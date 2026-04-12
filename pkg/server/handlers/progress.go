package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
)

// ProgressStoreIface is the interface the handler needs from ProgressStore.
// Defined here to keep the handlers package free of a concrete import cycle.
type ProgressStoreIface interface {
	Update(entry api.DeployProgress)
	Get(nodeMAC string) (*api.DeployProgress, bool)
	List() []api.DeployProgress
	Subscribe() (ch <-chan api.DeployProgress, cancel func())
}

// ProgressHandler handles all /api/v1/deploy/progress routes.
type ProgressHandler struct {
	Store ProgressStoreIface
}

// ListProgress handles GET /api/v1/deploy/progress
// Returns a JSON snapshot of all active deployments.
func (h *ProgressHandler) ListProgress(w http.ResponseWriter, r *http.Request) {
	entries := h.Store.List()
	if entries == nil {
		entries = []api.DeployProgress{}
	}
	writeJSON(w, http.StatusOK, entries)
}

// GetProgress handles GET /api/v1/deploy/progress/:mac
// Returns the latest progress for a single node.
func (h *ProgressHandler) GetProgress(w http.ResponseWriter, r *http.Request) {
	mac := chi.URLParam(r, "mac")
	entry, ok := h.Store.Get(mac)
	if !ok {
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "no progress found for node", Code: "not_found"})
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// IngestProgress handles POST /api/v1/deploy/progress
// Accepts a DeployProgress from the client, stores it, and fans out to SSE subscribers.
func (h *ProgressHandler) IngestProgress(w http.ResponseWriter, r *http.Request) {
	const maxBody = 64 * 1024 // 64 KiB
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var entry api.DeployProgress
	if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
		writeValidationError(w, "invalid JSON body: "+err.Error())
		return
	}
	if entry.NodeMAC == "" {
		writeValidationError(w, "node_mac is required")
		return
	}
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = time.Now().UTC()
	}

	h.Store.Update(entry)

	log.Debug().
		Str("mac", entry.NodeMAC).
		Str("phase", entry.Phase).
		Int64("bytes_done", entry.BytesDone).
		Int64("bytes_total", entry.BytesTotal).
		Msg("deploy progress ingested")

	w.WriteHeader(http.StatusOK)
}

// StreamProgress handles GET /api/v1/deploy/progress/stream
// Streams DeployProgress updates as Server-Sent Events.
func (h *ProgressHandler) StreamProgress(w http.ResponseWriter, r *http.Request) {
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

	// Initial snapshot — send all current states so the UI doesn't have to
	// make a separate GET on connect.
	for _, entry := range h.Store.List() {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "data: %s\n\n", data)
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
