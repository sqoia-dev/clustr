package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// NodesHandler handles all /api/v1/nodes routes.
type NodesHandler struct {
	DB *db.DB
}

// ListNodes handles GET /api/v1/nodes
func (h *NodesHandler) ListNodes(w http.ResponseWriter, r *http.Request) {
	baseImageID := r.URL.Query().Get("base_image_id")
	nodes, err := h.DB.ListNodeConfigs(r.Context(), baseImageID)
	if err != nil {
		log.Error().Err(err).Msg("list nodes")
		writeError(w, err)
		return
	}
	if nodes == nil {
		nodes = []api.NodeConfig{}
	}
	writeJSON(w, http.StatusOK, api.ListNodesResponse{Nodes: nodes, Total: len(nodes)})
}

// CreateNode handles POST /api/v1/nodes
func (h *NodesHandler) CreateNode(w http.ResponseWriter, r *http.Request) {
	var req api.CreateNodeConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	if req.Hostname == "" {
		writeValidationError(w, "hostname is required")
		return
	}
	if req.PrimaryMAC == "" {
		writeValidationError(w, "primary_mac is required")
		return
	}
	if req.BaseImageID == "" {
		writeValidationError(w, "base_image_id is required")
		return
	}

	now := time.Now().UTC()
	cfg := api.NodeConfig{
		ID:          uuid.New().String(),
		Hostname:    req.Hostname,
		FQDN:        req.FQDN,
		PrimaryMAC:  req.PrimaryMAC,
		Interfaces:  req.Interfaces,
		SSHKeys:     req.SSHKeys,
		KernelArgs:  req.KernelArgs,
		Groups:      req.Groups,
		CustomVars:  req.CustomVars,
		BaseImageID: req.BaseImageID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if cfg.Interfaces == nil {
		cfg.Interfaces = []api.InterfaceConfig{}
	}
	if cfg.SSHKeys == nil {
		cfg.SSHKeys = []string{}
	}
	if cfg.Groups == nil {
		cfg.Groups = []string{}
	}
	if cfg.CustomVars == nil {
		cfg.CustomVars = map[string]string{}
	}

	if err := h.DB.CreateNodeConfig(r.Context(), cfg); err != nil {
		log.Error().Err(err).Msg("create node config")
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, cfg)
}

// GetNode handles GET /api/v1/nodes/:id
func (h *NodesHandler) GetNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfg, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

// UpdateNode handles PUT /api/v1/nodes/:id
func (h *NodesHandler) UpdateNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req api.UpdateNodeConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	if req.Hostname == "" {
		writeValidationError(w, "hostname is required")
		return
	}
	if req.PrimaryMAC == "" {
		writeValidationError(w, "primary_mac is required")
		return
	}

	// Fetch to confirm existence.
	existing, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	cfg := api.NodeConfig{
		ID:          id,
		Hostname:    req.Hostname,
		FQDN:        req.FQDN,
		PrimaryMAC:  req.PrimaryMAC,
		Interfaces:  req.Interfaces,
		SSHKeys:     req.SSHKeys,
		KernelArgs:  req.KernelArgs,
		Groups:      req.Groups,
		CustomVars:  req.CustomVars,
		BaseImageID: req.BaseImageID,
		CreatedAt:   existing.CreatedAt,
		UpdatedAt:   time.Now().UTC(),
	}
	if cfg.Interfaces == nil {
		cfg.Interfaces = []api.InterfaceConfig{}
	}
	if cfg.SSHKeys == nil {
		cfg.SSHKeys = []string{}
	}
	if cfg.Groups == nil {
		cfg.Groups = []string{}
	}
	if cfg.CustomVars == nil {
		cfg.CustomVars = map[string]string{}
	}

	if err := h.DB.UpdateNodeConfig(r.Context(), cfg); err != nil {
		log.Error().Err(err).Msg("update node config")
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, cfg)
}

// DeleteNode handles DELETE /api/v1/nodes/:id
func (h *NodesHandler) DeleteNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.DB.DeleteNodeConfig(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetNodeByMAC handles GET /api/v1/nodes/by-mac/:mac
func (h *NodesHandler) GetNodeByMAC(w http.ResponseWriter, r *http.Request) {
	mac := chi.URLParam(r, "mac")
	cfg, err := h.DB.GetNodeConfigByMAC(r.Context(), mac)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}
