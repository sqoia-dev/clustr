// Package handlers — power.go implements POST /api/v1/nodes/{id}/power/flip-to-disk.
//
// This endpoint is called by the clonr client after a successful deploy to tell
// the server to instruct the node's power provider to set the next boot device
// to disk. An optional ?cycle=true query parameter also power-cycles the node
// so it reboots immediately into the deployed OS.
package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/power"
)

// flipToDiskTimeout caps the combined SetNextBoot + optional PowerCycle call.
const flipToDiskTimeout = 30 * time.Second

// PowerHandler handles power provider endpoints that go through the provider
// abstraction (not raw IPMI). Currently: POST /nodes/{id}/power/flip-to-disk.
type PowerHandler struct {
	DB       *db.DB
	Registry *power.Registry
}

// FlipToDisk handles POST /api/v1/nodes/{id}/power/flip-to-disk.
//
// It looks up the node, creates the configured power provider, and calls
// SetNextBoot(ctx, power.BootDisk). If ?cycle=true is set in the query string,
// it also calls PowerCycle so the node reboots into the deployed OS without
// manual intervention.
func (h *PowerHandler) FlipToDisk(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")
	doCycle := r.URL.Query().Get("cycle") == "true"

	cfg, err := h.DB.GetNodeConfig(r.Context(), nodeID)
	if err != nil {
		writeError(w, err)
		return
	}

	if cfg.PowerProvider == nil || cfg.PowerProvider.Type == "" {
		// Fall back to legacy IPMI if BMC is configured — keeps existing behaviour.
		if cfg.BMC != nil && cfg.BMC.IPAddress != "" {
			log.Info().Str("node_id", nodeID).Msg("power: no provider configured, falling back to IPMI")
			writeJSON(w, http.StatusUnprocessableEntity, api.ErrorResponse{
				Error: "no power provider configured — use the IPMI Boot-to-Disk button or configure a power provider",
				Code:  "no_power_provider",
			})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, api.ErrorResponse{
			Error: "no power provider configured for this node",
			Code:  "no_power_provider",
		})
		return
	}

	provCfg := power.ProviderConfig{
		Type:   cfg.PowerProvider.Type,
		Fields: cfg.PowerProvider.Fields,
	}
	provider, err := h.Registry.Create(provCfg)
	if err != nil {
		log.Error().Str("node_id", nodeID).Str("provider_type", cfg.PowerProvider.Type).
			Err(err).Msg("power: failed to create provider")
		writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
			Error: fmt.Sprintf("failed to create power provider %q: %v", cfg.PowerProvider.Type, err),
			Code:  "provider_error",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), flipToDiskTimeout)
	defer cancel()

	if err := provider.SetNextBoot(ctx, power.BootDisk); err != nil {
		log.Error().Str("node_id", nodeID).Str("provider", provider.Name()).
			Err(err).Msg("power: SetNextBoot(disk) failed")
		writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
			Error: fmt.Sprintf("SetNextBoot failed: %v", err),
			Code:  "provider_error",
		})
		return
	}

	log.Info().Str("node_id", nodeID).Str("provider", provider.Name()).
		Bool("cycle", doCycle).Msg("power: next boot set to disk")

	if doCycle {
		if err := provider.PowerCycle(ctx); err != nil {
			log.Error().Str("node_id", nodeID).Str("provider", provider.Name()).
				Err(err).Msg("power: PowerCycle after flip-to-disk failed")
			// Return 207 Multi-Status: flip succeeded but cycle failed.
			writeJSON(w, http.StatusMultiStatus, map[string]interface{}{
				"flip_to_disk": "ok",
				"power_cycle":  fmt.Sprintf("failed: %v", err),
			})
			return
		}
		log.Info().Str("node_id", nodeID).Str("provider", provider.Name()).
			Msg("power: power-cycled after flip-to-disk")
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"node_id":      nodeID,
		"provider":     provider.Name(),
		"boot_device":  "disk",
		"power_cycled": doCycle,
	})
}
