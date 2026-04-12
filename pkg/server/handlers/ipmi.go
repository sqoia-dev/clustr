// Package handlers — ipmi.go implements the power management API endpoints.
// All BMC operations are performed server-side via ipmitool; the web UI calls
// these endpoints rather than constructing raw ipmitool commands itself.
//
// Route prefix: /api/v1/nodes/{id}/power  and  /api/v1/nodes/{id}/sensors
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
	"github.com/sqoia-dev/clonr/pkg/ipmi"
)

// bmcTimeout caps every ipmitool invocation at 10 seconds.
// ipmitool can hang for 30+ seconds when a BMC is unreachable; we prefer a
// fast failure so the UI can show an error quickly.
const bmcTimeout = 10 * time.Second

// PowerCache is the minimal interface the IPMIHandler needs — satisfied by
// *server.PowerCache without importing the server package (avoids import cycle).
// GetFlat returns primitive values so this interface carries no cross-package
// struct types.
type PowerCache interface {
	GetFlat(nodeID string) (status string, lastChecked time.Time, errMsg string, ok bool)
	Set(nodeID, status, errMsg string)
	Invalidate(nodeID string)
}

// IPMIHandler handles /api/v1/nodes/{id}/power* and /api/v1/nodes/{id}/sensors.
type IPMIHandler struct {
	DB    *db.DB
	Cache PowerCache
}

// ─── Response types ───────────────────────────────────────────────────────────

// PowerStatusResponse is returned by GET /api/v1/nodes/{id}/power.
type PowerStatusResponse struct {
	Status      string    `json:"status"`                 // "on", "off", or "unknown"
	LastChecked time.Time `json:"last_checked"`
	Error       string    `json:"error,omitempty"`        // set when BMC was unreachable
}

// PowerActionResponse is returned after a successful power action.
type PowerActionResponse struct {
	Action      string    `json:"action"`
	NodeID      string    `json:"node_id"`
	Status      string    `json:"status,omitempty"`       // best-effort post-action status
	LastChecked time.Time `json:"last_checked,omitempty"`
}

// SensorsResponse is returned by GET /api/v1/nodes/{id}/sensors.
type SensorsResponse struct {
	NodeID      string       `json:"node_id"`
	Sensors     []ipmi.Sensor `json:"sensors"`
	LastChecked time.Time    `json:"last_checked"`
}

// ─── Helper: load node and extract BMC client ─────────────────────────────────

// nodeClient fetches the NodeConfig and returns a ready-to-use ipmi.Client.
// Returns a non-nil http error string and code when the node or BMC config is
// missing; the caller should call writeJSON and return immediately.
func (h *IPMIHandler) nodeClient(w http.ResponseWriter, r *http.Request) (nodeID string, c *ipmi.Client, ok bool) {
	nodeID = chi.URLParam(r, "id")
	cfg, err := h.DB.GetNodeConfig(r.Context(), nodeID)
	if err != nil {
		writeError(w, err)
		return nodeID, nil, false
	}
	if cfg.BMC == nil || cfg.BMC.IPAddress == "" {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{
			Error: "BMC is not configured for this node — edit the node to add BMC credentials",
			Code:  "bmc_not_configured",
		})
		return nodeID, nil, false
	}
	return nodeID, &ipmi.Client{
		Host:     cfg.BMC.IPAddress,
		Username: cfg.BMC.Username,
		Password: cfg.BMC.Password,
	}, true
}

// ─── GET /api/v1/nodes/{id}/power ────────────────────────────────────────────

// GetPowerStatus returns the current power state of the node's BMC.
// Results are cached for 15 seconds to avoid hammering the BMC on every UI poll.
func (h *IPMIHandler) GetPowerStatus(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")

	// Return cached result if still fresh.
	if h.Cache != nil {
		if status, lastChecked, errMsg, hit := h.Cache.GetFlat(nodeID); hit {
			writeJSON(w, http.StatusOK, PowerStatusResponse{
				Status:      status,
				LastChecked: lastChecked,
				Error:       errMsg,
			})
			return
		}
	}

	// Load node and BMC config.
	cfg, err := h.DB.GetNodeConfig(r.Context(), nodeID)
	if err != nil {
		writeError(w, err)
		return
	}
	if cfg.BMC == nil || cfg.BMC.IPAddress == "" {
		writeJSON(w, http.StatusBadRequest, api.ErrorResponse{
			Error: "BMC is not configured for this node",
			Code:  "bmc_not_configured",
		})
		return
	}

	client := &ipmi.Client{
		Host:     cfg.BMC.IPAddress,
		Username: cfg.BMC.Username,
		Password: cfg.BMC.Password,
	}

	ctx, cancel := context.WithTimeout(r.Context(), bmcTimeout)
	defer cancel()

	now := time.Now().UTC()
	ps, err := client.PowerStatus(ctx)
	if err != nil {
		errMsg := fmt.Sprintf("BMC unreachable (%s): %v", cfg.BMC.IPAddress, err)
		log.Warn().Str("node_id", nodeID).Str("bmc_ip", cfg.BMC.IPAddress).Err(err).Msg("ipmi: power status failed")
		if h.Cache != nil {
			h.Cache.Set(nodeID, "unknown", errMsg)
		}
		writeJSON(w, http.StatusOK, PowerStatusResponse{
			Status:      "unknown",
			LastChecked: now,
			Error:       errMsg,
		})
		return
	}

	status := string(ps)
	if h.Cache != nil {
		h.Cache.Set(nodeID, status, "")
	}
	writeJSON(w, http.StatusOK, PowerStatusResponse{
		Status:      status,
		LastChecked: now,
	})
}

// ─── Power action handlers ────────────────────────────────────────────────────

// PowerOn handles POST /api/v1/nodes/{id}/power/on
func (h *IPMIHandler) PowerOn(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "on", func(ctx context.Context, c *ipmi.Client) error {
		return c.PowerOn(ctx)
	})
}

// PowerOff handles POST /api/v1/nodes/{id}/power/off
func (h *IPMIHandler) PowerOff(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "off", func(ctx context.Context, c *ipmi.Client) error {
		return c.PowerOff(ctx)
	})
}

// PowerCycle handles POST /api/v1/nodes/{id}/power/cycle
func (h *IPMIHandler) PowerCycle(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "cycle", func(ctx context.Context, c *ipmi.Client) error {
		return c.PowerCycle(ctx)
	})
}

// PowerReset handles POST /api/v1/nodes/{id}/power/reset
func (h *IPMIHandler) PowerReset(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "reset", func(ctx context.Context, c *ipmi.Client) error {
		return c.PowerReset(ctx)
	})
}

// SetBootPXE handles POST /api/v1/nodes/{id}/power/pxe
// Sets next boot device to PXE, then power-cycles the node.
func (h *IPMIHandler) SetBootPXE(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "pxe", func(ctx context.Context, c *ipmi.Client) error {
		if err := c.SetBootPXE(ctx); err != nil {
			return fmt.Errorf("set boot PXE: %w", err)
		}
		return c.PowerCycle(ctx)
	})
}

// SetBootDisk handles POST /api/v1/nodes/{id}/power/disk
func (h *IPMIHandler) SetBootDisk(w http.ResponseWriter, r *http.Request) {
	h.doPowerAction(w, r, "disk", func(ctx context.Context, c *ipmi.Client) error {
		return c.SetBootDisk(ctx)
	})
}

// doPowerAction is the common implementation for all mutating power actions.
// It loads the node, runs fn against the BMC, invalidates the cache, and logs
// the action to the audit log.
func (h *IPMIHandler) doPowerAction(
	w http.ResponseWriter,
	r *http.Request,
	action string,
	fn func(ctx context.Context, c *ipmi.Client) error,
) {
	nodeID, client, ok := h.nodeClient(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), bmcTimeout)
	defer cancel()

	if err := fn(ctx, client); err != nil {
		log.Error().Str("node_id", nodeID).Str("action", action).Err(err).Msg("ipmi: power action failed")
		writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
			Error: fmt.Sprintf("IPMI %s failed: %v", action, err),
			Code:  "bmc_error",
		})
		return
	}

	// Invalidate cache so the next status poll reflects the new state.
	if h.Cache != nil {
		h.Cache.Invalidate(nodeID)
	}

	log.Info().Str("node_id", nodeID).Str("action", action).Msg("ipmi: power action succeeded")

	writeJSON(w, http.StatusOK, PowerActionResponse{
		Action:      action,
		NodeID:      nodeID,
		LastChecked: time.Now().UTC(),
	})
}

// ─── GET /api/v1/nodes/{id}/sensors ──────────────────────────────────────────

// GetSensors handles GET /api/v1/nodes/{id}/sensors.
// Returns all IPMI sensor readings from the node's BMC.
// Results are not cached here; the UI polls every 30 seconds which is a
// reasonable rate for sensor data.
func (h *IPMIHandler) GetSensors(w http.ResponseWriter, r *http.Request) {
	nodeID, client, ok := h.nodeClient(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), bmcTimeout)
	defer cancel()

	sensors, err := client.GetSensorData(ctx)
	if err != nil {
		log.Warn().Str("node_id", nodeID).Err(err).Msg("ipmi: sensor read failed")
		writeJSON(w, http.StatusBadGateway, api.ErrorResponse{
			Error: fmt.Sprintf("sensor read failed: %v", err),
			Code:  "bmc_error",
		})
		return
	}

	writeJSON(w, http.StatusOK, SensorsResponse{
		NodeID:      nodeID,
		Sensors:     sensors,
		LastChecked: time.Now().UTC(),
	})
}
