package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// LogBroker is the interface the handler needs from the broker — keeps the
// handler package free of a concrete import cycle.
type LogBroker interface {
	Subscribe(filter api.LogFilter) (id string, ch <-chan api.LogEntry, cancel func())
	Publish(entries []api.LogEntry)
}

// LogsHandler handles all /api/v1/logs routes.
type LogsHandler struct {
	DB     *db.DB
	Broker LogBroker
}

// IngestLogs handles POST /api/v1/logs
// Accepts a JSON array of LogEntry objects and persists them.
func (h *LogsHandler) IngestLogs(w http.ResponseWriter, r *http.Request) {
	const maxLogsBodyBytes = 5 << 20 // 5 MiB
	r.Body = http.MaxBytesReader(w, r.Body, maxLogsBodyBytes)

	var entries []api.LogEntry
	if err := json.NewDecoder(r.Body).Decode(&entries); err != nil {
		if err.Error() == "http: request body too large" {
			http.Error(w, "request body too large (max 5MB)", http.StatusRequestEntityTooLarge)
			return
		}
		writeValidationError(w, "invalid JSON body: expected array of log entries")
		return
	}
	if len(entries) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if len(entries) > 500 {
		writeValidationError(w, "batch too large: max 500 entries per request")
		return
	}

	// Validate required fields.
	for i, e := range entries {
		if e.ID == "" {
			writeValidationError(w, fmt.Sprintf("entry[%d]: id is required", i))
			return
		}
		if e.NodeMAC == "" {
			writeValidationError(w, fmt.Sprintf("entry[%d]: node_mac is required", i))
			return
		}
		if e.Level == "" {
			writeValidationError(w, fmt.Sprintf("entry[%d]: level is required", i))
			return
		}
		if e.Message == "" {
			writeValidationError(w, fmt.Sprintf("entry[%d]: message is required", i))
			return
		}
		if e.Timestamp.IsZero() {
			entries[i].Timestamp = time.Now().UTC()
		}
	}

	if err := h.DB.InsertLogBatch(r.Context(), entries); err != nil {
		log.Error().Err(err).Msg("ingest logs")
		writeError(w, err)
		return
	}

	// Publish to SSE subscribers after persisting — best-effort.
	h.Broker.Publish(entries)

	w.WriteHeader(http.StatusCreated)
}

// QueryLogs handles GET /api/v1/logs
// Query params: mac, hostname, level, component, since, limit
func (h *LogsHandler) QueryLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := api.LogFilter{
		NodeMAC:   q.Get("mac"),
		Hostname:  q.Get("hostname"),
		Level:     q.Get("level"),
		Component: q.Get("component"),
	}

	if sinceStr := q.Get("since"); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			writeValidationError(w, "invalid 'since' param: must be RFC3339 (e.g. 2024-01-01T00:00:00Z)")
			return
		}
		filter.Since = &t
	}

	if limitStr := q.Get("limit"); limitStr != "" {
		n, err := strconv.Atoi(limitStr)
		if err != nil || n <= 0 {
			writeValidationError(w, "invalid 'limit' param: must be a positive integer")
			return
		}
		filter.Limit = n
	}

	entries, err := h.DB.QueryLogs(r.Context(), filter)
	if err != nil {
		log.Error().Err(err).Msg("query logs")
		writeError(w, err)
		return
	}
	if entries == nil {
		entries = []api.LogEntry{}
	}
	writeJSON(w, http.StatusOK, api.ListLogsResponse{Logs: entries, Total: len(entries)})
}

// StreamLogs handles GET /api/v1/logs/stream
// Streams new log entries as Server-Sent Events.
// Optional query params: mac, hostname, level, component
func (h *LogsHandler) StreamLogs(w http.ResponseWriter, r *http.Request) {
	// Verify the client supports SSE.
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported by this server", http.StatusInternalServerError)
		return
	}

	q := r.URL.Query()
	filter := api.LogFilter{
		NodeMAC:   q.Get("mac"),
		Hostname:  q.Get("hostname"),
		Level:     q.Get("level"),
		Component: q.Get("component"),
	}

	_, ch, cancel := h.Broker.Subscribe(filter)
	defer cancel()

	// SSE headers — must be set before first write.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering if present
	w.WriteHeader(http.StatusOK)

	// Send a comment to establish the stream before any data arrives.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected — cancel() in defer handles cleanup.
			return
		case entry, open := <-ch:
			if !open {
				return
			}
			data, err := json.Marshal(entry)
			if err != nil {
				continue // skip unserializable entries
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
