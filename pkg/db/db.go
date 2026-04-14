// Package db provides the SQLite persistence layer for clonr.
// It uses modernc.org/sqlite (pure-Go, CGO_ENABLED=0 compatible).
package db

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
	_ "modernc.org/sqlite" // register "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrExpired is returned by LookupAPIKey when a key exists but its TTL has elapsed.
var ErrExpired = fmt.Errorf("api key expired")

// DB wraps sql.DB with typed clonr operations.
type DB struct {
	sql *sql.DB
}

// Open opens (or creates) the SQLite database at path and applies all pending migrations.
func Open(dbPath string) (*DB, error) {
	// WAL mode gives better concurrent read performance; journal_mode must be
	// set before any DDL runs.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=on&_busy_timeout=5000", dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db: open %s: %w", dbPath, err)
	}
	// SQLite handles concurrency via WAL; a single writer is fine.
	sqlDB.SetMaxOpenConns(1)

	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: ping %s: %w", dbPath, err)
	}

	db := &DB{sql: sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: migrate: %w", err)
	}
	return db, nil
}

// Close closes the underlying database connection.
func (db *DB) Close() error {
	return db.sql.Close()
}

// SQL returns the underlying *sql.DB for advanced queries not covered by typed methods.
// Use sparingly — prefer typed methods where possible.
func (db *DB) SQL() *sql.DB {
	return db.sql
}

// migrate applies all SQL migration files in order. Each file is applied once;
// applied migrations are tracked in the schema_migrations table.
func (db *DB) migrate() error {
	// Ensure tracking table exists.
	if _, err := db.sql.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	// Sort by filename to guarantee ordering.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		var count int
		if err := db.sql.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE name = ?`, entry.Name(),
		).Scan(&count); err != nil {
			return fmt.Errorf("check migration %s: %w", entry.Name(), err)
		}
		if count > 0 {
			continue // already applied
		}

		sql, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		if _, err := db.sql.Exec(string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		if _, err := db.sql.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			entry.Name(), time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// ─── BaseImage operations ────────────────────────────────────────────────────

// CreateBaseImage inserts a new BaseImage record. Status is set to "building".
func (db *DB) CreateBaseImage(ctx context.Context, img api.BaseImage) error {
	diskLayout, err := json.Marshal(img.DiskLayout)
	if err != nil {
		return fmt.Errorf("db: marshal disk_layout: %w", err)
	}
	tags, err := json.Marshal(img.Tags)
	if err != nil {
		return fmt.Errorf("db: marshal tags: %w", err)
	}
	builtForRoles := img.BuiltForRoles
	if builtForRoles == nil {
		builtForRoles = []string{}
	}
	builtForRolesJSON, err := json.Marshal(builtForRoles)
	if err != nil {
		return fmt.Errorf("db: marshal built_for_roles: %w", err)
	}

	firmware := string(img.Firmware)
	if firmware == "" {
		firmware = "uefi"
	}

	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO base_images
			(id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
			 blob_path, disk_layout, tags, source_url, notes, error_message,
			 built_for_roles, build_method, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		img.ID, img.Name, img.Version, img.OS, img.Arch,
		string(img.Status), string(img.Format), firmware,
		img.SizeBytes, img.Checksum, "",
		string(diskLayout), string(tags),
		img.SourceURL, img.Notes, img.ErrorMessage,
		string(builtForRolesJSON), img.BuildMethod,
		img.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: create base image: %w", err)
	}
	return nil
}

// GetBaseImage retrieves a single BaseImage by ID.
func (db *DB) GetBaseImage(ctx context.Context, id string) (api.BaseImage, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
		       blob_path, disk_layout, tags, source_url, notes, error_message,
		       built_for_roles, build_method, created_at, finalized_at
		FROM base_images WHERE id = ?
	`, id)

	return scanBaseImage(row)
}

// GetBlobPath returns the server-local filesystem path for an image's blob file.
func (db *DB) GetBlobPath(ctx context.Context, id string) (string, error) {
	var blobPath string
	err := db.sql.QueryRowContext(ctx, `SELECT blob_path FROM base_images WHERE id = ?`, id).Scan(&blobPath)
	if err == sql.ErrNoRows {
		return "", api.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("db: get blob path: %w", err)
	}
	return blobPath, nil
}

// SetBlobPath updates the blob_path for an image (called after blob is written to disk).
func (db *DB) SetBlobPath(ctx context.Context, id, blobPath string) error {
	res, err := db.sql.ExecContext(ctx, `UPDATE base_images SET blob_path = ? WHERE id = ?`, blobPath, id)
	if err != nil {
		return fmt.Errorf("db: set blob path: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ListBaseImages returns all BaseImages. If status is non-empty, it filters by that status.
func (db *DB) ListBaseImages(ctx context.Context, status string) ([]api.BaseImage, error) {
	var rows *sql.Rows
	var err error

	if status != "" {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
			       blob_path, disk_layout, tags, source_url, notes, error_message,
			       built_for_roles, build_method, created_at, finalized_at
			FROM base_images WHERE status = ? ORDER BY created_at DESC
		`, status)
	} else {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
			       blob_path, disk_layout, tags, source_url, notes, error_message,
			       built_for_roles, build_method, created_at, finalized_at
			FROM base_images ORDER BY created_at DESC
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("db: list base images: %w", err)
	}
	defer rows.Close()

	var images []api.BaseImage
	for rows.Next() {
		img, err := scanBaseImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

// UpdateBaseImageStatus updates the status and error_message for an image.
func (db *DB) UpdateBaseImageStatus(ctx context.Context, id string, status api.ImageStatus, errMsg string) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE base_images SET status = ?, error_message = ? WHERE id = ?
	`, string(status), errMsg, id)
	if err != nil {
		return fmt.Errorf("db: update image status: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// FinalizeBaseImage sets size, checksum, finalized_at and status=ready.
func (db *DB) FinalizeBaseImage(ctx context.Context, id string, sizeBytes int64, checksum string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE base_images
		SET size_bytes = ?, checksum = ?, status = 'ready', finalized_at = ?
		WHERE id = ?
	`, sizeBytes, checksum, now, id)
	if err != nil {
		return fmt.Errorf("db: finalize base image: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ArchiveBaseImage sets status=archived.
func (db *DB) ArchiveBaseImage(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE base_images SET status = 'archived' WHERE id = ?
	`, id)
	if err != nil {
		return fmt.Errorf("db: archive base image: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// DeleteBaseImage hard-deletes a BaseImage record. Returns ErrNotFound if the
// image does not exist. Callers must ensure blobs are removed from disk first.
func (db *DB) DeleteBaseImage(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM base_images WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete base image: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ListNodesByBaseImageID returns all NodeConfigs that reference the given image.
func (db *DB) ListNodesByBaseImageID(ctx context.Context, imageID string) ([]api.NodeConfig, error) {
	return db.ListNodeConfigs(ctx, imageID)
}

// ClearBaseImageOnNodes sets base_image_id = NULL for all nodes referencing the
// given image ID. Used by force-delete to unassign nodes before deleting the image.
func (db *DB) ClearBaseImageOnNodes(ctx context.Context, imageID string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET base_image_id = NULL, updated_at = ? WHERE base_image_id = ?`,
		time.Now().Unix(), imageID,
	)
	if err != nil {
		return fmt.Errorf("db: clear base_image_id on nodes: %w", err)
	}
	return nil
}

// UpdateDiskLayout replaces the disk_layout JSON for a BaseImage.
func (db *DB) UpdateDiskLayout(ctx context.Context, id string, layout api.DiskLayout) error {
	data, err := json.Marshal(layout)
	if err != nil {
		return fmt.Errorf("db: marshal disk_layout: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE base_images SET disk_layout = ? WHERE id = ?`, string(data), id)
	if err != nil {
		return fmt.Errorf("db: update disk layout: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ─── NodeConfig operations ───────────────────────────────────────────────────

// CreateNodeConfig inserts a new NodeConfig record.
func (db *DB) CreateNodeConfig(ctx context.Context, cfg api.NodeConfig) error {
	interfaces, err := json.Marshal(cfg.Interfaces)
	if err != nil {
		return fmt.Errorf("db: marshal interfaces: %w", err)
	}
	sshKeys, err := json.Marshal(cfg.SSHKeys)
	if err != nil {
		return fmt.Errorf("db: marshal ssh_keys: %w", err)
	}
	groups, err := json.Marshal(cfg.Groups)
	if err != nil {
		return fmt.Errorf("db: marshal groups: %w", err)
	}
	customVars, err := json.Marshal(cfg.CustomVars)
	if err != nil {
		return fmt.Errorf("db: marshal custom_vars: %w", err)
	}
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return fmt.Errorf("db: marshal hardware_profile: %w", err)
	}
	bmcConfig, err := marshalNullableJSON(cfg.BMC, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal bmc_config: %w", err)
	}
	ibConfig, err := marshalNullableJSON(cfg.IBConfig, "[]")
	if err != nil {
		return fmt.Errorf("db: marshal ib_config: %w", err)
	}
	powerProvider, err := marshalNullableJSON(cfg.PowerProvider, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal power_provider: %w", err)
	}

	diskLayoutOverride, err := marshalDiskLayoutOverride(cfg.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal disk_layout_override: %w", err)
	}
	extraMounts, err := marshalJSONSlice(cfg.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal extra_mounts: %w", err)
	}

	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_configs
			(id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
			 groups, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
			 power_provider, created_at, updated_at, group_id, disk_layout_override, extra_mounts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		cfg.ID, cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
		string(interfaces), string(sshKeys), cfg.KernelArgs,
		string(groups), string(customVars), nullableString(cfg.BaseImageID),
		string(hwProfile), bmcConfig, ibConfig, powerProvider,
		cfg.CreatedAt.Unix(), cfg.UpdatedAt.Unix(),
		nullableString(cfg.GroupID), diskLayoutOverride, extraMounts,
	)
	if err != nil {
		return fmt.Errorf("db: create node config: %w", err)
	}
	return nil
}

// UpsertNodeByMAC creates a new NodeConfig for the given MAC, or updates the
// hardware_profile and hostname of the existing record if one already exists.
// Returns the resulting NodeConfig (created or updated).
func (db *DB) UpsertNodeByMAC(ctx context.Context, cfg api.NodeConfig) (api.NodeConfig, error) {
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: marshal hardware_profile: %w", err)
	}

	// Check whether a record for this MAC already exists.
	existing, err := db.GetNodeConfigByMAC(ctx, cfg.PrimaryMAC)
	if err == nil {
		// Exists — update hardware_profile. Only overwrite hostname when the stored
		// hostname was auto-generated; admin-set hostnames are preserved.
		newHostname := existing.Hostname
		newHostnameAuto := existing.HostnameAuto
		if existing.HostnameAuto && cfg.Hostname != "" {
			newHostname = cfg.Hostname
			newHostnameAuto = cfg.HostnameAuto
		}
		_, err = db.sql.ExecContext(ctx, `
			UPDATE node_configs
			SET hardware_profile = ?, hostname = ?, hostname_auto = ?, updated_at = ?
			WHERE primary_mac = ?
		`, string(hwProfile), newHostname, boolToInt(newHostnameAuto), time.Now().Unix(), cfg.PrimaryMAC)
		if err != nil {
			return api.NodeConfig{}, fmt.Errorf("db: upsert node (update): %w", err)
		}
		return db.GetNodeConfigByMAC(ctx, cfg.PrimaryMAC)
	}

	if err != api.ErrNotFound {
		return api.NodeConfig{}, fmt.Errorf("db: upsert node (lookup): %w", err)
	}

	// New node — insert a stub with no image assigned.
	interfaces, _ := json.Marshal(cfg.Interfaces)
	sshKeys, _ := json.Marshal(cfg.SSHKeys)
	groups, _ := json.Marshal(cfg.Groups)
	customVars, _ := json.Marshal(cfg.CustomVars)

	now := time.Now().UTC()
	cfg.CreatedAt = now
	cfg.UpdatedAt = now

	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_configs
			(id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
			 groups, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
			 power_provider, created_at, updated_at, group_id, disk_layout_override, extra_mounts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, '{}', '[]', '{}', ?, ?, NULL, '{}', '[]')
	`,
		cfg.ID, cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
		string(interfaces), string(sshKeys), cfg.KernelArgs,
		string(groups), string(customVars),
		string(hwProfile),
		cfg.CreatedAt.Unix(), cfg.UpdatedAt.Unix(),
	)
	if err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: upsert node (insert): %w", err)
	}
	return cfg, nil
}

// nullableString returns nil when s is empty, otherwise the string value.
// Used for nullable TEXT columns like base_image_id.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nodeConfigCols is the canonical SELECT column list for node_configs.
// Update this constant whenever columns are added or removed.
const nodeConfigCols = `id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
	       groups, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
	       power_provider, reimage_pending, last_deploy_succeeded_at, last_deploy_failed_at,
	       created_at, updated_at, group_id, disk_layout_override, extra_mounts`

// GetNodeConfig retrieves a NodeConfig by its UUID.
func (db *DB) GetNodeConfig(ctx context.Context, id string) (api.NodeConfig, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+nodeConfigCols+` FROM node_configs WHERE id = ?`, id)
	return scanNodeConfig(row)
}

// GetNodeConfigByMAC retrieves the NodeConfig whose primary_mac matches mac.
func (db *DB) GetNodeConfigByMAC(ctx context.Context, mac string) (api.NodeConfig, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+nodeConfigCols+` FROM node_configs WHERE primary_mac = ?`, mac)
	return scanNodeConfig(row)
}

// ListNodeConfigs returns all NodeConfigs. If baseImageID is non-empty, filters by it.
func (db *DB) ListNodeConfigs(ctx context.Context, baseImageID string) ([]api.NodeConfig, error) {
	var rows *sql.Rows
	var err error

	if baseImageID != "" {
		rows, err = db.sql.QueryContext(ctx,
			`SELECT `+nodeConfigCols+` FROM node_configs WHERE base_image_id = ? ORDER BY hostname ASC`,
			baseImageID)
	} else {
		rows, err = db.sql.QueryContext(ctx,
			`SELECT `+nodeConfigCols+` FROM node_configs ORDER BY hostname ASC`)
	}
	if err != nil {
		return nil, fmt.Errorf("db: list node configs: %w", err)
	}
	defer rows.Close()

	var cfgs []api.NodeConfig
	for rows.Next() {
		cfg, err := scanNodeConfig(rows)
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, rows.Err()
}

// UpdateNodeConfig replaces the mutable fields of a NodeConfig.
func (db *DB) UpdateNodeConfig(ctx context.Context, cfg api.NodeConfig) error {
	interfaces, err := json.Marshal(cfg.Interfaces)
	if err != nil {
		return fmt.Errorf("db: marshal interfaces: %w", err)
	}
	sshKeys, err := json.Marshal(cfg.SSHKeys)
	if err != nil {
		return fmt.Errorf("db: marshal ssh_keys: %w", err)
	}
	groups, err := json.Marshal(cfg.Groups)
	if err != nil {
		return fmt.Errorf("db: marshal groups: %w", err)
	}
	customVars, err := json.Marshal(cfg.CustomVars)
	if err != nil {
		return fmt.Errorf("db: marshal custom_vars: %w", err)
	}
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return fmt.Errorf("db: marshal hardware_profile: %w", err)
	}
	bmcConfig, err := marshalNullableJSON(cfg.BMC, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal bmc_config: %w", err)
	}
	ibConfig, err := marshalNullableJSON(cfg.IBConfig, "[]")
	if err != nil {
		return fmt.Errorf("db: marshal ib_config: %w", err)
	}
	powerProvider, err := marshalNullableJSON(cfg.PowerProvider, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal power_provider: %w", err)
	}

	diskLayoutOverride, err := marshalDiskLayoutOverride(cfg.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal disk_layout_override: %w", err)
	}
	extraMounts, err := marshalJSONSlice(cfg.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal extra_mounts: %w", err)
	}

	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET hostname = ?, hostname_auto = ?, fqdn = ?, primary_mac = ?, interfaces = ?, ssh_keys = ?,
		    kernel_args = ?, groups = ?, custom_vars = ?, base_image_id = ?,
		    hardware_profile = ?, bmc_config = ?, ib_config = ?, power_provider = ?,
		    group_id = ?, disk_layout_override = ?, extra_mounts = ?, updated_at = ?
		WHERE id = ?
	`,
		cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
		string(interfaces), string(sshKeys), cfg.KernelArgs,
		string(groups), string(customVars), nullableString(cfg.BaseImageID),
		string(hwProfile), bmcConfig, ibConfig, powerProvider,
		nullableString(cfg.GroupID), diskLayoutOverride, extraMounts,
		time.Now().Unix(), cfg.ID,
	)
	if err != nil {
		return fmt.Errorf("db: update node config: %w", err)
	}
	return requireOneRow(res, "node_configs", cfg.ID)
}

// DeleteNodeConfig removes a NodeConfig by ID.
func (db *DB) DeleteNodeConfig(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM node_configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete node config: %w", err)
	}
	return requireOneRow(res, "node_configs", id)
}

// SetReimagePending sets or clears the reimage_pending flag for a node.
// Set to true before power-cycling for a reimage; clear to false after finalize.
func (db *DB) SetReimagePending(ctx context.Context, nodeID string, pending bool) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET reimage_pending = ?, updated_at = ? WHERE id = ?`,
		boolToInt(pending), time.Now().Unix(), nodeID,
	)
	if err != nil {
		return fmt.Errorf("db: set reimage_pending: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// RecordDeploySucceeded marks a node's last deployment as successful.
// Sets last_deploy_succeeded_at = now() and clears reimage_pending.
// Called by the deploy-complete HTTP callback from the node after finalize.
func (db *DB) RecordDeploySucceeded(ctx context.Context, nodeID string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET last_deploy_succeeded_at = ?, reimage_pending = 0, updated_at = ?
		WHERE id = ?
	`, now, now, nodeID)
	if err != nil {
		return fmt.Errorf("db: record deploy succeeded: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// RecordDeployFailed marks a node's last deployment as failed.
// Sets last_deploy_failed_at = now(). Does not clear reimage_pending —
// the admin must decide whether to retry or cancel.
func (db *DB) RecordDeployFailed(ctx context.Context, nodeID string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET last_deploy_failed_at = ?, updated_at = ?
		WHERE id = ?
	`, now, now, nodeID)
	if err != nil {
		return fmt.Errorf("db: record deploy failed: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// ─── ReimageRequest operations ───────────────────────────────────────────────

// CreateReimageRequest inserts a new reimage request with status "pending".
func (db *DB) CreateReimageRequest(ctx context.Context, req api.ReimageRequest) error {
	var scheduledAtVal interface{}
	if req.ScheduledAt != nil {
		scheduledAtVal = req.ScheduledAt.Unix()
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO reimage_requests
			(id, node_id, image_id, status, scheduled_at, error_message,
			 requested_by, dry_run, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		req.ID, req.NodeID, req.ImageID, string(req.Status),
		scheduledAtVal, req.ErrorMessage,
		req.RequestedBy, boolToInt(req.DryRun), req.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: create reimage request: %w", err)
	}
	return nil
}

// GetReimageRequest retrieves a single ReimageRequest by ID.
func (db *DB) GetReimageRequest(ctx context.Context, id string) (api.ReimageRequest, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
		       started_at, completed_at, error_message, requested_by, dry_run, created_at,
		       exit_code, exit_name, phase
		FROM reimage_requests WHERE id = ?
	`, id)
	return scanReimageRequest(row)
}

// ListReimageRequests returns all ReimageRequests for a node, newest first.
// If nodeID is empty, returns all requests.
func (db *DB) ListReimageRequests(ctx context.Context, nodeID string) ([]api.ReimageRequest, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if nodeID != "" {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
			       started_at, completed_at, error_message, requested_by, dry_run, created_at,
			       exit_code, exit_name, phase
			FROM reimage_requests WHERE node_id = ? ORDER BY created_at DESC
		`, nodeID)
	} else {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
			       started_at, completed_at, error_message, requested_by, dry_run, created_at,
			       exit_code, exit_name, phase
			FROM reimage_requests ORDER BY created_at DESC
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("db: list reimage requests: %w", err)
	}
	defer rows.Close()
	return collectReimageRows(rows)
}

// ListPendingScheduledRequests returns all requests with status "pending" and a
// scheduled_at time at or before `before`. Used by the scheduler goroutine.
func (db *DB) ListPendingScheduledRequests(ctx context.Context, before time.Time) ([]api.ReimageRequest, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
		       started_at, completed_at, error_message, requested_by, dry_run, created_at,
		       exit_code, exit_name, phase
		FROM reimage_requests
		WHERE status = 'pending' AND scheduled_at IS NOT NULL AND scheduled_at <= ?
		ORDER BY scheduled_at ASC
	`, before.Unix())
	if err != nil {
		return nil, fmt.Errorf("db: list pending scheduled requests: %w", err)
	}
	defer rows.Close()
	return collectReimageRows(rows)
}

// UpdateReimageRequestStatus updates the status and error_message of a request.
// It also sets the appropriate timestamp column based on the new status.
func (db *DB) UpdateReimageRequestStatus(ctx context.Context, id string, status api.ReimageStatus, errMsg string) error {
	now := time.Now().Unix()
	var q string
	switch status {
	case api.ReimageStatusTriggered:
		q = `UPDATE reimage_requests SET status = ?, error_message = ?, triggered_at = ? WHERE id = ?`
	case api.ReimageStatusInProgress:
		q = `UPDATE reimage_requests SET status = ?, error_message = ?, started_at = ? WHERE id = ?`
	case api.ReimageStatusComplete, api.ReimageStatusFailed, api.ReimageStatusCanceled:
		q = `UPDATE reimage_requests SET status = ?, error_message = ?, completed_at = ? WHERE id = ?`
	default:
		q = `UPDATE reimage_requests SET status = ?, error_message = ?, triggered_at = ? WHERE id = ?`
	}
	res, err := db.sql.ExecContext(ctx, q, string(status), errMsg, now, id)
	if err != nil {
		return fmt.Errorf("db: update reimage status: %w", err)
	}
	return requireOneRow(res, "reimage_requests", id)
}

// UpdateReimageRequestFailed transitions a reimage request to failed status and
// captures the structured failure detail from the deploy agent's exit code payload.
func (db *DB) UpdateReimageRequestFailed(ctx context.Context, id string, errMsg string, exitCode int, exitName, phase string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE reimage_requests
		SET status = 'failed', error_message = ?, completed_at = ?,
		    exit_code = ?, exit_name = ?, phase = ?
		WHERE id = ?
	`, errMsg, now, exitCode, exitName, phase, id)
	if err != nil {
		return fmt.Errorf("db: update reimage failed: %w", err)
	}
	return requireOneRow(res, "reimage_requests", id)
}

// GetActiveReimageForNode returns the first non-terminal reimage request for
// nodeID, or (nil, nil) if none exists.
func (db *DB) GetActiveReimageForNode(ctx context.Context, nodeID string) (*api.ReimageRequest, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
		       started_at, completed_at, error_message, requested_by, dry_run, created_at,
		       exit_code, exit_name, phase
		FROM reimage_requests
		WHERE node_id = ?
		  AND status NOT IN ('complete', 'failed', 'canceled')
		ORDER BY created_at DESC
		LIMIT 1
	`, nodeID)
	req, err := scanReimageRequest(row)
	if err == api.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// ─── ReimageRequest scan helpers ─────────────────────────────────────────────

// scanNullableTimestamp converts a raw SQLite column value to *time.Time.
// SQLite is dynamically typed: a column declared INTEGER may hold a TEXT value
// if an older code path stored a formatted string instead of a Unix int64.
// This helper handles both representations so we don't blow up on legacy rows.
func scanNullableTimestamp(v any) (*time.Time, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case int64:
		if x == 0 {
			return nil, nil
		}
		t := time.Unix(x, 0).UTC()
		return &t, nil
	case string:
		if x == "" {
			return nil, nil
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			t := time.Unix(n, 0).UTC()
			return &t, nil
		}
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, x); err == nil {
				t = t.UTC()
				return &t, nil
			}
		}
		return nil, fmt.Errorf("db: unknown timestamp format: %q", x)
	default:
		return nil, fmt.Errorf("db: unexpected timestamp type %T", v)
	}
}

func scanReimageRequest(s scanner) (api.ReimageRequest, error) {
	var (
		req           api.ReimageRequest
		status        string
		scheduledAt   any
		triggeredAt   any
		startedAt     any
		completedAt   any
		dryRunInt     int
		createdAtUnix int64
		exitCodeNull  sql.NullInt64
		exitNameNull  sql.NullString
		phaseNull     sql.NullString
	)
	err := s.Scan(
		&req.ID, &req.NodeID, &req.ImageID, &status,
		&scheduledAt, &triggeredAt, &startedAt, &completedAt,
		&req.ErrorMessage, &req.RequestedBy, &dryRunInt, &createdAtUnix,
		&exitCodeNull, &exitNameNull, &phaseNull,
	)
	if err == sql.ErrNoRows {
		return api.ReimageRequest{}, api.ErrNotFound
	}
	if err != nil {
		return api.ReimageRequest{}, fmt.Errorf("db: scan reimage request: %w", err)
	}
	req.Status = api.ReimageStatus(status)
	req.DryRun = dryRunInt != 0
	req.CreatedAt = time.Unix(createdAtUnix, 0).UTC()

	if exitCodeNull.Valid {
		v := int(exitCodeNull.Int64)
		req.ExitCode = &v
	}
	if exitNameNull.Valid {
		req.ExitName = exitNameNull.String
	}
	if phaseNull.Valid {
		req.Phase = phaseNull.String
	}

	var tsErr error
	if req.ScheduledAt, tsErr = scanNullableTimestamp(scheduledAt); tsErr != nil {
		return api.ReimageRequest{}, fmt.Errorf("db: scan reimage request scheduled_at: %w", tsErr)
	}
	if req.TriggeredAt, tsErr = scanNullableTimestamp(triggeredAt); tsErr != nil {
		return api.ReimageRequest{}, fmt.Errorf("db: scan reimage request triggered_at: %w", tsErr)
	}
	if req.StartedAt, tsErr = scanNullableTimestamp(startedAt); tsErr != nil {
		return api.ReimageRequest{}, fmt.Errorf("db: scan reimage request started_at: %w", tsErr)
	}
	if req.CompletedAt, tsErr = scanNullableTimestamp(completedAt); tsErr != nil {
		return api.ReimageRequest{}, fmt.Errorf("db: scan reimage request completed_at: %w", tsErr)
	}
	return req, nil
}

func collectReimageRows(rows *sql.Rows) ([]api.ReimageRequest, error) {
	var reqs []api.ReimageRequest
	for rows.Next() {
		req, err := scanReimageRequest(rows)
		if err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate reimage requests: %w", err)
	}
	return reqs, nil
}

// ─── Internal scan helpers ───────────────────────────────────────────────────

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanBaseImage(s scanner) (api.BaseImage, error) {
	var (
		img                api.BaseImage
		status             string
		format             string
		firmware           string
		diskLayoutJSON     string
		tagsJSON           string
		builtForRolesJSON  string
		createdAtUnix      int64
		finalizedAtUnix    sql.NullInt64
		blobPath           string // scanned but not exposed in API type
	)

	err := s.Scan(
		&img.ID, &img.Name, &img.Version, &img.OS, &img.Arch,
		&status, &format, &firmware,
		&img.SizeBytes, &img.Checksum, &blobPath,
		&diskLayoutJSON, &tagsJSON,
		&img.SourceURL, &img.Notes, &img.ErrorMessage,
		&builtForRolesJSON, &img.BuildMethod,
		&createdAtUnix, &finalizedAtUnix,
	)
	if err == sql.ErrNoRows {
		return api.BaseImage{}, api.ErrNotFound
	}
	if err != nil {
		return api.BaseImage{}, fmt.Errorf("db: scan base image: %w", err)
	}

	img.Status = api.ImageStatus(status)
	img.Format = api.ImageFormat(format)
	if firmware == "" {
		firmware = "uefi"
	}
	img.Firmware = api.ImageFirmware(firmware)
	img.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	if finalizedAtUnix.Valid {
		t := time.Unix(finalizedAtUnix.Int64, 0).UTC()
		img.FinalizedAt = &t
	}

	if err := json.Unmarshal([]byte(diskLayoutJSON), &img.DiskLayout); err != nil {
		return api.BaseImage{}, fmt.Errorf("db: unmarshal disk_layout: %w", err)
	}
	if err := json.Unmarshal([]byte(tagsJSON), &img.Tags); err != nil {
		return api.BaseImage{}, fmt.Errorf("db: unmarshal tags: %w", err)
	}
	if img.Tags == nil {
		img.Tags = []string{}
	}
	if builtForRolesJSON != "" && builtForRolesJSON != "null" {
		if err := json.Unmarshal([]byte(builtForRolesJSON), &img.BuiltForRoles); err != nil {
			// Non-fatal: old rows may have malformed JSON; just leave the field nil.
			img.BuiltForRoles = nil
		}
	}

	return img, nil
}

func scanNodeConfig(s scanner) (api.NodeConfig, error) {
	var (
		cfg                      api.NodeConfig
		hostnameAuto             int
		interfacesJSON           string
		sshKeysJSON              string
		groupsJSON               string
		customVarsJSON           string
		baseImageID              sql.NullString
		hwProfileJSON            string
		bmcConfigJSON            string
		ibConfigJSON             string
		powerProviderJSON        string
		reimagePending           int
		lastDeploySucceededAtVal sql.NullInt64
		lastDeployFailedAtVal    sql.NullInt64
		createdAtUnix            int64
		updatedAtUnix            int64
		groupID                  sql.NullString
		diskLayoutOverrideJSON   string
		extraMountsJSON          string
	)

	err := s.Scan(
		&cfg.ID, &cfg.Hostname, &hostnameAuto, &cfg.FQDN, &cfg.PrimaryMAC,
		&interfacesJSON, &sshKeysJSON, &cfg.KernelArgs,
		&groupsJSON, &customVarsJSON, &baseImageID,
		&hwProfileJSON, &bmcConfigJSON, &ibConfigJSON,
		&powerProviderJSON, &reimagePending,
		&lastDeploySucceededAtVal, &lastDeployFailedAtVal,
		&createdAtUnix, &updatedAtUnix,
		&groupID, &diskLayoutOverrideJSON, &extraMountsJSON,
	)
	if err == sql.ErrNoRows {
		return api.NodeConfig{}, api.ErrNotFound
	}
	if err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: scan node config: %w", err)
	}

	cfg.HostnameAuto = hostnameAuto != 0
	cfg.ReimagePending = reimagePending != 0

	if baseImageID.Valid {
		cfg.BaseImageID = baseImageID.String
	}
	if groupID.Valid {
		cfg.GroupID = groupID.String
	}
	if diskLayoutOverrideJSON != "" && diskLayoutOverrideJSON != "{}" {
		var layout api.DiskLayout
		if err := json.Unmarshal([]byte(diskLayoutOverrideJSON), &layout); err == nil {
			// Only treat as a real override if it has at least one partition defined.
			if len(layout.Partitions) > 0 {
				cfg.DiskLayoutOverride = &layout
			}
		}
	}

	if lastDeploySucceededAtVal.Valid {
		t := time.Unix(lastDeploySucceededAtVal.Int64, 0).UTC()
		cfg.LastDeploySucceededAt = &t
	}
	if lastDeployFailedAtVal.Valid {
		t := time.Unix(lastDeployFailedAtVal.Int64, 0).UTC()
		cfg.LastDeployFailedAt = &t
	}

	cfg.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	cfg.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()

	if err := json.Unmarshal([]byte(interfacesJSON), &cfg.Interfaces); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal interfaces: %w", err)
	}
	if err := json.Unmarshal([]byte(sshKeysJSON), &cfg.SSHKeys); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal ssh_keys: %w", err)
	}
	if err := json.Unmarshal([]byte(groupsJSON), &cfg.Groups); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal groups: %w", err)
	}
	if err := json.Unmarshal([]byte(customVarsJSON), &cfg.CustomVars); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal custom_vars: %w", err)
	}
	if hwProfileJSON != "" {
		if err := json.Unmarshal([]byte(hwProfileJSON), &cfg.HardwareProfile); err != nil {
			// Non-fatal: log but don't abort.
			cfg.HardwareProfile = nil
		}
	}
	if bmcConfigJSON != "" && bmcConfigJSON != "{}" {
		var bmc api.BMCNodeConfig
		if err := json.Unmarshal([]byte(bmcConfigJSON), &bmc); err == nil {
			cfg.BMC = &bmc
		}
	}
	if ibConfigJSON != "" && ibConfigJSON != "[]" {
		if err := json.Unmarshal([]byte(ibConfigJSON), &cfg.IBConfig); err != nil {
			cfg.IBConfig = nil
		}
	}
	if powerProviderJSON != "" && powerProviderJSON != "{}" {
		var pp api.PowerProviderConfig
		if err := json.Unmarshal([]byte(powerProviderJSON), &pp); err == nil && pp.Type != "" {
			cfg.PowerProvider = &pp
		}
	}

	if extraMountsJSON != "" && extraMountsJSON != "[]" {
		if err := json.Unmarshal([]byte(extraMountsJSON), &cfg.ExtraMounts); err != nil {
			cfg.ExtraMounts = nil // non-fatal: treat corrupt entry as empty
		}
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

	return cfg, nil
}

// marshalNullableJSON marshals v to JSON. If v is nil or an empty slice/map,
// returns the provided default JSON string (e.g. "{}" or "[]").
func marshalNullableJSON(v interface{}, defaultVal string) (string, error) {
	if v == nil {
		return defaultVal, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// marshalJSONSlice marshals a slice to a JSON array string.
// A nil or empty slice marshals as "[]".
func marshalJSONSlice(v interface{}) (string, error) {
	if v == nil {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ─── Log operations ──────────────────────────────────────────────────────────

// InsertLog persists a single LogEntry.
func (db *DB) InsertLog(ctx context.Context, entry api.LogEntry) error {
	fields, err := json.Marshal(entry.Fields)
	if err != nil {
		return fmt.Errorf("db: marshal log fields: %w", err)
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_logs (id, node_mac, hostname, level, component, message, fields, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID, entry.NodeMAC, entry.Hostname, entry.Level, entry.Component,
		entry.Message, string(fields), entry.Timestamp.Unix())
	if err != nil {
		return fmt.Errorf("db: insert log: %w", err)
	}
	return nil
}

// InsertLogBatch persists a slice of LogEntry records in a single transaction.
func (db *DB) InsertLogBatch(ctx context.Context, entries []api.LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin log batch tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT OR IGNORE INTO node_logs (id, node_mac, hostname, level, component, message, fields, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return fmt.Errorf("db: prepare log batch: %w", err)
	}
	defer stmt.Close()

	for _, entry := range entries {
		fields, err := json.Marshal(entry.Fields)
		if err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("db: marshal log fields: %w", err)
		}
		if _, err := stmt.ExecContext(ctx, entry.ID, entry.NodeMAC, entry.Hostname,
			entry.Level, entry.Component, entry.Message,
			string(fields), entry.Timestamp.Unix()); err != nil {
			tx.Rollback() //nolint:errcheck
			return fmt.Errorf("db: exec log batch insert: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit log batch: %w", err)
	}
	return nil
}

// QueryLogs returns log entries matching the given filter, newest first.
func (db *DB) QueryLogs(ctx context.Context, f api.LogFilter) ([]api.LogEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	query := `SELECT id, node_mac, hostname, level, component, message, fields, timestamp
	          FROM node_logs WHERE 1=1`
	args := []interface{}{}

	if f.NodeMAC != "" {
		query += " AND node_mac = ?"
		args = append(args, f.NodeMAC)
	}
	if f.Hostname != "" {
		query += " AND hostname = ?"
		args = append(args, f.Hostname)
	}
	if f.Level != "" {
		query += " AND level = ?"
		args = append(args, f.Level)
	}
	if f.Component != "" {
		query += " AND component = ?"
		args = append(args, f.Component)
	}
	if f.Since != nil {
		query += " AND timestamp >= ?"
		args = append(args, f.Since.Unix())
	}
	query += " ORDER BY timestamp DESC LIMIT ?"
	args = append(args, limit)

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query logs: %w", err)
	}
	defer rows.Close()

	var entries []api.LogEntry
	for rows.Next() {
		var (
			entry      api.LogEntry
			fieldsJSON string
			tsUnix     int64
		)
		if err := rows.Scan(&entry.ID, &entry.NodeMAC, &entry.Hostname,
			&entry.Level, &entry.Component, &entry.Message,
			&fieldsJSON, &tsUnix); err != nil {
			return nil, fmt.Errorf("db: scan log entry: %w", err)
		}
		entry.Timestamp = time.Unix(tsUnix, 0).UTC()
		if fieldsJSON != "" && fieldsJSON != "{}" {
			if err := json.Unmarshal([]byte(fieldsJSON), &entry.Fields); err != nil {
				entry.Fields = nil // non-fatal, just drop corrupt fields
			}
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate logs: %w", err)
	}
	return entries, nil
}

// PurgeLogs deletes log entries older than olderThan and returns the count deleted.
func (db *DB) PurgeLogs(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`DELETE FROM node_logs WHERE timestamp < ?`, olderThan.Unix())
	if err != nil {
		return 0, fmt.Errorf("db: purge logs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ─── NodeGroup operations ────────────────────────────────────────────────────

// CreateNodeGroup inserts a new NodeGroup.
func (db *DB) CreateNodeGroup(ctx context.Context, g api.NodeGroup) error {
	diskLayout, err := marshalDiskLayoutOverride(g.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal node group disk_layout: %w", err)
	}
	extraMounts, err := marshalJSONSlice(g.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal node group extra_mounts: %w", err)
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_groups (id, name, description, disk_layout, extra_mounts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, g.ID, g.Name, g.Description, diskLayout, extraMounts, g.CreatedAt.Unix(), g.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("db: create node group: %w", err)
	}
	return nil
}

// GetNodeGroup retrieves a NodeGroup by ID.
func (db *DB) GetNodeGroup(ctx context.Context, id string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, disk_layout, extra_mounts, created_at, updated_at
		FROM node_groups WHERE id = ?
	`, id)
	return scanNodeGroup(row)
}

// GetNodeGroupByName retrieves a NodeGroup by name.
func (db *DB) GetNodeGroupByName(ctx context.Context, name string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, disk_layout, extra_mounts, created_at, updated_at
		FROM node_groups WHERE name = ?
	`, name)
	return scanNodeGroup(row)
}

// ListNodeGroups returns all NodeGroups, ordered by name.
func (db *DB) ListNodeGroups(ctx context.Context) ([]api.NodeGroup, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, description, disk_layout, extra_mounts, created_at, updated_at
		FROM node_groups ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list node groups: %w", err)
	}
	defer rows.Close()

	var groups []api.NodeGroup
	for rows.Next() {
		g, err := scanNodeGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// UpdateNodeGroup replaces the mutable fields of a NodeGroup.
func (db *DB) UpdateNodeGroup(ctx context.Context, g api.NodeGroup) error {
	diskLayout, err := marshalDiskLayoutOverride(g.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal node group disk_layout: %w", err)
	}
	extraMounts, err := marshalJSONSlice(g.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal node group extra_mounts: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_groups
		SET name = ?, description = ?, disk_layout = ?, extra_mounts = ?, updated_at = ?
		WHERE id = ?
	`, g.Name, g.Description, diskLayout, extraMounts, time.Now().Unix(), g.ID)
	if err != nil {
		return fmt.Errorf("db: update node group: %w", err)
	}
	return requireOneRow(res, "node_groups", g.ID)
}

// DeleteNodeGroup removes a NodeGroup. Nodes in the group have their group_id
// cleared first to avoid orphaned references.
func (db *DB) DeleteNodeGroup(ctx context.Context, id string) error {
	// Clear group membership on any nodes in this group.
	_, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET group_id = NULL, updated_at = ? WHERE group_id = ?`,
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("db: clear node group memberships before delete: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM node_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete node group: %w", err)
	}
	return requireOneRow(res, "node_groups", id)
}

// AssignNodeToGroup sets or clears the group_id for a node. Pass empty groupID to remove.
func (db *DB) AssignNodeToGroup(ctx context.Context, nodeID, groupID string) error {
	var gid interface{}
	if groupID != "" {
		gid = groupID
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET group_id = ?, updated_at = ? WHERE id = ?`,
		gid, time.Now().Unix(), nodeID)
	if err != nil {
		return fmt.Errorf("db: assign node to group: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// SetNodeLayoutOverride sets or clears the disk_layout_override for a node.
// Pass nil to clear the override (node will use group or image layout).
func (db *DB) SetNodeLayoutOverride(ctx context.Context, nodeID string, layout *api.DiskLayout) error {
	override, err := marshalDiskLayoutOverride(layout)
	if err != nil {
		return fmt.Errorf("db: marshal node layout override: %w", err)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET disk_layout_override = ?, updated_at = ? WHERE id = ?`,
		override, time.Now().Unix(), nodeID)
	if err != nil {
		return fmt.Errorf("db: set node layout override: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// scanNodeGroup scans a single NodeGroup row.
func scanNodeGroup(s scanner) (api.NodeGroup, error) {
	var (
		g               api.NodeGroup
		diskLayoutJSON  string
		extraMountsJSON string
		createdAtUnix   int64
		updatedAtUnix   int64
	)
	err := s.Scan(&g.ID, &g.Name, &g.Description, &diskLayoutJSON, &extraMountsJSON, &createdAtUnix, &updatedAtUnix)
	if err == sql.ErrNoRows {
		return api.NodeGroup{}, api.ErrNotFound
	}
	if err != nil {
		return api.NodeGroup{}, fmt.Errorf("db: scan node group: %w", err)
	}
	g.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	g.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()

	if diskLayoutJSON != "" && diskLayoutJSON != "{}" {
		var layout api.DiskLayout
		if err := json.Unmarshal([]byte(diskLayoutJSON), &layout); err == nil {
			if len(layout.Partitions) > 0 {
				g.DiskLayoutOverride = &layout
			}
		}
	}
	if extraMountsJSON != "" && extraMountsJSON != "[]" {
		if err := json.Unmarshal([]byte(extraMountsJSON), &g.ExtraMounts); err != nil {
			g.ExtraMounts = nil // non-fatal
		}
	}
	return g, nil
}

// ─── Group membership operations (020_group_memberships) ────────────────────

// AddGroupMember inserts a node_group_memberships row. Idempotent via INSERT OR IGNORE.
// Also updates node_configs.group_id to this group (last-write wins for single-group display).
func (db *DB) AddGroupMember(ctx context.Context, groupID, nodeID string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO node_group_memberships (node_id, group_id) VALUES (?, ?)`,
		nodeID, groupID)
	if err != nil {
		return fmt.Errorf("db: add group member: %w", err)
	}
	// Update the fast-path group_id on node_configs for display / layout resolution.
	_, _ = db.sql.ExecContext(ctx,
		`UPDATE node_configs SET group_id = ?, updated_at = ? WHERE id = ?`,
		groupID, time.Now().Unix(), nodeID)
	return nil
}

// RemoveGroupMember deletes a node_group_memberships row. No-op if absent.
func (db *DB) RemoveGroupMember(ctx context.Context, groupID, nodeID string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM node_group_memberships WHERE node_id = ? AND group_id = ?`,
		nodeID, groupID)
	if err != nil {
		return fmt.Errorf("db: remove group member: %w", err)
	}
	// Clear group_id on node_configs if this was the node's active group.
	_, _ = db.sql.ExecContext(ctx,
		`UPDATE node_configs SET group_id = NULL, updated_at = ?
		 WHERE id = ? AND group_id = ?`,
		time.Now().Unix(), nodeID, groupID)
	return nil
}

// ListGroupMembers returns all NodeConfigs that are members of groupID via the
// node_group_memberships table.
func (db *DB) ListGroupMembers(ctx context.Context, groupID string) ([]api.NodeConfig, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+nodeConfigCols+`
		 FROM node_configs
		 WHERE id IN (SELECT node_id FROM node_group_memberships WHERE group_id = ?)
		 ORDER BY hostname ASC`, groupID)
	if err != nil {
		return nil, fmt.Errorf("db: list group members: %w", err)
	}
	defer rows.Close()
	var cfgs []api.NodeConfig
	for rows.Next() {
		cfg, err := scanNodeConfig(rows)
		if err != nil {
			return nil, err
		}
		cfgs = append(cfgs, cfg)
	}
	return cfgs, rows.Err()
}

// ListGroupMemberships returns the group IDs for a node.
func (db *DB) ListGroupMemberships(ctx context.Context, nodeID string) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT group_id FROM node_group_memberships WHERE node_id = ? ORDER BY group_id`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("db: list group memberships: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListNodeGroupsWithCount returns all NodeGroups with member_count populated from
// the node_group_memberships table.
func (db *DB) ListNodeGroupsWithCount(ctx context.Context) ([]api.NodeGroupWithCount, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT ng.id, ng.name, ng.description, ng.role, ng.disk_layout, ng.extra_mounts,
		       ng.created_at, ng.updated_at,
		       COUNT(m.node_id) AS member_count
		FROM node_groups ng
		LEFT JOIN node_group_memberships m ON m.group_id = ng.id
		GROUP BY ng.id
		ORDER BY ng.name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list node groups with count: %w", err)
	}
	defer rows.Close()

	var groups []api.NodeGroupWithCount
	for rows.Next() {
		var (
			g               api.NodeGroupWithCount
			roleNull        sql.NullString
			diskLayoutJSON  string
			extraMountsJSON string
			createdAtUnix   int64
			updatedAtUnix   int64
		)
		err := rows.Scan(&g.ID, &g.Name, &g.Description, &roleNull,
			&diskLayoutJSON, &extraMountsJSON, &createdAtUnix, &updatedAtUnix,
			&g.MemberCount)
		if err != nil {
			return nil, fmt.Errorf("db: scan node group with count: %w", err)
		}
		g.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
		g.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
		if roleNull.Valid {
			g.Role = roleNull.String
		}
		if diskLayoutJSON != "" && diskLayoutJSON != "{}" {
			var layout api.DiskLayout
			if err := json.Unmarshal([]byte(diskLayoutJSON), &layout); err == nil && len(layout.Partitions) > 0 {
				g.DiskLayoutOverride = &layout
			}
		}
		if extraMountsJSON != "" && extraMountsJSON != "[]" {
			if err := json.Unmarshal([]byte(extraMountsJSON), &g.ExtraMounts); err != nil {
				g.ExtraMounts = nil
			}
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// GetNodeGroupFull returns a NodeGroup with role populated.
func (db *DB) GetNodeGroupFull(ctx context.Context, id string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, role, disk_layout, extra_mounts, created_at, updated_at
		FROM node_groups WHERE id = ?
	`, id)
	return scanNodeGroupFull(row)
}

// GetNodeGroupByNameFull returns a NodeGroup by name with role populated.
func (db *DB) GetNodeGroupByNameFull(ctx context.Context, name string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, role, disk_layout, extra_mounts, created_at, updated_at
		FROM node_groups WHERE name = ?
	`, name)
	return scanNodeGroupFull(row)
}

// UpdateNodeGroupFull replaces all mutable fields including role.
func (db *DB) UpdateNodeGroupFull(ctx context.Context, g api.NodeGroup) error {
	diskLayout, err := marshalDiskLayoutOverride(g.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal node group disk_layout: %w", err)
	}
	extraMounts, err := marshalJSONSlice(g.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal node group extra_mounts: %w", err)
	}
	var roleVal interface{}
	if g.Role != "" {
		roleVal = g.Role
	}
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_groups
		SET name = ?, description = ?, role = ?, disk_layout = ?, extra_mounts = ?, updated_at = ?
		WHERE id = ?
	`, g.Name, g.Description, roleVal, diskLayout, extraMounts, time.Now().Unix(), g.ID)
	if err != nil {
		return fmt.Errorf("db: update node group full: %w", err)
	}
	return requireOneRow(res, "node_groups", g.ID)
}

// CreateNodeGroupFull inserts a NodeGroup with role support.
func (db *DB) CreateNodeGroupFull(ctx context.Context, g api.NodeGroup) error {
	diskLayout, err := marshalDiskLayoutOverride(g.DiskLayoutOverride)
	if err != nil {
		return fmt.Errorf("db: marshal node group disk_layout: %w", err)
	}
	extraMounts, err := marshalJSONSlice(g.ExtraMounts)
	if err != nil {
		return fmt.Errorf("db: marshal node group extra_mounts: %w", err)
	}
	var roleVal interface{}
	if g.Role != "" {
		roleVal = g.Role
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_groups (id, name, description, role, disk_layout, extra_mounts, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, g.ID, g.Name, g.Description, roleVal, diskLayout, extraMounts, g.CreatedAt.Unix(), g.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("db: create node group full: %w", err)
	}
	return nil
}

func scanNodeGroupFull(s scanner) (api.NodeGroup, error) {
	var (
		g               api.NodeGroup
		roleNull        sql.NullString
		diskLayoutJSON  string
		extraMountsJSON string
		createdAtUnix   int64
		updatedAtUnix   int64
	)
	err := s.Scan(&g.ID, &g.Name, &g.Description, &roleNull,
		&diskLayoutJSON, &extraMountsJSON, &createdAtUnix, &updatedAtUnix)
	if err == sql.ErrNoRows {
		return api.NodeGroup{}, api.ErrNotFound
	}
	if err != nil {
		return api.NodeGroup{}, fmt.Errorf("db: scan node group full: %w", err)
	}
	g.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	g.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	if roleNull.Valid {
		g.Role = roleNull.String
	}
	if diskLayoutJSON != "" && diskLayoutJSON != "{}" {
		var layout api.DiskLayout
		if err := json.Unmarshal([]byte(diskLayoutJSON), &layout); err == nil && len(layout.Partitions) > 0 {
			g.DiskLayoutOverride = &layout
		}
	}
	if extraMountsJSON != "" && extraMountsJSON != "[]" {
		if err := json.Unmarshal([]byte(extraMountsJSON), &g.ExtraMounts); err != nil {
			g.ExtraMounts = nil
		}
	}
	return g, nil
}

// ─── Group reimage jobs (020_group_memberships) ──────────────────────────────

// GroupReimageJob is a row in group_reimage_jobs.
type GroupReimageJob struct {
	ID                 string    `json:"id"`
	GroupID            string    `json:"group_id"`
	ImageID            string    `json:"image_id"`
	Concurrency        int       `json:"concurrency"`
	PauseOnFailurePct  int       `json:"pause_on_failure_pct"`
	Status             string    `json:"status"`
	TotalNodes         int       `json:"total_nodes"`
	TriggeredNodes     int       `json:"triggered_nodes"`
	SucceededNodes     int       `json:"succeeded_nodes"`
	FailedNodes        int       `json:"failed_nodes"`
	ErrorMessage       string    `json:"error_message,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// CreateGroupReimageJob inserts a new group reimage job.
func (db *DB) CreateGroupReimageJob(ctx context.Context, j GroupReimageJob) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO group_reimage_jobs
			(id, group_id, image_id, concurrency, pause_on_failure_pct, status,
			 total_nodes, triggered_nodes, succeeded_nodes, failed_nodes,
			 error_message, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, j.ID, j.GroupID, j.ImageID, j.Concurrency, j.PauseOnFailurePct, j.Status,
		j.TotalNodes, j.TriggeredNodes, j.SucceededNodes, j.FailedNodes,
		j.ErrorMessage, j.CreatedAt.Unix(), j.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("db: create group reimage job: %w", err)
	}
	return nil
}

// GetGroupReimageJob retrieves a single job by ID.
func (db *DB) GetGroupReimageJob(ctx context.Context, id string) (GroupReimageJob, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, group_id, image_id, concurrency, pause_on_failure_pct, status,
		       total_nodes, triggered_nodes, succeeded_nodes, failed_nodes,
		       error_message, created_at, updated_at
		FROM group_reimage_jobs WHERE id = ?
	`, id)
	return scanGroupReimageJob(row)
}

// UpdateGroupReimageJob replaces all mutable fields of a job record.
func (db *DB) UpdateGroupReimageJob(ctx context.Context, j GroupReimageJob) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE group_reimage_jobs
		SET status = ?, total_nodes = ?, triggered_nodes = ?, succeeded_nodes = ?,
		    failed_nodes = ?, error_message = ?, updated_at = ?
		WHERE id = ?
	`, j.Status, j.TotalNodes, j.TriggeredNodes, j.SucceededNodes, j.FailedNodes,
		j.ErrorMessage, time.Now().Unix(), j.ID)
	if err != nil {
		return fmt.Errorf("db: update group reimage job: %w", err)
	}
	return nil
}

// ResumeGroupReimageJob transitions a paused job back to running.
func (db *DB) ResumeGroupReimageJob(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE group_reimage_jobs SET status = 'running', updated_at = ? WHERE id = ? AND status = 'paused'`,
		time.Now().Unix(), id)
	if err != nil {
		return fmt.Errorf("db: resume group reimage job: %w", err)
	}
	return nil
}

func scanGroupReimageJob(s scanner) (GroupReimageJob, error) {
	var (
		j             GroupReimageJob
		createdAtUnix int64
		updatedAtUnix int64
	)
	err := s.Scan(&j.ID, &j.GroupID, &j.ImageID, &j.Concurrency, &j.PauseOnFailurePct,
		&j.Status, &j.TotalNodes, &j.TriggeredNodes, &j.SucceededNodes, &j.FailedNodes,
		&j.ErrorMessage, &createdAtUnix, &updatedAtUnix)
	if err == sql.ErrNoRows {
		return GroupReimageJob{}, api.ErrNotFound
	}
	if err != nil {
		return GroupReimageJob{}, fmt.Errorf("db: scan group reimage job: %w", err)
	}
	j.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	j.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	return j, nil
}

// ─── Image build resumable fields ───────────────────────────────────────────

// SetImageResumable marks an image as interrupted and resumable, recording the
// last phase it reached. Called by ReconcileStuckBuilds (F2/F3 feature).
func (db *DB) SetImageResumable(ctx context.Context, id, fromPhase string) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE base_images
		SET status = 'interrupted', resumable = 1, resume_from_phase = ?,
		    error_message = 'build interrupted — server was restarted'
		WHERE id = ?
	`, fromPhase, id)
	if err != nil {
		return fmt.Errorf("db: set image resumable: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ClearImageResumable clears the resumable flag, e.g. on successful resume or delete.
func (db *DB) ClearImageResumable(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE base_images SET resumable = 0, resume_from_phase = '' WHERE id = ?
	`, id)
	return err
}

// ListResumableImages returns all images with resumable=1.
func (db *DB) ListResumableImages(ctx context.Context) ([]api.BaseImage, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
		       blob_path, disk_layout, tags, source_url, notes, error_message,
		       built_for_roles, build_method, created_at, finalized_at
		FROM base_images WHERE resumable = 1 ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list resumable images: %w", err)
	}
	defer rows.Close()

	var images []api.BaseImage
	for rows.Next() {
		img, err := scanBaseImage(rows)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, rows.Err()
}

// GetImageResumePhase returns the resume_from_phase for an image.
func (db *DB) GetImageResumePhase(ctx context.Context, id string) (string, bool, error) {
	var phase string
	var resumable int
	err := db.sql.QueryRowContext(ctx,
		`SELECT resume_from_phase, resumable FROM base_images WHERE id = ?`, id,
	).Scan(&phase, &resumable)
	if err == sql.ErrNoRows {
		return "", false, api.ErrNotFound
	}
	if err != nil {
		return "", false, fmt.Errorf("db: get image resume phase: %w", err)
	}
	return phase, resumable != 0, nil
}

// ─── Initramfs build history ─────────────────────────────────────────────────

// InitramfsBuildRecord is a row in the initramfs_builds table.
type InitramfsBuildRecord struct {
	ID                string     `json:"id"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at,omitempty"`
	SHA256            string     `json:"sha256"`
	SizeBytes         int64      `json:"size_bytes"`
	KernelVersion     string     `json:"kernel_version"`
	TriggeredByPrefix string     `json:"triggered_by_prefix"`
	TriggeredByLabel  string     `json:"triggered_by_label,omitempty"` // human label from api_keys.label; empty for session auth
	Outcome           string     `json:"outcome"`
}

// CreateInitramfsBuild inserts a new initramfs build record.
func (db *DB) CreateInitramfsBuild(ctx context.Context, r InitramfsBuildRecord) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO initramfs_builds (id, started_at, finished_at, sha256, size_bytes, kernel_version, triggered_by_prefix, outcome)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, r.ID, r.StartedAt.Unix(), nullableTimestamp(r.FinishedAt), r.SHA256, r.SizeBytes,
		r.KernelVersion, r.TriggeredByPrefix, r.Outcome)
	if err != nil {
		return fmt.Errorf("db: create initramfs build: %w", err)
	}
	return nil
}

// FinishInitramfsBuild updates a build record on completion.
func (db *DB) FinishInitramfsBuild(ctx context.Context, id string, sha256 string, sizeBytes int64, kernelVersion, outcome string) error {
	now := time.Now().Unix()
	_, err := db.sql.ExecContext(ctx, `
		UPDATE initramfs_builds
		SET finished_at = ?, sha256 = ?, size_bytes = ?, kernel_version = ?, outcome = ?
		WHERE id = ?
	`, now, sha256, sizeBytes, kernelVersion, outcome, id)
	if err != nil {
		return fmt.Errorf("db: finish initramfs build: %w", err)
	}
	return nil
}

// ListInitramfsBuilds returns the last N initramfs build records, newest first.
// triggered_by_label is resolved by joining api_keys on the key prefix: we look
// for a key whose hash starts with the stored 8-char prefix. When the build was
// triggered via a browser session (no API key) the prefix is "session" and no
// label is available; in that case TriggeredByLabel is left empty.
func (db *DB) ListInitramfsBuilds(ctx context.Context, limit int) ([]InitramfsBuildRecord, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT ib.id, ib.started_at, ib.finished_at, ib.sha256, ib.size_bytes,
		       ib.kernel_version, ib.triggered_by_prefix, ib.outcome,
		       COALESCE(ak.label, '') AS triggered_by_label
		FROM initramfs_builds ib
		LEFT JOIN api_keys ak
		       ON ib.triggered_by_prefix != 'session'
		      AND ak.label IS NOT NULL
		      AND ak.revoked_at IS NULL
		      AND SUBSTR(ak.key_hash, 1, 8) = ib.triggered_by_prefix
		ORDER BY ib.started_at DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list initramfs builds: %w", err)
	}
	defer rows.Close()

	var records []InitramfsBuildRecord
	for rows.Next() {
		var r InitramfsBuildRecord
		var startedAtUnix int64
		var finishedAtUnix sql.NullInt64
		if err := rows.Scan(&r.ID, &startedAtUnix, &finishedAtUnix,
			&r.SHA256, &r.SizeBytes, &r.KernelVersion, &r.TriggeredByPrefix,
			&r.Outcome, &r.TriggeredByLabel); err != nil {
			return nil, fmt.Errorf("db: scan initramfs build: %w", err)
		}
		r.StartedAt = time.Unix(startedAtUnix, 0).UTC()
		if finishedAtUnix.Valid {
			t := time.Unix(finishedAtUnix.Int64, 0).UTC()
			r.FinishedAt = &t
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// TrimInitramfsBuilds deletes old records keeping only the most recent `keep` rows.
func (db *DB) TrimInitramfsBuilds(ctx context.Context, keep int) error {
	_, err := db.sql.ExecContext(ctx, `
		DELETE FROM initramfs_builds WHERE id NOT IN (
			SELECT id FROM initramfs_builds ORDER BY started_at DESC LIMIT ?
		)
	`, keep)
	return err
}

// DeleteInitramfsBuild deletes a single build record by ID.
func (db *DB) DeleteInitramfsBuild(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM initramfs_builds WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete initramfs build: %w", err)
	}
	return nil
}

// LatestSuccessfulInitramfsBuildID returns the ID of the most recent build with
// outcome = 'success'. Returns ("", sql.ErrNoRows) when none exists.
func (db *DB) LatestSuccessfulInitramfsBuildID(ctx context.Context) (string, error) {
	var id string
	err := db.sql.QueryRowContext(ctx, `
		SELECT id FROM initramfs_builds
		WHERE outcome = 'success'
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// nullableTimestamp converts *time.Time to a SQLite-compatible nullable int64.
func nullableTimestamp(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// marshalDiskLayoutOverride serialises a *DiskLayout to JSON for storage.
// nil (no override) is stored as '{}' to distinguish from a real empty layout.
func marshalDiskLayoutOverride(layout *api.DiskLayout) (string, error) {
	if layout == nil {
		return "{}", nil
	}
	b, err := json.Marshal(layout)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// boolToInt converts a bool to SQLite's INTEGER representation (0 or 1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// requireOneRow returns ErrNotFound if no rows were affected.
func requireOneRow(res sql.Result, table, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("db: rows affected (%s %s): %w", table, id, err)
	}
	if n == 0 {
		return api.ErrNotFound
	}
	return nil
}
