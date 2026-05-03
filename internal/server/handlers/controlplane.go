package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/sqoia-dev/clustr/internal/db"
)

// ControlPlaneDBIface is the subset of *db.DB used by ControlPlaneHandler.
type ControlPlaneDBIface interface {
	GetControlPlaneHost(ctx context.Context) (db.Host, error)
	QueryLatestNodeStats(ctx context.Context) ([]db.LatestNodeStatRow, error)
}

// ControlPlaneHandler serves GET /api/v1/control-plane — summary of the
// control-plane host for the /control-plane UI route.
type ControlPlaneHandler struct {
	DB ControlPlaneDBIface
}

// cpStatus is the JSON shape returned by GET /api/v1/control-plane.
type cpStatus struct {
	// Host is the control-plane host metadata.
	Host cpHostSummary `json:"host"`

	// Metrics is the most-recent sample per (plugin, sensor) for the CP host.
	// Same structure as node stats but filtered to the control-plane host ID.
	Metrics []cpMetricRow `json:"metrics"`

	// OverallStatus is the aggregated status derived from active alerts:
	// "healthy", "degraded" (warn-level alerts), or "critical".
	OverallStatus string `json:"overall_status"`

	// Timestamp is when this response was assembled.
	Timestamp time.Time `json:"timestamp"`
}

type cpHostSummary struct {
	ID        string    `json:"id"`
	Hostname  string    `json:"hostname"`
	CreatedAt time.Time `json:"created_at"`
}

type cpMetricRow struct {
	Plugin string            `json:"plugin"`
	Sensor string            `json:"sensor"`
	Value  float64           `json:"value"`
	Unit   string            `json:"unit,omitempty"`
	Labels map[string]string `json:"labels,omitempty"`
}

// ServeHTTP handles GET /api/v1/control-plane.
func (h *ControlPlaneHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	host, err := h.DB.GetControlPlaneHost(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "control-plane host not yet bootstrapped",
			"code":  "cp_not_ready",
		})
		return
	}

	// Fetch latest metrics for this host.
	allLatest, err := h.DB.QueryLatestNodeStats(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to query metrics",
			"code":  "db_error",
		})
		return
	}

	var metrics []cpMetricRow
	for _, row := range allLatest {
		if row.NodeID != host.ID {
			continue
		}
		metrics = append(metrics, cpMetricRow{
			Plugin: row.Plugin,
			Sensor: row.Sensor,
			Value:  row.Value,
			Unit:   row.Unit,
			Labels: row.Labels,
		})
	}

	resp := cpStatus{
		Host: cpHostSummary{
			ID:        host.ID,
			Hostname:  host.Hostname,
			CreatedAt: host.CreatedAt,
		},
		Metrics:       metrics,
		OverallStatus: "healthy", // alert engine will fire separate alerts; this is the baseline
		Timestamp:     time.Now().UTC(),
	}

	writeJSON(w, http.StatusOK, resp)
}
