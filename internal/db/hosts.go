package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
)

// HostRole is the discriminator for the hosts table.
type HostRole string

const (
	HostRoleControlPlane HostRole = "control_plane"
	HostRoleClusterNode  HostRole = "cluster_node"
)

// Host is a row from the hosts table.
type Host struct {
	ID        string
	Hostname  string
	Role      HostRole
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BootstrapControlPlaneHost ensures exactly one control_plane row exists in the
// hosts table, identified by os.Hostname(). If a row already exists it is
// returned unchanged.  If no row exists a new one is inserted with a fresh UUID.
//
// This is idempotent: safe to call on every startup.  The application-layer
// guard here enforces the "at most one control_plane" invariant that the SQL
// UNIQUE index on (hostname, role) alone cannot fully express across host
// renames (which would create a second row for the new hostname).
func (db *DB) BootstrapControlPlaneHost(ctx context.Context) (Host, error) {
	// Check if any control_plane row exists already.
	var existing Host
	row := db.sql.QueryRowContext(ctx,
		`SELECT id, hostname, role, created_at, updated_at
		 FROM hosts WHERE role = 'control_plane' LIMIT 1`)
	err := row.Scan(&existing.ID, &existing.Hostname, &existing.Role,
		&existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		return existing, nil
	}
	if err != sql.ErrNoRows {
		return Host{}, fmt.Errorf("db: BootstrapControlPlaneHost: query: %w", err)
	}

	// No existing control_plane row — insert one.
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	id := uuid.New().String()
	now := time.Now().UTC()

	_, err = db.sql.ExecContext(ctx, `
		INSERT OR IGNORE INTO hosts (id, hostname, role, created_at, updated_at)
		VALUES (?, ?, 'control_plane', ?, ?)
	`, id, hostname, now, now)
	if err != nil {
		return Host{}, fmt.Errorf("db: BootstrapControlPlaneHost: insert: %w", err)
	}

	// Re-read to get the actual row (INSERT OR IGNORE means another process may
	// have raced us; read back what was committed).
	row = db.sql.QueryRowContext(ctx,
		`SELECT id, hostname, role, created_at, updated_at
		 FROM hosts WHERE role = 'control_plane' LIMIT 1`)
	if err := row.Scan(&existing.ID, &existing.Hostname, &existing.Role,
		&existing.CreatedAt, &existing.UpdatedAt); err != nil {
		return Host{}, fmt.Errorf("db: BootstrapControlPlaneHost: re-read: %w", err)
	}
	return existing, nil
}

// GetControlPlaneHost returns the control_plane host row, or an error if none exists.
func (db *DB) GetControlPlaneHost(ctx context.Context) (Host, error) {
	var h Host
	row := db.sql.QueryRowContext(ctx,
		`SELECT id, hostname, role, created_at, updated_at
		 FROM hosts WHERE role = 'control_plane' LIMIT 1`)
	if err := row.Scan(&h.ID, &h.Hostname, &h.Role, &h.CreatedAt, &h.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return Host{}, fmt.Errorf("db: GetControlPlaneHost: no control_plane row found")
		}
		return Host{}, fmt.Errorf("db: GetControlPlaneHost: %w", err)
	}
	return h, nil
}
