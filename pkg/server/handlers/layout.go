package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/hardware"
	"github.com/sqoia-dev/clonr/pkg/image/layout"
)

// LayoutHandler handles layout recommendation, validation, and override endpoints.
type LayoutHandler struct {
	DB *db.DB
}

// GetLayoutRecommendation handles GET /api/v1/nodes/:id/layout-recommendation.
// Returns a hardware-aware DiskLayout recommendation for the node, based on its
// stored hardware profile. The recommendation includes human-readable reasoning
// so the admin can evaluate it before applying.
func (h *LayoutHandler) GetLayoutRecommendation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	if len(node.HardwareProfile) == 0 {
		writeValidationError(w, "node has no hardware profile — hardware is discovered on first PXE boot")
		return
	}

	// Parse the stored hardware profile JSON into a SystemInfo.
	var hw hardware.SystemInfo
	if err := json.Unmarshal(node.HardwareProfile, &hw); err != nil {
		log.Error().Err(err).Str("node_id", id).Msg("parse hardware profile for layout recommendation")
		writeError(w, fmt.Errorf("cannot parse hardware profile: %w", err))
		return
	}

	// Determine image format for the recommendation (affects whether we need /boot etc).
	imageFormat := string(api.ImageFormatFilesystem)
	if node.BaseImageID != "" {
		img, imgErr := h.DB.GetBaseImage(r.Context(), node.BaseImageID)
		if imgErr == nil {
			imageFormat = string(img.Format)
		}
	}

	rec, err := layout.Recommend(hw, imageFormat)
	if err != nil {
		log.Error().Err(err).Str("node_id", id).Msg("layout recommendation failed")
		writeValidationError(w, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, api.LayoutRecommendation{
		Layout:    rec.Layout,
		Reasoning: rec.Reasoning,
		Warnings:  rec.Warnings,
	})
}

// GetEffectiveLayout handles GET /api/v1/nodes/:id/effective-layout.
// Returns the resolved DiskLayout that will be used for the next deployment,
// including the source level (node / group / image).
func (h *LayoutHandler) GetEffectiveLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	var img *api.BaseImage
	if node.BaseImageID != "" {
		fetched, imgErr := h.DB.GetBaseImage(r.Context(), node.BaseImageID)
		if imgErr == nil {
			img = &fetched
		}
	}

	var group *api.NodeGroup
	if node.GroupID != "" {
		fetched, gErr := h.DB.GetNodeGroup(r.Context(), node.GroupID)
		if gErr == nil {
			group = &fetched
		}
	}

	effective := node.EffectiveLayout(img, group)
	source := node.EffectiveLayoutSource(img, group)

	resp := api.EffectiveLayoutResponse{
		Layout:  effective,
		Source:  source,
		GroupID: node.GroupID,
	}
	if img != nil {
		resp.ImageID = img.ID
	}

	writeJSON(w, http.StatusOK, resp)
}

// GetEffectiveMounts handles GET /api/v1/nodes/:id/effective-mounts.
// Returns the merged fstab entries that will be applied on the next deployment,
// annotated with their source (node-level or group-level).
func (h *LayoutHandler) GetEffectiveMounts(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	var group *api.NodeGroup
	if node.GroupID != "" {
		fetched, gErr := h.DB.GetNodeGroup(r.Context(), node.GroupID)
		if gErr == nil {
			group = &fetched
		}
	}

	// Build annotated entries showing provenance.
	resp := api.EffectiveMountsResponse{
		NodeID:  node.ID,
		GroupID: node.GroupID,
	}

	// Start with group mounts.
	if group != nil {
		for _, m := range group.ExtraMounts {
			resp.Mounts = append(resp.Mounts, api.EffectiveMountEntry{
				FstabEntry: m,
				Source:     "group",
				GroupID:    group.ID,
			})
		}
	}
	// Apply node overrides / additions.
	seen := map[string]int{}
	for i, e := range resp.Mounts {
		seen[e.MountPoint] = i
	}
	for _, m := range node.ExtraMounts {
		if idx, exists := seen[m.MountPoint]; exists {
			resp.Mounts[idx] = api.EffectiveMountEntry{
				FstabEntry: m,
				Source:     "node",
			}
		} else {
			resp.Mounts = append(resp.Mounts, api.EffectiveMountEntry{
				FstabEntry: m,
				Source:     "node",
			})
		}
	}
	if resp.Mounts == nil {
		resp.Mounts = []api.EffectiveMountEntry{}
	}

	writeJSON(w, http.StatusOK, resp)
}

// SetNodeLayoutOverride handles PUT /api/v1/nodes/:id/layout-override.
// Stores a node-level DiskLayout override. Send an empty partitions array or
// set clear_layout_override=true to remove the override.
func (h *LayoutHandler) SetNodeLayoutOverride(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req struct {
		Layout              *api.DiskLayout `json:"layout"`
		ClearLayoutOverride bool            `json:"clear_layout_override"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// Confirm node exists.
	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	var newOverride *api.DiskLayout
	if !req.ClearLayoutOverride && req.Layout != nil && len(req.Layout.Partitions) > 0 {
		// Validate before saving.
		result := layout.Validate(*req.Layout, hardware.Disk{})
		if !result.Valid {
			writeJSON(w, http.StatusUnprocessableEntity, api.LayoutValidationResponse{
				Valid:    false,
				Errors:   result.Errors,
				Warnings: result.Warnings,
			})
			return
		}
		newOverride = req.Layout
	}
	// else: clear override (newOverride stays nil)

	if err := h.DB.SetNodeLayoutOverride(r.Context(), id, newOverride); err != nil {
		log.Error().Err(err).Str("node_id", id).Msg("set node layout override")
		writeError(w, err)
		return
	}

	// Return the updated node.
	node.DiskLayoutOverride = newOverride
	writeJSON(w, http.StatusOK, sanitizeNodeConfig(node))
}

// ValidateLayout handles POST /api/v1/nodes/:id/layout/validate.
// Validates a DiskLayout against the node's discovered hardware.
func (h *LayoutHandler) ValidateLayout(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req api.LayoutValidationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}

	// Extract target disk from hardware profile for size checking.
	targetDisk := hardware.Disk{}
	var ramKB uint64
	if len(node.HardwareProfile) > 0 {
		var hw hardware.SystemInfo
		if parseErr := json.Unmarshal(node.HardwareProfile, &hw); parseErr == nil {
			ramKB = hw.Memory.TotalKB
			// Pick the first non-boot disk as the target for validation.
			for _, d := range hw.Disks {
				if !isBoot(d) {
					targetDisk = d
					break
				}
			}
		}
	}

	result := layout.ValidateWithRAM(req.Layout, targetDisk, ramKB)
	writeJSON(w, http.StatusOK, api.LayoutValidationResponse{
		Valid:    result.Valid,
		Errors:   result.Errors,
		Warnings: result.Warnings,
	})
}

// AssignNodeGroup handles PUT /api/v1/nodes/:id/group.
func (h *LayoutHandler) AssignNodeGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req api.AssignGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "invalid JSON body")
		return
	}

	// If a group ID is specified, confirm it exists.
	if req.GroupID != "" {
		if _, err := h.DB.GetNodeGroup(r.Context(), req.GroupID); err != nil {
			writeError(w, err)
			return
		}
	}

	if err := h.DB.AssignNodeToGroup(r.Context(), id, req.GroupID); err != nil {
		log.Error().Err(err).Str("node_id", id).Str("group_id", req.GroupID).Msg("assign node to group")
		writeError(w, err)
		return
	}

	node, err := h.DB.GetNodeConfig(r.Context(), id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sanitizeNodeConfig(node))
}

// isBoot returns true if any partition on the disk is mounted at "/" or "/boot".
func isBoot(d hardware.Disk) bool {
	for _, p := range d.Partitions {
		mp := p.MountPoint
		if mp == "/" || mp == "/boot" || mp == "/boot/efi" {
			return true
		}
	}
	return false
}
