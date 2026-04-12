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
)

// NodeGroupsHandler handles all /api/v1/node-groups routes.
type NodeGroupsHandler struct {
	DB *db.DB
}

// ListNodeGroups handles GET /api/v1/node-groups.
func (h *NodeGroupsHandler) ListNodeGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.DB.ListNodeGroups(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("list node groups")
		writeError(w, err)
		return
	}
	if groups == nil {
		groups = []api.NodeGroup{}
	}
	writeJSON(w, http.StatusOK, api.ListNodeGroupsResponse{Groups: groups, Total: len(groups)})
}

// CreateNodeGroup handles POST /api/v1/node-groups.
func (h *NodeGroupsHandler) CreateNodeGroup(w http.ResponseWriter, r *http.Request) {
	var req api.CreateNodeGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}
	for i, m := range req.ExtraMounts {
		if err := api.ValidateFstabEntry(m); err != nil {
			writeValidationError(w, fmt.Sprintf("extra_mounts[%d]: %s", i, err.Error()))
			return
		}
	}

	now := time.Now().UTC()
	g := api.NodeGroup{
		ID:                 uuid.New().String(),
		Name:               req.Name,
		Description:        req.Description,
		DiskLayoutOverride: req.DiskLayoutOverride,
		ExtraMounts:        req.ExtraMounts,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if err := h.DB.CreateNodeGroup(r.Context(), g); err != nil {
		log.Error().Err(err).Msg("create node group")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, g)
}

// GetNodeGroup handles GET /api/v1/node-groups/:id.
func (h *NodeGroupsHandler) GetNodeGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	g, err := h.DB.GetNodeGroup(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// UpdateNodeGroup handles PUT /api/v1/node-groups/:id.
func (h *NodeGroupsHandler) UpdateNodeGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req api.UpdateNodeGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}
	if req.Name == "" {
		writeValidationError(w, "name is required")
		return
	}

	// Confirm existence.
	existing, err := h.DB.GetNodeGroup(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	for i, m := range req.ExtraMounts {
		if err := api.ValidateFstabEntry(m); err != nil {
			writeValidationError(w, fmt.Sprintf("extra_mounts[%d]: %s", i, err.Error()))
			return
		}
	}

	override := req.DiskLayoutOverride
	if req.ClearLayoutOverride {
		override = nil
	}

	g := api.NodeGroup{
		ID:                 id,
		Name:               req.Name,
		Description:        req.Description,
		DiskLayoutOverride: override,
		ExtraMounts:        req.ExtraMounts,
		CreatedAt:          existing.CreatedAt,
		UpdatedAt:          time.Now().UTC(),
	}
	if err := h.DB.UpdateNodeGroup(r.Context(), g); err != nil {
		log.Error().Err(err).Msg("update node group")
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

// DeleteNodeGroup handles DELETE /api/v1/node-groups/:id.
func (h *NodeGroupsHandler) DeleteNodeGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.DB.DeleteNodeGroup(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
