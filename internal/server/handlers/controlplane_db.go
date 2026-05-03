package handlers

import (
	"context"

	"github.com/sqoia-dev/clustr/internal/db"
)

// controlPlaneDBAdapter wraps *db.DB to satisfy ControlPlaneDBIface.
type controlPlaneDBAdapter struct {
	db *db.DB
}

// NewControlPlaneDBAdapter wraps a *db.DB for use by ControlPlaneHandler.
func NewControlPlaneDBAdapter(database *db.DB) ControlPlaneDBIface {
	return &controlPlaneDBAdapter{db: database}
}

func (a *controlPlaneDBAdapter) GetControlPlaneHost(ctx context.Context) (db.Host, error) {
	return a.db.GetControlPlaneHost(ctx)
}

func (a *controlPlaneDBAdapter) QueryLatestNodeStats(ctx context.Context) ([]db.LatestNodeStatRow, error) {
	return a.db.QueryLatestNodeStats(ctx)
}
