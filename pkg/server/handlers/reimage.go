// Package handlers — reimage.go implements the reimage workflow API endpoints.
//
// Routes (all under /api/v1, bearer-auth required):
//
//	POST   /nodes/{id}/reimage      — queue or immediately trigger a reimage
//	GET    /nodes/{id}/reimage      — list reimage history for a node
//	GET    /reimage/{id}            — get a single reimage request by ID
//	DELETE /reimage/{id}            — cancel a pending scheduled reimage
//	POST   /reimage/{id}/retry      — retry a failed reimage
package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/reimage"
)

// ReimageHandler handles all /api/v1/nodes/{id}/reimage and /api/v1/reimage routes.
type ReimageHandler struct {
	DB           *db.DB
	Orchestrator *reimage.Orchestrator
}

// ─── POST /api/v1/nodes/{id}/reimage ─────────────────────────────────────────

// Create handles a new reimage request.
// Immediate reimages (scheduled_at == nil) are triggered synchronously and
// return 200 with the resulting request record.
// Scheduled reimages return 202 Accepted with the request record.
func (h *ReimageHandler) Create(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")

	var body api.CreateReimageRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// Load and validate the node.
	node, err := h.DB.GetNodeConfig(r.Context(), nodeID)
	if err != nil {
		writeError(w, err)
		return
	}

	// Resolve the target image ID — either explicit in the request or from the
	// node's current base_image_id.
	imageID := body.ImageID
	if imageID == "" {
		imageID = node.BaseImageID
	}
	if imageID == "" {
		writeValidationError(w, "image_id is required (node has no base_image_id assigned)")
		return
	}

	if !body.Force {
		// Pre-check 1: target image must exist and be ready.
		img, err := h.DB.GetBaseImage(r.Context(), imageID)
		if err != nil {
			writeError(w, err)
			return
		}
		if img.Status != api.ImageStatusReady {
			writeJSON(w, http.StatusConflict, api.ErrorResponse{
				Error: fmt.Sprintf("image %q is not ready (status: %s) — use force=true to skip this check", imageID, img.Status),
				Code:  "image_not_ready",
			})
			return
		}

		// Pre-check 2: no active (non-terminal) reimage already in flight.
		active, err := h.DB.GetActiveReimageForNode(r.Context(), nodeID)
		if err != nil {
			log.Error().Err(err).Str("node_id", nodeID).Msg("reimage create: check active")
			writeError(w, err)
			return
		}
		if active != nil {
			writeJSON(w, http.StatusConflict, api.ErrorResponse{
				Error: fmt.Sprintf("node already has an active reimage request %q (status: %s) — cancel it first or use force=true", active.ID, active.Status),
				Code:  "reimage_active",
			})
			return
		}
	}

	// Build and persist the request record.
	status := api.ReimageStatusPending
	req := api.ReimageRequest{
		ID:          uuid.New().String(),
		NodeID:      nodeID,
		ImageID:     imageID,
		Status:      status,
		ScheduledAt: body.ScheduledAt,
		ErrorMessage: "",
		RequestedBy: "api",
		DryRun:      body.DryRun,
		CreatedAt:   time.Now().UTC(),
	}

	if err := h.DB.CreateReimageRequest(r.Context(), req); err != nil {
		log.Error().Err(err).Str("node_id", nodeID).Msg("reimage create: db insert")
		writeError(w, err)
		return
	}

	log.Info().
		Str("req_id", req.ID).
		Str("node_id", nodeID).
		Str("image_id", imageID).
		Bool("dry_run", req.DryRun).
		Bool("scheduled", req.ScheduledAt != nil).
		Msg("reimage request created")

	// Scheduled reimage — return 202 and let the scheduler goroutine fire it.
	if body.ScheduledAt != nil {
		writeJSON(w, http.StatusAccepted, req)
		return
	}

	// Immediate reimage — trigger synchronously.
	if err := h.Orchestrator.Trigger(r.Context(), req.ID); err != nil {
		log.Error().Err(err).Str("req_id", req.ID).Msg("reimage create: trigger failed")
		// Reload the request so the caller sees the "failed" status.
		updated, dbErr := h.DB.GetReimageRequest(r.Context(), req.ID)
		if dbErr == nil {
			writeJSON(w, http.StatusBadGateway, updated)
		} else {
			writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
				Error: fmt.Sprintf("reimage trigger failed: %v", err),
				Code:  "reimage_trigger_failed",
			})
		}
		return
	}

	// Reload to pick up triggered_at and updated status.
	final, err := h.DB.GetReimageRequest(r.Context(), req.ID)
	if err != nil {
		final = req // best effort
	}
	writeJSON(w, http.StatusOK, final)
}

// ─── GET /api/v1/nodes/{id}/reimage ──────────────────────────────────────────

// ListForNode handles GET /api/v1/nodes/{id}/reimage.
func (h *ReimageHandler) ListForNode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")

	// Confirm node exists.
	if _, err := h.DB.GetNodeConfig(r.Context(), nodeID); err != nil {
		writeError(w, err)
		return
	}

	reqs, err := h.DB.ListReimageRequests(r.Context(), nodeID)
	if err != nil {
		log.Error().Err(err).Str("node_id", nodeID).Msg("reimage list")
		writeError(w, err)
		return
	}
	if reqs == nil {
		reqs = []api.ReimageRequest{}
	}
	writeJSON(w, http.StatusOK, api.ListReimagesResponse{Requests: reqs, Total: len(reqs)})
}

// ─── GET /api/v1/reimage/{id} ─────────────────────────────────────────────────

// Get handles GET /api/v1/reimage/{id}.
func (h *ReimageHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	req, err := h.DB.GetReimageRequest(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// ─── DELETE /api/v1/reimage/{id} ─────────────────────────────────────────────

// Cancel handles DELETE /api/v1/reimage/{id}.
// Only requests in "pending" status can be canceled.
func (h *ReimageHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	req, err := h.DB.GetReimageRequest(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	if req.Status != api.ReimageStatusPending {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: fmt.Sprintf("cannot cancel reimage in status %q — only pending requests can be canceled", req.Status),
			Code:  "not_cancelable",
		})
		return
	}

	if err := h.DB.UpdateReimageRequestStatus(r.Context(), id, api.ReimageStatusCanceled, "canceled by operator"); err != nil {
		log.Error().Err(err).Str("req_id", id).Msg("reimage cancel")
		writeError(w, err)
		return
	}

	log.Info().Str("req_id", id).Str("node_id", req.NodeID).Msg("reimage request canceled")
	w.WriteHeader(http.StatusNoContent)
}

// ─── POST /api/v1/reimage/{id}/retry ─────────────────────────────────────────

// Retry handles POST /api/v1/reimage/{id}/retry.
// Creates a new reimage request cloned from the failed one and triggers it
// immediately. Only requests in "failed" status can be retried.
func (h *ReimageHandler) Retry(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	orig, err := h.DB.GetReimageRequest(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	if orig.Status != api.ReimageStatusFailed {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: fmt.Sprintf("cannot retry reimage in status %q — only failed requests can be retried", orig.Status),
			Code:  "not_retryable",
		})
		return
	}

	// Check for an already-active reimage on this node.
	active, err := h.DB.GetActiveReimageForNode(r.Context(), orig.NodeID)
	if err != nil {
		log.Error().Err(err).Str("node_id", orig.NodeID).Msg("reimage retry: check active")
		writeError(w, err)
		return
	}
	if active != nil && active.ID != id {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{
			Error: fmt.Sprintf("node already has an active reimage request %q (status: %s)", active.ID, active.Status),
			Code:  "reimage_active",
		})
		return
	}

	// Create a new request cloned from the original.
	newReq := api.ReimageRequest{
		ID:          uuid.New().String(),
		NodeID:      orig.NodeID,
		ImageID:     orig.ImageID,
		Status:      api.ReimageStatusPending,
		DryRun:      orig.DryRun,
		RequestedBy: "api-retry",
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.DB.CreateReimageRequest(r.Context(), newReq); err != nil {
		log.Error().Err(err).Str("orig_id", id).Msg("reimage retry: create")
		writeError(w, err)
		return
	}

	if err := h.Orchestrator.Trigger(r.Context(), newReq.ID); err != nil {
		log.Error().Err(err).Str("req_id", newReq.ID).Msg("reimage retry: trigger failed")
		updated, dbErr := h.DB.GetReimageRequest(r.Context(), newReq.ID)
		if dbErr == nil {
			writeJSON(w, http.StatusBadGateway, updated)
		} else {
			writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
				Error: fmt.Sprintf("reimage trigger failed: %v", err),
				Code:  "reimage_trigger_failed",
			})
		}
		return
	}

	final, err := h.DB.GetReimageRequest(r.Context(), newReq.ID)
	if err != nil {
		final = newReq
	}
	writeJSON(w, http.StatusOK, final)
}
