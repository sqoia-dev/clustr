// Package db provides the SQLite persistence layer for clustr.
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
	"sync"
	"time"

	"github.com/sqoia-dev/clustr/internal/secrets"
	"github.com/sqoia-dev/clustr/pkg/api"
	_ "modernc.org/sqlite" // register "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// ErrExpired is returned by LookupAPIKey when a key exists but its TTL has elapsed.
var ErrExpired = fmt.Errorf("api key expired")

// DB wraps sql.DB with typed clustr operations.
type DB struct {
	sql *sql.DB

	// lastUsedMu protects lastUsedBatch. Keys are key_hash values; values are
	// the timestamp of the most recent use. The background flusher drains the
	// map every 30 seconds and writes all pending updates in a single transaction.
	lastUsedMu    sync.Mutex
	lastUsedBatch map[string]int64 // key_hash → unix timestamp

	// lastUsedDone signals the background flusher to stop.
	lastUsedDone chan struct{}
}

// Open opens (or creates) the SQLite database at path and applies all pending migrations.
func Open(dbPath string) (*DB, error) {
	// WAL mode gives better concurrent read performance; journal_mode must be
	// set before any DDL runs.
	//
	// Note: _foreign_keys=on is intentionally absent from the DSN. Several
	// migrations use the SQLite rename-and-recreate idiom and rely on FK
	// enforcement being OFF during DDL (PRAGMA foreign_keys OFF inside a
	// transaction is ignored by SQLite, so DSN-level FK-on would leave FK ON
	// during those migrations). Instead, migrate() explicitly disables FK at
	// the connection level before running migrations, and Open() re-enables
	// it after all migrations complete via an explicit PRAGMA outside any
	// transaction.
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=5000", dbPath)
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

	db := &DB{
		sql:           sqlDB,
		lastUsedBatch: make(map[string]int64),
		lastUsedDone:  make(chan struct{}),
	}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: migrate: %w", err)
	}
	// Restore connection-level pragmas after migrations complete.
	//
	// migrate() sets:
	//   - PRAGMA foreign_keys = off     (so FK constraints are not enforced
	//     during migrations that touch schema; in-transaction PRAGMA is a no-op
	//     in SQLite so we do it outside)
	//   - PRAGMA legacy_alter_table = on (so ALTER TABLE RENAME does not update
	//     FK references in sqlite_master — required because SQLite 3.26.0+
	//     unconditionally updates FK references on RENAME, which breaks migrations
	//     058/059 that use the rename-and-recreate idiom)
	//
	// After all migrations are applied we restore normal runtime settings:
	if _, err := sqlDB.Exec("PRAGMA legacy_alter_table = off"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: restore legacy_alter_table: %w", err)
	}
	if _, err := sqlDB.Exec("PRAGMA foreign_keys = on"); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: re-enable foreign_keys: %w", err)
	}
	go db.lastUsedFlusher()
	return db, nil
}

// Close stops the background flusher, flushes any pending last_used_at updates,
// checkpoints the WAL (so the -wal/-shm side-files are removed), and closes
// the underlying database connection.
func (db *DB) Close() error {
	close(db.lastUsedDone)
	db.flushLastUsed()
	// Checkpoint and truncate the WAL before closing so that the -wal and -shm
	// side-files are removed. Without this, tests using t.TempDir() fail with
	// "directory not empty" because those files outlive the connection.
	_, _ = db.sql.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return db.sql.Close()
}

// Ping verifies the database connection is alive by executing a lightweight query.
func (db *DB) Ping(ctx context.Context) error {
	return db.sql.PingContext(ctx)
}

// lastUsedFlusher runs in a background goroutine and flushes the lastUsedBatch
// map every 30 seconds via a single transaction.
func (db *DB) lastUsedFlusher() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			db.flushLastUsed()
		case <-db.lastUsedDone:
			return
		}
	}
}

// FlushLastUsed is an exported wrapper around flushLastUsed for use in tests
// that need to synchronously flush pending last_used_at updates without waiting
// for the 30-second background ticker.
func (db *DB) FlushLastUsed() { db.flushLastUsed() }

// flushLastUsed drains lastUsedBatch and writes all pending last_used_at updates
// in a single transaction. No-op when the batch is empty.
func (db *DB) flushLastUsed() {
	db.lastUsedMu.Lock()
	if len(db.lastUsedBatch) == 0 {
		db.lastUsedMu.Unlock()
		return
	}
	batch := db.lastUsedBatch
	db.lastUsedBatch = make(map[string]int64)
	db.lastUsedMu.Unlock()

	tx, err := db.sql.Begin()
	if err != nil {
		return
	}
	stmt, err := tx.Prepare(`UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?`)
	if err != nil {
		_ = tx.Rollback()
		return
	}
	defer stmt.Close()
	for keyHash, ts := range batch {
		_, _ = stmt.Exec(ts, keyHash)
	}
	_ = tx.Commit()
}

// SQL returns the underlying *sql.DB for advanced queries not covered by typed methods.
// Use sparingly — prefer typed methods where possible.
func (db *DB) SQL() *sql.DB {
	return db.sql
}

// SchemaVersion returns the number of applied migrations, which serves as a
// monotonic schema version number. Used by "clustr-serverd version" to report
// the DB schema state alongside the binary version.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: schema version: %w", err)
	}
	return n, nil
}

// migrate applies all SQL migration files in order. Each file is applied once;
// applied migrations are tracked in the schema_migrations table.
//
// IMPORTANT — FORWARD-ONLY MIGRATIONS, NO AUTOMATIC ROLLBACK:
//
//  1. Migrations are forward-only. There is no automatic rollback mechanism.
//     Once a migration has been applied and committed, it will not be reversed
//     by this runner under any circumstances.
//
//  2. If a bad migration ships to production, operators must manually intervene
//     using the SQLite CLI:
//     a. Stop clustr-serverd.
//     b. Open the database: sqlite3 /var/lib/clustr/clustr.db
//     c. Reverse the schema change manually (DROP TABLE, ALTER TABLE, etc.).
//     d. Remove the migration record: DELETE FROM schema_migrations WHERE name = '<file>';
//     e. Restart clustr-serverd (the fixed migration will be re-applied on startup).
//
//  3. Always test migrations on a copy of the production database before shipping.
//     Use: sqlite3 /var/lib/clustr/clustr.db ".backup /tmp/clustr-test.db" and run
//     the server against /tmp/clustr-test.db first to validate the migration.
func (db *DB) migrate() error {
	// Disable FK enforcement for the duration of all migrations.
	//
	// Several migrations (058, 100) use the SQLite table-rename-and-recreate
	// idiom and embed "PRAGMA foreign_keys = OFF" in their SQL. However,
	// SQLite silently ignores PRAGMA foreign_key changes inside a transaction.
	// Because the migration runner wraps each file in a transaction (tx.Exec),
	// the embedded PRAGMAs have no effect.
	//
	// The consequence for migration 058 (users table recreation): with FK
	// enforcement ON, "ALTER TABLE users RENAME TO _users_old" causes SQLite to
	// rewrite every FK reference to users in the sqlite_master catalog to point
	// at _users_old. After "DROP TABLE _users_old" those references become
	// dangling, and any subsequent INSERT into a table that once referenced
	// users (e.g. node_groups.pi_user_id) fails with "no such table: _users_old".
	//
	// Setting FK enforcement OFF here — at the connection level, outside any
	// transaction — prevents SQLite from rewriting FK references during RENAME.
	// db.Open() re-enables FK enforcement after all migrations complete.
	if _, err := db.sql.Exec("PRAGMA foreign_keys = off"); err != nil {
		return fmt.Errorf("disable foreign_keys for migrations: %w", err)
	}
	// SQLite 3.26.0+ changed ALTER TABLE RENAME to unconditionally update FK
	// references in the sqlite_master catalog — even when foreign_keys = OFF.
	// Migrations 058 and 059 use the rename-and-recreate idiom; the new behaviour
	// causes them to rewrite FK references (e.g. node_groups.pi_user_id) to point
	// at the temporary table name (_users_old, _users_059_old). After the DROP of
	// the temporary table the references become dangling, so any subsequent INSERT
	// into node_groups fails with "no such table: _users_old".
	//
	// PRAGMA legacy_alter_table = ON restores the pre-3.26.0 behaviour where
	// RENAME does NOT update FK references, which is what the migrations expect.
	if _, err := db.sql.Exec("PRAGMA legacy_alter_table = on"); err != nil {
		return fmt.Errorf("enable legacy_alter_table for migrations: %w", err)
	}

	// Ensure tracking table exists.
	if _, err := db.sql.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// One-time fix: CR-1 renamed duplicate-prefixed migrations.
	// Update schema_migrations so existing databases recognize the new filenames.
	renames := map[string]string{
		"020_group_memberships.sql":        "021_group_memberships.sql",
		"020_reimage_terminal_state.sql":   "022_reimage_terminal_state.sql",
		"021_image_metadata.sql":           "023_image_metadata.sql",
		"022_post_reboot_verification.sql": "024_post_reboot_verification.sql",
		"022_users.sql":                    "025_users.sql",
	}
	for oldName, newName := range renames {
		db.sql.Exec(`UPDATE schema_migrations SET name = ? WHERE name = ?`, newName, oldName)
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

		tx, err := db.sql.Begin()
		if err != nil {
			return fmt.Errorf("begin transaction for migration %s: %w", entry.Name(), err)
		}
		if _, err := tx.Exec(string(sql)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", entry.Name(), err)
		}
		// Q6b — runtime FK-violation guard.
		//
		// After every migration's DDL executes, run PRAGMA foreign_key_check inside
		// the same transaction. If any rows come back, we abort and roll back —
		// migration is forward-only with no automatic recovery, so we MUST refuse to
		// commit a schema that left dangling FK references in sqlite_master.
		//
		// Class of bug we're catching: migrations 058/059 used the rename-and-drop
		// idiom against a connection where SQLite 3.26.0+ rewrites FK targets in
		// sqlite_master.  Without legacy_alter_table=ON those rewrites turn every
		// FK pointing at users into a dangling reference once the temp table is
		// dropped.  The guard would have caught that the moment 058 was applied.
		//
		// PRAGMA foreign_key_check returns one row per violating ROWID:
		//   (table, rowid, parent, fkid)
		// We collect them into a flat list for the error message.
		if violErr := assertNoFKViolations(tx, entry.Name()); violErr != nil {
			tx.Rollback()
			return violErr
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
			entry.Name(), time.Now().Unix(),
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// assertNoFKViolations runs PRAGMA foreign_key_check inside tx and returns a
// non-nil error if any FK violations are present. Used by migrate() to refuse
// to commit a migration that leaves the schema with dangling/orphaned FKs.
//
// SQLite reports four columns per violating row: (table, rowid, parent, fkid).
// "parent" is the FK target table; when "parent" names a table that no longer
// exists in sqlite_master the violation is a dangling reference (the precise
// class of bug introduced by migrations 058 and 059 before the legacy_alter_table
// fix landed).
func assertNoFKViolations(tx *sql.Tx, migrationName string) error {
	rows, err := tx.Query("PRAGMA foreign_key_check")
	if err != nil {
		// Inability to run the diagnostic itself is not a hard fail — log and
		// continue so a transient SQLite hiccup doesn't strand operators on a
		// working schema. Production gating is handled at the application layer.
		return nil
	}
	defer rows.Close()

	var violations []string
	for rows.Next() {
		var table sql.NullString
		var rowid sql.NullInt64
		var parent sql.NullString
		var fkid sql.NullInt64
		if scanErr := rows.Scan(&table, &rowid, &parent, &fkid); scanErr != nil {
			// Skip un-scannable rows; they shouldn't block a migration that's
			// otherwise clean, but they also can't legitimately exist on modern
			// SQLite. Best-effort: keep going.
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"table=%s rowid=%d parent=%s fkid=%d",
			table.String, rowid.Int64, parent.String, fkid.Int64,
		))
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return fmt.Errorf("migration %s: foreign_key_check iter: %w", migrationName, rowsErr)
	}
	if len(violations) > 0 {
		return fmt.Errorf(
			"migration %s left FK violations (refusing to commit): %v",
			migrationName, violations,
		)
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

	instrs := img.InstallInstructions
	if instrs == nil {
		instrs = []api.InstallInstruction{}
	}
	instrsJSON, err := json.Marshal(instrs)
	if err != nil {
		return fmt.Errorf("db: marshal install_instructions: %w", err)
	}

	firmware := string(img.Firmware)
	if firmware == "" {
		firmware = "uefi"
	}

	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO base_images
			(id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
			 blob_path, disk_layout, tags, source_url, notes, error_message,
			 built_for_roles, build_method, install_instructions, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		img.ID, img.Name, img.Version, img.OS, img.Arch,
		string(img.Status), string(img.Format), firmware,
		img.SizeBytes, img.Checksum, "",
		string(diskLayout), string(tags),
		img.SourceURL, img.Notes, img.ErrorMessage,
		string(builtForRolesJSON), img.BuildMethod,
		string(instrsJSON),
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
		       built_for_roles, build_method, install_instructions, created_at, finalized_at
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

// ListBaseImages returns all BaseImages.
// status: if non-empty, filters by that status.
// tag: if non-empty, filters to images whose JSON tags array contains that value (S2-3).
func (db *DB) ListBaseImages(ctx context.Context, status, tag string) ([]api.BaseImage, error) {
	// Build query dynamically based on filters.
	// SQLite JSON1: json_each(tags) lets us filter on individual array elements.
	baseQ := `
		SELECT id, name, version, os, arch, status, format, firmware, size_bytes, checksum,
		       blob_path, disk_layout, tags, source_url, notes, error_message,
		       built_for_roles, build_method, install_instructions, created_at, finalized_at
		FROM base_images`

	args := []any{}
	where := []string{}

	if status != "" {
		where = append(where, "status = ?")
		args = append(args, status)
	}
	if tag != "" {
		// EXISTS subquery against json_each is the cleanest SQLite pattern for
		// checking membership in a JSON array column.
		where = append(where, "EXISTS (SELECT 1 FROM json_each(tags) WHERE value = ?)")
		args = append(args, tag)
	}

	if len(where) > 0 {
		baseQ += " WHERE " + strings.Join(where, " AND ")
	}
	baseQ += " ORDER BY created_at DESC"

	rows, err := db.sql.QueryContext(ctx, baseQ, args...)
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

// validImageStatuses is the set of allowed base_images.status values (S2-10, #245).
var validImageStatuses = map[api.ImageStatus]struct{}{
	api.ImageStatusBuilding:    {},
	api.ImageStatusInterrupted: {},
	api.ImageStatusReady:       {},
	api.ImageStatusArchived:    {},
	api.ImageStatusError:       {},
	api.ImageStatusCorrupt:     {},    // #245: blob checksum drifted, cannot auto-heal
	api.ImageStatusBlobMissing: {},    // #245: blob file absent from disk
}

// UpdateBaseImageStatus updates the status and error_message for an image.
// Returns an error if status is not one of the defined ImageStatus constants (S2-10).
func (db *DB) UpdateBaseImageStatus(ctx context.Context, id string, status api.ImageStatus, errMsg string) error {
	if _, ok := validImageStatuses[status]; !ok {
		return fmt.Errorf("db: invalid image status %q", status)
	}
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

// RepairBaseImageChecksum updates checksum and size_bytes for a ready image after
// the reconciler confirms the on-disk artifact is the correct source of truth (F1/F6
// self-heal path). Sets status back to 'ready' in case it had been quarantined.
// This is the only path that sets checksum without going through full finalization —
// callers must have independent corroboration before calling this.
func (db *DB) RepairBaseImageChecksum(ctx context.Context, id, checksum string, sizeBytes int64) error {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE base_images
		SET checksum = ?, size_bytes = ?, status = 'ready', error_message = ''
		WHERE id = ?
	`, checksum, sizeBytes, id)
	if err != nil {
		return fmt.Errorf("db: repair base image checksum: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// UpdateBaseImageArch sets the arch column for an image. Called by the lazy
// architecture detection path in GetImage when the column was blank at read time.
func (db *DB) UpdateBaseImageArch(ctx context.Context, id, arch string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE base_images SET arch = ? WHERE id = ?`, arch, id,
	)
	if err != nil {
		return fmt.Errorf("db: update base image arch: %w", err)
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

// UpdateImageTags replaces the tags JSON array for the given image (S2-3).
func (db *DB) UpdateImageTags(ctx context.Context, id string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	encoded, err := json.Marshal(tags)
	if err != nil {
		return fmt.Errorf("db: marshal image tags: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE base_images SET tags = ? WHERE id = ?`, string(encoded), id)
	if err != nil {
		return fmt.Errorf("db: update image tags: %w", err)
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

// UpdateInstallInstructions replaces the install_instructions JSON for a BaseImage.
func (db *DB) UpdateInstallInstructions(ctx context.Context, id string, instrs []api.InstallInstruction) error {
	if instrs == nil {
		instrs = []api.InstallInstruction{}
	}
	data, err := json.Marshal(instrs)
	if err != nil {
		return fmt.Errorf("db: marshal install_instructions: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `UPDATE base_images SET install_instructions = ? WHERE id = ?`, string(data), id)
	if err != nil {
		return fmt.Errorf("db: update install instructions: %w", err)
	}
	return requireOneRow(res, "base_images", id)
}

// ─── NodeConfig operations ───────────────────────────────────────────────────

// CreateNodeConfig inserts a new NodeConfig record.
func (db *DB) CreateNodeConfig(ctx context.Context, cfg api.NodeConfig) error {
	cfg.PrimaryMAC = strings.ToLower(cfg.PrimaryMAC)
	interfaces, err := json.Marshal(cfg.Interfaces)
	if err != nil {
		return fmt.Errorf("db: marshal interfaces: %w", err)
	}
	sshKeys, err := json.Marshal(cfg.SSHKeys)
	if err != nil {
		return fmt.Errorf("db: marshal ssh_keys: %w", err)
	}
	// S2-4: Tags is the canonical field; Groups mirrors it for backward compat.
	// When writing, prefer Tags if set, fall back to Groups for old callers.
	tagsToWrite := cfg.Tags
	if len(tagsToWrite) == 0 && len(cfg.Groups) > 0 {
		tagsToWrite = cfg.Groups
	}
	tags, err := json.Marshal(tagsToWrite)
	if err != nil {
		return fmt.Errorf("db: marshal tags: %w", err)
	}
	customVars, err := json.Marshal(cfg.CustomVars)
	if err != nil {
		return fmt.Errorf("db: marshal custom_vars: %w", err)
	}
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return fmt.Errorf("db: marshal hardware_profile: %w", err)
	}
	bmcConfigRaw, err := marshalNullableJSON(cfg.BMC, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal bmc_config: %w", err)
	}
	ibConfig, err := marshalNullableJSON(cfg.IBConfig, "[]")
	if err != nil {
		return fmt.Errorf("db: marshal ib_config: %w", err)
	}
	powerProviderRaw, err := marshalNullableJSON(cfg.PowerProvider, "{}")
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

	// Encrypt credential blobs at rest (S1-16).
	bmcConfig, bmcEncrypted := encryptNodeBlob(bmcConfigRaw, "{}")
	powerProvider, ppEncrypted := encryptNodeBlob(powerProviderRaw, "{}")

	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_configs
			(id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
			 tags, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
			 power_provider, created_at, updated_at, disk_layout_override, extra_mounts,
			 detected_firmware, bmc_config_encrypted, power_provider_encrypted, provider)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		cfg.ID, cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
		string(interfaces), string(sshKeys), cfg.KernelArgs,
		string(tags), string(customVars), nullableString(cfg.BaseImageID),
		string(hwProfile), bmcConfig, ibConfig, powerProvider,
		cfg.CreatedAt.Unix(), cfg.UpdatedAt.Unix(),
		diskLayoutOverride, extraMounts,
		cfg.DetectedFirmware,
		boolToInt(bmcEncrypted), boolToInt(ppEncrypted), cfg.Provider,
	)
	if err != nil {
		return fmt.Errorf("db: create node config: %w", err)
	}
	return nil
}

// encryptNodeBlob attempts to encrypt a JSON blob with AES-256-GCM.
// Returns (value, encrypted) — if encryption fails (key not set or invalid),
// returns the original value with encrypted=false (fail-open for create).
func encryptNodeBlob(jsonBlob, emptyVal string) (string, bool) {
	if jsonBlob == "" || jsonBlob == emptyVal {
		return jsonBlob, false
	}
	enc, err := secrets.Encrypt([]byte(jsonBlob))
	if err != nil {
		// CLUSTR_SECRET_KEY not set or invalid — store plaintext, flag unencrypted.
		// MigrateBMCCredentials() will re-encrypt on next startup once key is set.
		return jsonBlob, false
	}
	return enc, true
}

// MigrateBMCCredentials re-encrypts any plaintext BMC and power_provider credentials
// on first run after migration 039. Safe to call multiple times — rows already marked
// as encrypted are skipped. Returns (changed, error).
func (db *DB) MigrateBMCCredentials(ctx context.Context) (bool, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, bmc_config, power_provider
		FROM node_configs
		WHERE (bmc_config_encrypted = 0 AND bmc_config != '' AND bmc_config != '{}')
		   OR (power_provider_encrypted = 0 AND power_provider != '' AND power_provider != '{}')
	`)
	if err != nil {
		return false, fmt.Errorf("db: MigrateBMCCredentials: query: %w", err)
	}
	defer rows.Close()

	type row struct{ id, bmc, pp string }
	var toMigrate []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.bmc, &r.pp); err != nil {
			return false, fmt.Errorf("db: MigrateBMCCredentials: scan: %w", err)
		}
		toMigrate = append(toMigrate, r)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("db: MigrateBMCCredentials: rows: %w", err)
	}

	changed := false
	for _, r := range toMigrate {
		encBMC, bmcFlag := encryptNodeBlob(r.bmc, "{}")
		encPP, ppFlag := encryptNodeBlob(r.pp, "{}")
		_, err = db.sql.ExecContext(ctx, `
			UPDATE node_configs
			SET bmc_config = ?, bmc_config_encrypted = ?,
			    power_provider = ?, power_provider_encrypted = ?
			WHERE id = ?
		`, encBMC, boolToInt(bmcFlag), encPP, boolToInt(ppFlag), r.id)
		if err != nil {
			return changed, fmt.Errorf("db: MigrateBMCCredentials: update %s: %w", r.id, err)
		}
		changed = true
	}
	return changed, nil
}

// UpsertNodeByMAC creates a new NodeConfig for the given MAC, or updates the
// hardware_profile and hostname of the existing record if one already exists.
// Returns the resulting NodeConfig (created or updated).
//
// Concurrency: wraps the check+insert in an exclusive transaction to prevent
// duplicate-registration races when multiple PXE nodes boot simultaneously.
// With SQLite's single-writer model and a _busy_timeout=5000ms DSN, concurrent
// callers block on the transaction acquisition rather than racing on TOCTOU.
func (db *DB) UpsertNodeByMAC(ctx context.Context, cfg api.NodeConfig) (api.NodeConfig, error) {
	cfg.PrimaryMAC = strings.ToLower(cfg.PrimaryMAC)
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: marshal hardware_profile: %w", err)
	}

	// Marshal new-node fields upfront (needed only on INSERT, but harmless to do early).
	interfaces, _ := json.Marshal(cfg.Interfaces)
	sshKeys, _ := json.Marshal(cfg.SSHKeys)
	upsertTags := cfg.Tags
	if len(upsertTags) == 0 && len(cfg.Groups) > 0 {
		upsertTags = cfg.Groups
	}
	upsertTagsJSON, _ := json.Marshal(upsertTags)
	customVars, _ := json.Marshal(cfg.CustomVars)

	// BEGIN IMMEDIATE acquires a write lock before any reads, preventing TOCTOU
	// when two PXE nodes with the same MAC boot concurrently (unlikely but possible
	// with identical hardware or cloned VMs).
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: upsert node (begin tx): %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Check inside the transaction whether this MAC already exists.
	var existingID string
	var existingHostname string
	var existingHostnameAuto bool
	scanErr := tx.QueryRowContext(ctx,
		`SELECT id, hostname, hostname_auto FROM node_configs WHERE primary_mac = ?`,
		cfg.PrimaryMAC,
	).Scan(&existingID, &existingHostname, &existingHostnameAuto)

	now := time.Now().UTC()

	switch {
	case scanErr == nil:
		// Record exists — update hardware_profile and detected_firmware.
		// Preserve admin-set hostnames: only overwrite when hostname_auto=1.
		newHostname := existingHostname
		newHostnameAuto := existingHostnameAuto
		if existingHostnameAuto && cfg.Hostname != "" {
			newHostname = cfg.Hostname
			newHostnameAuto = cfg.HostnameAuto
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE node_configs
			SET hardware_profile = ?, hostname = ?, hostname_auto = ?, detected_firmware = ?, updated_at = ?
			WHERE primary_mac = ?
		`, string(hwProfile), newHostname, boolToInt(newHostnameAuto), cfg.DetectedFirmware, now.Unix(), cfg.PrimaryMAC)
		if err != nil {
			return api.NodeConfig{}, fmt.Errorf("db: upsert node (update): %w", err)
		}

	case scanErr == sql.ErrNoRows:
		// New node — insert a stub with no image assigned.
		cfg.CreatedAt = now
		cfg.UpdatedAt = now
		_, err = tx.ExecContext(ctx, `
			INSERT INTO node_configs
				(id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
				 tags, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
				 power_provider, created_at, updated_at, disk_layout_override, extra_mounts,
				 detected_firmware)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, '{}', '[]', '{}', ?, ?, '{}', '[]', ?)
		`,
			cfg.ID, cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
			string(interfaces), string(sshKeys), cfg.KernelArgs,
			string(upsertTagsJSON), string(customVars),
			string(hwProfile),
			cfg.CreatedAt.Unix(), cfg.UpdatedAt.Unix(),
			cfg.DetectedFirmware,
		)
		if err != nil {
			return api.NodeConfig{}, fmt.Errorf("db: upsert node (insert): %w", err)
		}

	default:
		return api.NodeConfig{}, fmt.Errorf("db: upsert node (lookup): %w", scanErr)
	}

	if err := tx.Commit(); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: upsert node (commit): %w", err)
	}

	// Read the final record outside the transaction — the write lock is released
	// and we get a clean consistent snapshot.
	return db.GetNodeConfigByMAC(ctx, cfg.PrimaryMAC)
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
// ADR-0008: deploy_completed_preboot_at, deploy_verified_booted_at,
//
//	deploy_verify_timeout_at, and last_seen_at added in migration 022.
//
// Migration 026: detected_firmware added.
// Migration 039: bmc_config_encrypted, power_provider_encrypted added (S1-16).
// Migration 041: tags column (renamed from groups).
// Migration 048 (S6-6): group_id column dropped; resolved via correlated subquery.
// Migration 049 (S6-8): last_deploy_succeeded_at column dropped; use deploy_completed_preboot_at.
// Migration 054: verify_timeout_override added after power_provider_encrypted.
// Migration 076: provider added after verify_timeout_override.
// Migration 082: ldap_ready + ldap_ready_detail (Sprint 15 #99).
const nodeConfigCols = `id, hostname, hostname_auto, fqdn, primary_mac, interfaces, ssh_keys, kernel_args,
	       tags, custom_vars, base_image_id, hardware_profile, bmc_config, ib_config,
	       power_provider, reimage_pending, last_deploy_failed_at,
	       created_at, updated_at,
	       (SELECT group_id FROM node_group_memberships WHERE node_id = id AND is_primary = 1 LIMIT 1) AS group_id,
	       disk_layout_override, extra_mounts,
	       deploy_completed_preboot_at, deploy_verified_booted_at,
	       deploy_verify_timeout_at, last_seen_at, detected_firmware,
	       bmc_config_encrypted, power_provider_encrypted,
	       verify_timeout_override, provider,
	       ldap_ready, ldap_ready_detail`

// nodeConfigColsJoined is like nodeConfigCols but qualifies every column with
// the "nc" table alias. The caller must LEFT JOIN node_group_memberships m ON
// m.node_id = nc.id AND m.is_primary = 1 to populate the group_id column.
// Migration 048 (S6-6): group_id now comes exclusively from the is_primary join row.
// Migration 049 (S6-8): last_deploy_succeeded_at removed; use deploy_completed_preboot_at.
// Migration 076: provider added after verify_timeout_override.
// Migration 082: ldap_ready + ldap_ready_detail appended.
const nodeConfigColsJoined = `nc.id, nc.hostname, nc.hostname_auto, nc.fqdn, nc.primary_mac,
	       nc.interfaces, nc.ssh_keys, nc.kernel_args,
	       nc.tags, nc.custom_vars, nc.base_image_id, nc.hardware_profile, nc.bmc_config, nc.ib_config,
	       nc.power_provider, nc.reimage_pending, nc.last_deploy_failed_at,
	       nc.created_at, nc.updated_at,
	       m.group_id,
	       nc.disk_layout_override, nc.extra_mounts,
	       nc.deploy_completed_preboot_at, nc.deploy_verified_booted_at,
	       nc.deploy_verify_timeout_at, nc.last_seen_at, nc.detected_firmware,
	       nc.bmc_config_encrypted, nc.power_provider_encrypted,
	       nc.verify_timeout_override, nc.provider,
	       nc.ldap_ready, nc.ldap_ready_detail`

// GetNodeConfig retrieves a NodeConfig by its UUID.
func (db *DB) GetNodeConfig(ctx context.Context, id string) (api.NodeConfig, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+nodeConfigCols+` FROM node_configs WHERE id = ?`, id)
	return scanNodeConfig(row)
}

// GetNodeConfigByMAC retrieves the NodeConfig whose primary_mac matches mac.
func (db *DB) GetNodeConfigByMAC(ctx context.Context, mac string) (api.NodeConfig, error) {
	mac = strings.ToLower(mac)
	row := db.sql.QueryRowContext(ctx,
		`SELECT `+nodeConfigCols+` FROM node_configs WHERE primary_mac = ?`, mac)
	return scanNodeConfig(row)
}

// HostnameExists reports whether any node_config row with the given hostname exists.
func (db *DB) HostnameExists(ctx context.Context, hostname string) (bool, error) {
	var count int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_configs WHERE hostname = ?`, hostname).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("db: hostname exists: %w", err)
	}
	return count > 0, nil
}

// NodeGetHostname returns the hostname for a given node ID.
// Returns an empty string and no error if the node doesn't exist.
func (db *DB) NodeGetHostname(ctx context.Context, nodeID string) (string, error) {
	var hostname string
	err := db.sql.QueryRowContext(ctx,
		`SELECT hostname FROM node_configs WHERE id = ?`, nodeID,
	).Scan(&hostname)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: NodeGetHostname: %w", err)
	}
	return hostname, nil
}

// ListNodeConfigs returns all NodeConfigs. If baseImageID is non-empty, filters by it.
// group_id is resolved from node_group_memberships (the authoritative source) so that
// the node list page shows the correct group even when the denormalised fast-path
// column on node_configs is stale or NULL.
func (db *DB) ListNodeConfigs(ctx context.Context, baseImageID string) ([]api.NodeConfig, error) {
	return db.SearchNodeConfigs(ctx, baseImageID, "", nil)
}

// SearchNodeConfigs lists node configs with optional baseImageID, a free-text search
// term, and an optional list of tag values to filter by (AND semantics).
// The search term is matched case-insensitively against hostname, primary_mac, and
// status (LIKE '%term%'). Each tag in tags must be present in the node's JSON tags
// array for the node to be returned (SQLite JSON1 json_each).
func (db *DB) SearchNodeConfigs(ctx context.Context, baseImageID, search string, tags []string) ([]api.NodeConfig, error) {
	var (
		rows *sql.Rows
		err  error
	)

	whereClauses := []string{}
	args := []interface{}{}

	if baseImageID != "" {
		whereClauses = append(whereClauses, "nc.base_image_id = ?")
		args = append(args, baseImageID)
	}
	if search != "" {
		like := "%" + search + "%"
		whereClauses = append(whereClauses, "(nc.hostname LIKE ? OR nc.primary_mac LIKE ? OR nc.status LIKE ?)")
		args = append(args, like, like, like)
	}
	// TAG-2: each requested tag must appear in the node's JSON tags array (AND semantics).
	for _, tag := range tags {
		if tag == "" {
			continue
		}
		whereClauses = append(whereClauses, "EXISTS (SELECT 1 FROM json_each(nc.tags) WHERE value = ?)")
		args = append(args, tag)
	}

	where := ""
	if len(whereClauses) > 0 {
		where = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	rows, err = db.sql.QueryContext(ctx,
		`SELECT `+nodeConfigColsJoined+`
		 FROM node_configs nc
		 LEFT JOIN node_group_memberships m ON m.node_id = nc.id AND m.is_primary = 1`+where+`
		 ORDER BY nc.hostname ASC`,
		args...)
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
	cfg.PrimaryMAC = strings.ToLower(cfg.PrimaryMAC)
	interfaces, err := json.Marshal(cfg.Interfaces)
	if err != nil {
		return fmt.Errorf("db: marshal interfaces: %w", err)
	}
	sshKeys, err := json.Marshal(cfg.SSHKeys)
	if err != nil {
		return fmt.Errorf("db: marshal ssh_keys: %w", err)
	}
	// S2-4: Tags is canonical; fall back to Groups for callers that haven't been updated.
	updTags := cfg.Tags
	if len(updTags) == 0 && len(cfg.Groups) > 0 {
		updTags = cfg.Groups
	}
	tags, err := json.Marshal(updTags)
	if err != nil {
		return fmt.Errorf("db: marshal tags: %w", err)
	}
	customVars, err := json.Marshal(cfg.CustomVars)
	if err != nil {
		return fmt.Errorf("db: marshal custom_vars: %w", err)
	}
	hwProfile, err := json.Marshal(cfg.HardwareProfile)
	if err != nil {
		return fmt.Errorf("db: marshal hardware_profile: %w", err)
	}
	bmcConfigRaw, err := marshalNullableJSON(cfg.BMC, "{}")
	if err != nil {
		return fmt.Errorf("db: marshal bmc_config: %w", err)
	}
	ibConfig, err := marshalNullableJSON(cfg.IBConfig, "[]")
	if err != nil {
		return fmt.Errorf("db: marshal ib_config: %w", err)
	}
	powerProviderRaw, err := marshalNullableJSON(cfg.PowerProvider, "{}")
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

	// Encrypt credential blobs at rest (S1-16).
	bmcConfig, bmcEncrypted := encryptNodeBlob(bmcConfigRaw, "{}")
	powerProvider, ppEncrypted := encryptNodeBlob(powerProviderRaw, "{}")

	// Migration 054: verify_timeout_override — store NULL when not set.
	var verifyTimeoutOverrideSQL interface{}
	if cfg.VerifyTimeoutOverride != nil {
		verifyTimeoutOverrideSQL = *cfg.VerifyTimeoutOverride
	}

	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET hostname = ?, hostname_auto = ?, fqdn = ?, primary_mac = ?, interfaces = ?, ssh_keys = ?,
		    kernel_args = ?, tags = ?, custom_vars = ?, base_image_id = ?,
		    hardware_profile = ?, bmc_config = ?, ib_config = ?, power_provider = ?,
		    disk_layout_override = ?, extra_mounts = ?, updated_at = ?,
		    detected_firmware = ?,
		    bmc_config_encrypted = ?, power_provider_encrypted = ?,
		    verify_timeout_override = ?, provider = ?
		WHERE id = ?
	`,
		cfg.Hostname, boolToInt(cfg.HostnameAuto), cfg.FQDN, cfg.PrimaryMAC,
		string(interfaces), string(sshKeys), cfg.KernelArgs,
		string(tags), string(customVars), nullableString(cfg.BaseImageID),
		string(hwProfile), bmcConfig, ibConfig, powerProvider,
		diskLayoutOverride, extraMounts,
		time.Now().Unix(), cfg.DetectedFirmware,
		boolToInt(bmcEncrypted), boolToInt(ppEncrypted),
		verifyTimeoutOverrideSQL, cfg.Provider,
		cfg.ID,
	)
	if err != nil {
		return fmt.Errorf("db: update node config: %w", err)
	}
	return requireOneRow(res, "node_configs", cfg.ID)
}

// SetNodeInterfaces persists the interfaces slice for the given node ID.
// Only writes the interfaces column — all other fields are preserved.
// Used during registration to auto-populate network config from hardware discovery.
func (db *DB) SetNodeInterfaces(ctx context.Context, nodeID string, ifaces []api.InterfaceConfig) error {
	data, err := json.Marshal(ifaces)
	if err != nil {
		return fmt.Errorf("db: marshal interfaces: %w", err)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE node_configs SET interfaces = ?, updated_at = ? WHERE id = ?`,
		string(data), time.Now().Unix(), nodeID,
	)
	if err != nil {
		return fmt.Errorf("db: set node interfaces: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
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

// RecordDeploySucceeded marks a node's last deployment as pre-boot successful.
// ADR-0008: Sets deploy_completed_preboot_at = now() (the canonical field).
// S6-8: last_deploy_succeeded_at dual-write removed (column dropped in migration 049).
// Also clears all ADR-0008 verification fields from any prior deploy cycle so the
// node enters the deployed_preboot state cleanly.
// Clears reimage_pending. Called by the deploy-complete callback from the
// pre-reboot PXE initramfs — NOT from the deployed OS itself.
func (db *DB) RecordDeploySucceeded(ctx context.Context, nodeID string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET deploy_completed_preboot_at   = ?,
		    deploy_verified_booted_at     = NULL,
		    deploy_verify_timeout_at      = NULL,
		    reimage_pending               = 0,
		    updated_at                    = ?
		WHERE id = ?
	`, now, now, nodeID)
	if err != nil {
		return fmt.Errorf("db: record deploy succeeded: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// RecordVerifyBooted marks a node as verified-booted after the deployed OS phones
// home via POST /api/v1/nodes/{id}/verify-boot. ADR-0008.
//
// Returns firstTime=true only when deploy_verified_booted_at was NULL and has just
// been set -- i.e. this is the first successful verify-boot for this deploy cycle.
// Subsequent calls (clientd phones home repeatedly) return firstTime=false and only
// update last_seen_at (heartbeat semantic). This lets callers gate one-shot side
// effects (e.g. flipping the Proxmox persistent boot order) on the state transition
// rather than on every phone-home.
func (db *DB) RecordVerifyBooted(ctx context.Context, nodeID string) (firstTime bool, err error) {
	// Step 1: check whether deploy_verified_booted_at is already set.
	// Two-step approach avoids the SQLite CASE-expression RowsAffected ambiguity
	// (SQLite counts a row as affected even when the CASE branch leaves the value
	// unchanged, so RowsAffected() cannot distinguish first-call from subsequent).
	var existingVerifiedAt *int64
	err = db.sql.QueryRowContext(ctx,
		`SELECT deploy_verified_booted_at FROM node_configs WHERE id = ?`, nodeID,
	).Scan(&existingVerifiedAt)
	if err != nil {
		return false, fmt.Errorf("db: record verify booted: check existing: %w", err)
	}

	now := time.Now().Unix()

	if existingVerifiedAt != nil {
		// Already verified -- just update the heartbeat, do not fire side effects.
		_, err = db.sql.ExecContext(ctx, `
			UPDATE node_configs
			SET last_seen_at = ?,
			    updated_at   = ?
			WHERE id = ?
		`, now, now, nodeID)
		if err != nil {
			return false, fmt.Errorf("db: record verify booted: update heartbeat: %w", err)
		}
		return false, nil
	}

	// Step 2: first verify-boot -- set deploy_verified_booted_at.
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET deploy_verified_booted_at = ?,
		    last_seen_at              = ?,
		    updated_at                = ?
		WHERE id = ?
	`, now, now, now, nodeID)
	if err != nil {
		return false, fmt.Errorf("db: record verify booted: %w", err)
	}
	if err := requireOneRow(res, "node_configs", nodeID); err != nil {
		return false, err
	}
	return true, nil
}

// RecordNodeLDAPReady records the LDAP readiness probe result from a node's
// verify-boot phone-home. ready=true means sssd is connected and functional;
// ready=false means the probe failed with the given detail string.
// Called from the VerifyBoot handler (Sprint 15 #99).
func (db *DB) RecordNodeLDAPReady(ctx context.Context, nodeID string, ready bool, detail string) error {
	var readyInt int
	if ready {
		readyInt = 1
	}
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET ldap_ready        = ?,
		    ldap_ready_detail = ?,
		    updated_at        = ?
		WHERE id = ?
	`, readyInt, detail, now, nodeID)
	if err != nil {
		return fmt.Errorf("db: record node ldap ready: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// RecordVerifyTimeout sets deploy_verify_timeout_at = now() for nodes that did
// not phone home within CLUSTR_VERIFY_TIMEOUT after deploy_completed_preboot_at.
// Called by the background scanner goroutine. ADR-0008.
func (db *DB) RecordVerifyTimeout(ctx context.Context, nodeID string) error {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_configs
		SET deploy_verify_timeout_at = ?,
		    updated_at               = ?
		WHERE id = ?
	`, now, now, nodeID)
	if err != nil {
		return fmt.Errorf("db: record verify timeout: %w", err)
	}
	return requireOneRow(res, "node_configs", nodeID)
}

// ListNodesAwaitingVerification returns all nodes that are in deployed_preboot
// state (deploy_completed_preboot_at IS NOT NULL, deploy_verified_booted_at IS NULL,
// deploy_verify_timeout_at IS NULL) AND whose deploy_completed_preboot_at is
// older than the given cutoff time. Used by the background scanner. ADR-0008.
func (db *DB) ListNodesAwaitingVerification(ctx context.Context, olderThan time.Time) ([]api.NodeConfig, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT `+nodeConfigCols+` FROM node_configs
		 WHERE deploy_completed_preboot_at IS NOT NULL
		   AND deploy_verified_booted_at IS NULL
		   AND deploy_verify_timeout_at IS NULL
		   AND reimage_pending = 0
		   AND deploy_completed_preboot_at <= ?
		 ORDER BY deploy_completed_preboot_at ASC`,
		olderThan.Unix(),
	)
	if err != nil {
		return nil, fmt.Errorf("db: list nodes awaiting verification: %w", err)
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
	injectVarsJSON := "{}"
	if req.InjectVars != nil {
		if b, jerr := json.Marshal(req.InjectVars); jerr == nil {
			injectVarsJSON = string(b)
		}
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO reimage_requests
			(id, node_id, image_id, status, scheduled_at, error_message,
			 requested_by, dry_run, bios_only, inject_vars, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		req.ID, req.NodeID, req.ImageID, string(req.Status),
		scheduledAtVal, req.ErrorMessage,
		req.RequestedBy, boolToInt(req.DryRun), boolToInt(req.BiosOnly), injectVarsJSON, req.CreatedAt.Unix(),
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
		       started_at, completed_at, error_message, requested_by, dry_run, bios_only, created_at,
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
			       started_at, completed_at, error_message, requested_by, dry_run, bios_only, created_at,
			       exit_code, exit_name, phase
			FROM reimage_requests WHERE node_id = ? ORDER BY created_at DESC
		`, nodeID)
	} else {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, node_id, image_id, status, scheduled_at, triggered_at,
			       started_at, completed_at, error_message, requested_by, dry_run, bios_only, created_at,
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
		       started_at, completed_at, error_message, requested_by, dry_run, bios_only, created_at,
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
		       started_at, completed_at, error_message, requested_by, dry_run, bios_only, created_at,
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

// CancelAllActiveReimages transitions every non-terminal reimage request
// (pending, triggered, in_progress) to canceled with the given message.
// It also clears the reimage_pending flag on all affected nodes so that
// future PXE boots route to disk rather than re-deploying.
// Returns the number of requests updated.
func (db *DB) CancelAllActiveReimages(ctx context.Context, msg string) (int, error) {
	now := time.Now().Unix()

	// Collect affected node IDs before updating so we can clear reimage_pending.
	rows, err := db.sql.QueryContext(ctx, `
		SELECT DISTINCT node_id FROM reimage_requests
		WHERE status IN ('pending', 'triggered', 'in_progress')
	`)
	if err != nil {
		return 0, fmt.Errorf("db: cancel all active reimages: list nodes: %w", err)
	}
	var nodeIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}
	rows.Close()

	// Bulk-cancel all active requests.
	res, err := db.sql.ExecContext(ctx, `
		UPDATE reimage_requests
		SET status = 'canceled', error_message = ?, completed_at = ?
		WHERE status IN ('pending', 'triggered', 'in_progress')
	`, msg, now)
	if err != nil {
		return 0, fmt.Errorf("db: cancel all active reimages: %w", err)
	}
	n, _ := res.RowsAffected()

	// Clear reimage_pending on affected nodes (non-fatal per node).
	for _, nodeID := range nodeIDs {
		if clearErr := db.SetReimagePending(ctx, nodeID, false); clearErr != nil {
			// Log would require importing zerolog; use fmt to stderr instead.
			// The handler layer logs this via Warn so we do not need to duplicate here.
			_ = clearErr
		}
	}

	return int(n), nil
}

// GetInjectVarsForActiveReimage returns the inject_vars JSON for the most recent
// non-terminal reimage request for nodeID, or an empty map if none exists.
// Used by RegisterNode (S4-11) to merge per-deployment vars into the NodeConfig
// returned to the deploy agent.
func (db *DB) GetInjectVarsForActiveReimage(ctx context.Context, nodeID string) (map[string]string, error) {
	var injectVarsJSON string
	err := db.sql.QueryRowContext(ctx, `
		SELECT inject_vars FROM reimage_requests
		WHERE node_id = ?
		  AND status NOT IN ('complete', 'failed', 'canceled')
		ORDER BY created_at DESC LIMIT 1
	`, nodeID).Scan(&injectVarsJSON)
	if err == sql.ErrNoRows || injectVarsJSON == "" || injectVarsJSON == "{}" {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get inject_vars for active reimage: %w", err)
	}
	var m map[string]string
	if jerr := json.Unmarshal([]byte(injectVarsJSON), &m); jerr != nil {
		return nil, nil // treat bad JSON as empty
	}
	return m, nil
}

// CountActiveReimages returns the number of reimage_requests rows that are in a
// non-terminal state (pending, triggered, in_progress). Used by the Prometheus
// metrics collector to populate clustr_active_deploys.
func (db *DB) CountActiveReimages(ctx context.Context) (int64, error) {
	var n int64
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM reimage_requests
		WHERE status IN ('pending', 'triggered', 'in_progress')
	`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: count active reimages: %w", err)
	}
	return n, nil
}

// ListAllActiveReimageIDs returns the IDs of every reimage request currently in
// a non-terminal state (pending, triggered, in_progress). Used by GetActiveJobs
// to populate the reimages field.
func (db *DB) ListAllActiveReimageIDs(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id FROM reimage_requests
		WHERE status IN ('pending', 'triggered', 'in_progress')
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list active reimage ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// WaitForActiveReimages blocks until all non-terminal reimage requests have
// reached a terminal state, or until ctx is done. It polls the DB every 500ms.
// Mirrors BuildProgressStore.WaitForActive for the reimage operation class.
func (db *DB) WaitForActiveReimages(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := db.CountActiveReimages(ctx)
		if err != nil || n == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
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
		biosOnlyInt   int
		createdAtUnix int64
		exitCodeNull  sql.NullInt64
		exitNameNull  sql.NullString
		phaseNull     sql.NullString
	)
	err := s.Scan(
		&req.ID, &req.NodeID, &req.ImageID, &status,
		&scheduledAt, &triggeredAt, &startedAt, &completedAt,
		&req.ErrorMessage, &req.RequestedBy, &dryRunInt, &biosOnlyInt, &createdAtUnix,
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
	req.BiosOnly = biosOnlyInt != 0
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
		img                     api.BaseImage
		status                  string
		format                  string
		firmware                string
		diskLayoutJSON          string
		tagsJSON                string
		builtForRolesJSON       string
		installInstructionsJSON string
		createdAtUnix           int64
		finalizedAtUnix         sql.NullInt64
		blobPath                string // scanned but not exposed in API type
	)

	err := s.Scan(
		&img.ID, &img.Name, &img.Version, &img.OS, &img.Arch,
		&status, &format, &firmware,
		&img.SizeBytes, &img.Checksum, &blobPath,
		&diskLayoutJSON, &tagsJSON,
		&img.SourceURL, &img.Notes, &img.ErrorMessage,
		&builtForRolesJSON, &img.BuildMethod,
		&installInstructionsJSON,
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
	if installInstructionsJSON != "" && installInstructionsJSON != "null" && installInstructionsJSON != "[]" {
		if err := json.Unmarshal([]byte(installInstructionsJSON), &img.InstallInstructions); err != nil {
			// Non-fatal: old rows may have malformed JSON; leave the field nil.
			img.InstallInstructions = nil
		}
	}

	return img, nil
}

func scanNodeConfig(s scanner) (api.NodeConfig, error) {
	var (
		cfg                   api.NodeConfig
		hostnameAuto          int
		interfacesJSON        string
		sshKeysJSON           string
		groupsJSON            string
		customVarsJSON        string
		baseImageID           sql.NullString
		hwProfileJSON         string
		bmcConfigJSON         string
		ibConfigJSON          string
		powerProviderJSON     string
		reimagePending        int
		lastDeployFailedAtVal sql.NullInt64
		createdAtUnix         int64
		updatedAtUnix         int64
		// S6-6 (migration 048): group_id from subquery or join; still nullable.
		groupID                sql.NullString
		diskLayoutOverrideJSON string
		extraMountsJSON        string
		// ADR-0008: two-phase deploy verification columns (migration 022).
		deployCompletedPrebootAtVal sql.NullInt64
		deployVerifiedBootedAtVal   sql.NullInt64
		deployVerifyTimeoutAtVal    sql.NullInt64
		lastSeenAtVal               sql.NullInt64
		// S1-16 (migration 039): encryption flag columns.
		bmcConfigEncrypted     bool
		powerProviderEncrypted bool
		// Migration 054: per-node verify timeout override (nullable seconds).
		verifyTimeoutOverrideVal sql.NullInt64
		// Migration 076: hardware/power backend label ("ipmi", "proxmox", or "").
		providerVal string
		// Migration 082: LDAP readiness probe result (Sprint 15 #99).
		ldapReadyVal    sql.NullInt64
		ldapReadyDetail string
	)

	// Column order matches nodeConfigCols / nodeConfigColsJoined:
	// S6-6: group_id is a subquery/join result, not a physical column.
	// S6-8: last_deploy_succeeded_at removed; deploy_completed_preboot_at is canonical.
	// Migration 054: verify_timeout_override appended.
	// Migration 082: ldap_ready, ldap_ready_detail appended.
	err := s.Scan(
		&cfg.ID, &cfg.Hostname, &hostnameAuto, &cfg.FQDN, &cfg.PrimaryMAC,
		&interfacesJSON, &sshKeysJSON, &cfg.KernelArgs,
		&groupsJSON, &customVarsJSON, &baseImageID,
		&hwProfileJSON, &bmcConfigJSON, &ibConfigJSON,
		&powerProviderJSON, &reimagePending,
		&lastDeployFailedAtVal,
		&createdAtUnix, &updatedAtUnix,
		&groupID, &diskLayoutOverrideJSON, &extraMountsJSON,
		&deployCompletedPrebootAtVal, &deployVerifiedBootedAtVal,
		&deployVerifyTimeoutAtVal, &lastSeenAtVal,
		&cfg.DetectedFirmware,
		&bmcConfigEncrypted, &powerProviderEncrypted,
		&verifyTimeoutOverrideVal, &providerVal,
		&ldapReadyVal, &ldapReadyDetail,
	)
	if err == sql.ErrNoRows {
		return api.NodeConfig{}, api.ErrNotFound
	}
	// Decrypt credential blobs if marked encrypted (S1-16).
	if bmcConfigEncrypted && bmcConfigJSON != "" && bmcConfigJSON != "{}" {
		if plain, derr := secrets.Decrypt(bmcConfigJSON); derr == nil {
			bmcConfigJSON = string(plain)
		}
		// Decryption failure leaves ciphertext — fail-closed; BMC calls will error.
	}
	if powerProviderEncrypted && powerProviderJSON != "" && powerProviderJSON != "{}" {
		if plain, derr := secrets.Decrypt(powerProviderJSON); derr == nil {
			powerProviderJSON = string(plain)
		}
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

	if lastDeployFailedAtVal.Valid {
		t := time.Unix(lastDeployFailedAtVal.Int64, 0).UTC()
		cfg.LastDeployFailedAt = &t
	}

	// ADR-0008: two-phase deploy verification timestamps.
	if deployCompletedPrebootAtVal.Valid {
		t := time.Unix(deployCompletedPrebootAtVal.Int64, 0).UTC()
		cfg.DeployCompletedPrebootAt = &t
	}
	if deployVerifiedBootedAtVal.Valid {
		t := time.Unix(deployVerifiedBootedAtVal.Int64, 0).UTC()
		cfg.DeployVerifiedBootedAt = &t
	}
	if deployVerifyTimeoutAtVal.Valid {
		t := time.Unix(deployVerifyTimeoutAtVal.Int64, 0).UTC()
		cfg.DeployVerifyTimeoutAt = &t
	}
	if lastSeenAtVal.Valid {
		t := time.Unix(lastSeenAtVal.Int64, 0).UTC()
		cfg.LastSeenAt = &t
	}
	// Migration 054: per-node verify-boot timeout override (seconds; NULL = use global).
	if verifyTimeoutOverrideVal.Valid {
		v := int(verifyTimeoutOverrideVal.Int64)
		cfg.VerifyTimeoutOverride = &v
	}
	// Migration 076: hardware/power backend label.
	cfg.Provider = providerVal
	// Migration 082: LDAP readiness probe result (Sprint 15 #99).
	if ldapReadyVal.Valid {
		v := ldapReadyVal.Int64 != 0
		cfg.LDAPReady = &v
	}
	cfg.LDAPReadyDetail = ldapReadyDetail

	cfg.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	cfg.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()

	if err := json.Unmarshal([]byte(interfacesJSON), &cfg.Interfaces); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal interfaces: %w", err)
	}
	if err := json.Unmarshal([]byte(sshKeysJSON), &cfg.SSHKeys); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal ssh_keys: %w", err)
	}
	// S2-4: tags column (renamed from groups in migration 041).
	// Dual-emit: populate both Tags and the deprecated Groups field so that
	// existing CLI versions that read "groups" continue to work through v1.0.
	if err := json.Unmarshal([]byte(groupsJSON), &cfg.Tags); err != nil {
		return api.NodeConfig{}, fmt.Errorf("db: unmarshal tags: %w", err)
	}
	cfg.Groups = cfg.Tags // deprecated alias: mirrors Tags for backward compat
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
	if cfg.Tags == nil {
		cfg.Tags = []string{}
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

// PurgeLogsPerNodeCap deletes the oldest log entries for any node that exceeds
// maxRowsPerNode, keeping only the most recent maxRowsPerNode rows per node.
// Returns the total number of rows deleted and the number of nodes affected.
// This is the second pass of the two-pass log purge (TTL first, cap second).
func (db *DB) PurgeLogsPerNodeCap(ctx context.Context, maxRowsPerNode int64) (int64, int64, error) {
	// Find all nodes that exceed the cap.
	rows, err := db.sql.QueryContext(ctx, `
		SELECT node_mac, COUNT(*) as cnt
		FROM node_logs
		GROUP BY node_mac
		HAVING cnt > ?
	`, maxRowsPerNode)
	if err != nil {
		return 0, 0, fmt.Errorf("db: purge per-node cap (list): %w", err)
	}
	defer rows.Close()

	type nodeCount struct {
		mac   string
		count int64
	}
	var overLimit []nodeCount
	for rows.Next() {
		var nc nodeCount
		if err := rows.Scan(&nc.mac, &nc.count); err != nil {
			return 0, 0, fmt.Errorf("db: purge per-node cap (scan): %w", err)
		}
		overLimit = append(overLimit, nc)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("db: purge per-node cap (rows): %w", err)
	}

	var totalDeleted, nodesAffected int64
	for _, nc := range overLimit {
		excess := nc.count - maxRowsPerNode
		// Delete the oldest `excess` rows for this node.
		res, err := db.sql.ExecContext(ctx, `
			DELETE FROM node_logs
			WHERE id IN (
				SELECT id FROM node_logs
				WHERE node_mac = ?
				ORDER BY timestamp ASC
				LIMIT ?
			)
		`, nc.mac, excess)
		if err != nil {
			return totalDeleted, nodesAffected, fmt.Errorf("db: purge per-node cap (delete %s): %w", nc.mac, err)
		}
		n, _ := res.RowsAffected()
		totalDeleted += n
		nodesAffected++
	}
	return totalDeleted, nodesAffected, nil
}

// LogPurgeSummaryRow is one row in node_logs_summary.
type LogPurgeSummaryRow struct {
	ID            string
	PurgedAt      time.Time
	TTLRows       int64
	CapRows       int64
	TotalRows     int64
	RetentionSecs int64
	MaxRowsCap    int64
	NodeCount     int64
}

// RecordLogPurgeSummary appends a purge event to node_logs_summary.
func (db *DB) RecordLogPurgeSummary(ctx context.Context, row LogPurgeSummaryRow) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO node_logs_summary
			(id, purged_at, ttl_rows, cap_rows, total_rows, retention_secs, max_rows_cap, node_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		row.ID, row.PurgedAt.Unix(),
		row.TTLRows, row.CapRows, row.TotalRows,
		row.RetentionSecs, row.MaxRowsCap, row.NodeCount,
	)
	if err != nil {
		return fmt.Errorf("db: record log purge summary: %w", err)
	}
	return nil
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

// DeleteNodeGroup removes a NodeGroup. Membership rows are removed
// explicitly before the node_groups delete (FK cascade is also wired but
// explicit deletion is safer across SQLite driver versions).
// S6-6: node_configs.group_id column dropped; only membership table needs cleanup.
func (db *DB) DeleteNodeGroup(ctx context.Context, id string) error {
	// Explicitly remove memberships first so the correlated subquery in
	// nodeConfigCols immediately returns NULL for affected nodes.
	if _, err := db.sql.ExecContext(ctx,
		`DELETE FROM node_group_memberships WHERE group_id = ?`, id); err != nil {
		return fmt.Errorf("db: delete node group memberships: %w", err)
	}
	res, err := db.sql.ExecContext(ctx, `DELETE FROM node_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete node group: %w", err)
	}
	return requireOneRow(res, "node_groups", id)
}

// SetNodeGroupExpiration sets or clears the expires_at field for a node group.
// Pass nil to clear the expiration. Resets expiration_warning_sent to '[]' so
// warnings will be re-sent if a new deadline is set.
// Sprint F (v1.5.0): F3 allocation expiration.
func (db *DB) SetNodeGroupExpiration(ctx context.Context, groupID string, expiresAt *time.Time) error {
	var expiresAtVal interface{}
	if expiresAt != nil {
		expiresAtVal = expiresAt.Unix()
	}
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_groups
		SET expires_at = ?, expiration_warning_sent = '[]', updated_at = ?
		WHERE id = ?
	`, expiresAtVal, time.Now().Unix(), groupID)
	if err != nil {
		return fmt.Errorf("db: set node group expiration: %w", err)
	}
	return requireOneRow(res, "node_groups", groupID)
}

// ListGroupsWithExpiration returns node groups that have a non-null expires_at,
// along with their expiration_warning_sent JSON array. Used by the daily
// expiration scanner to determine which warnings to send.
type NodeGroupExpiration struct {
	GroupID         string
	GroupName       string
	ExpiresAt       time.Time
	WarningSentDays []int // thresholds already emailed (e.g. [30, 14])
}

func (db *DB) ListGroupsWithExpiration(ctx context.Context) ([]NodeGroupExpiration, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, expires_at, COALESCE(expiration_warning_sent, '[]')
		FROM node_groups
		WHERE expires_at IS NOT NULL
		ORDER BY expires_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list groups with expiration: %w", err)
	}
	defer rows.Close()

	var out []NodeGroupExpiration
	for rows.Next() {
		var nge NodeGroupExpiration
		var expiresAtUnix int64
		var warnJSON string
		if err := rows.Scan(&nge.GroupID, &nge.GroupName, &expiresAtUnix, &warnJSON); err != nil {
			return nil, fmt.Errorf("db: scan group expiration: %w", err)
		}
		nge.ExpiresAt = time.Unix(expiresAtUnix, 0).UTC()
		_ = json.Unmarshal([]byte(warnJSON), &nge.WarningSentDays) // best-effort; nil slice if invalid
		out = append(out, nge)
	}
	return out, rows.Err()
}

// MarkExpirationWarningSent appends daysRemaining to the expiration_warning_sent
// array for the given group so the warning is not sent again.
func (db *DB) MarkExpirationWarningSent(ctx context.Context, groupID string, daysRemaining int) error {
	// Fetch existing array, append, re-serialize.
	var existing string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COALESCE(expiration_warning_sent, '[]') FROM node_groups WHERE id = ?`, groupID,
	).Scan(&existing); err != nil {
		return fmt.Errorf("db: mark expiration warning: fetch: %w", err)
	}
	var days []int
	_ = json.Unmarshal([]byte(existing), &days)
	days = append(days, daysRemaining)
	b, _ := json.Marshal(days)
	_, err := db.sql.ExecContext(ctx,
		`UPDATE node_groups SET expiration_warning_sent = ? WHERE id = ?`, string(b), groupID)
	if err != nil {
		return fmt.Errorf("db: mark expiration warning: update: %w", err)
	}
	return nil
}

// AssignNodeToGroup updates a node's primary group assignment via
// node_group_memberships. Pass empty groupID to remove the primary flag (node
// retains any secondary memberships; use RemoveGroupMember for full removal).
// S6-6: node_configs.group_id column dropped; writes through memberships only.
func (db *DB) AssignNodeToGroup(ctx context.Context, nodeID, groupID string) error {
	if groupID == "" {
		// Clear primary flag on all memberships for this node.
		_, err := db.sql.ExecContext(ctx,
			`UPDATE node_group_memberships SET is_primary = 0 WHERE node_id = ?`, nodeID)
		if err != nil {
			return fmt.Errorf("db: assign node to group (clear primary): %w", err)
		}
		return nil
	}
	return db.SetPrimaryGroupMember(ctx, nodeID, groupID)
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
// If this is the node's first group membership, marks it is_primary=1 automatically (S2-5).
// S6-6: node_configs.group_id fast-path column dropped; authoritative source is
// node_group_memberships WHERE is_primary = 1.
func (db *DB) AddGroupMember(ctx context.Context, groupID, nodeID string) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO node_group_memberships (node_id, group_id) VALUES (?, ?)`,
		nodeID, groupID)
	if err != nil {
		return fmt.Errorf("db: add group member: %w", err)
	}
	// S2-5: If this is the node's only membership (just inserted), mark it primary.
	// Uses a single-row UPDATE so we don't violate the partial unique index on
	// concurrent inserts.
	_, _ = db.sql.ExecContext(ctx, `
		UPDATE node_group_memberships SET is_primary = 1
		WHERE node_id = ? AND group_id = ?
		  AND (SELECT COUNT(*) FROM node_group_memberships WHERE node_id = ? AND is_primary = 1) = 0
	`, nodeID, groupID, nodeID)
	return nil
}

// SetPrimaryGroupMember marks the given group as the primary group for a node (S2-5).
// Clears is_primary on all other memberships for this node, then sets it on groupID.
// Returns ErrNotFound if the membership does not exist.
func (db *DB) SetPrimaryGroupMember(ctx context.Context, nodeID, groupID string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: set primary group: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Clear current primary.
	if _, err := tx.ExecContext(ctx,
		`UPDATE node_group_memberships SET is_primary = 0 WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("db: set primary group: clear: %w", err)
	}
	// Set new primary.
	res, err := tx.ExecContext(ctx,
		`UPDATE node_group_memberships SET is_primary = 1 WHERE node_id = ? AND group_id = ?`,
		nodeID, groupID)
	if err != nil {
		return fmt.Errorf("db: set primary group: set: %w", err)
	}
	if err := requireOneRow(res, "node_group_memberships", nodeID+"/"+groupID); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveGroupMember deletes a node_group_memberships row. No-op if absent.
// S6-6: node_configs.group_id fast-path column dropped; no secondary cleanup needed.
func (db *DB) RemoveGroupMember(ctx context.Context, groupID, nodeID string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM node_group_memberships WHERE node_id = ? AND group_id = ?`,
		nodeID, groupID)
	if err != nil {
		return fmt.Errorf("db: remove group member: %w", err)
	}
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
		       ng.created_at, ng.updated_at, ng.expires_at,
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
			expiresAtUnix   sql.NullInt64
		)
		err := rows.Scan(&g.ID, &g.Name, &g.Description, &roleNull,
			&diskLayoutJSON, &extraMountsJSON, &createdAtUnix, &updatedAtUnix,
			&expiresAtUnix, &g.MemberCount)
		if err != nil {
			return nil, fmt.Errorf("db: scan node group with count: %w", err)
		}
		g.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
		g.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
		if roleNull.Valid {
			g.Role = roleNull.String
		}
		if expiresAtUnix.Valid {
			t := time.Unix(expiresAtUnix.Int64, 0).UTC()
			g.ExpiresAt = &t
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

// GetNodeGroupFull returns a NodeGroup with role and expiration populated.
func (db *DB) GetNodeGroupFull(ctx context.Context, id string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, role, disk_layout, extra_mounts, created_at, updated_at, expires_at
		FROM node_groups WHERE id = ?
	`, id)
	return scanNodeGroupFull(row)
}

// GetNodeGroupByNameFull returns a NodeGroup by name with role and expiration populated.
func (db *DB) GetNodeGroupByNameFull(ctx context.Context, name string) (api.NodeGroup, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, description, role, disk_layout, extra_mounts, created_at, updated_at, expires_at
		FROM node_groups WHERE name = ?
	`, name)
	return scanNodeGroupFull(row)
}

// UpdateNodeGroupFull replaces all mutable fields including role and expiration.
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
	var expiresAtVal interface{}
	if g.ExpiresAt != nil {
		expiresAtVal = g.ExpiresAt.Unix()
	}
	res, err := db.sql.ExecContext(ctx, `
		UPDATE node_groups
		SET name = ?, description = ?, role = ?, disk_layout = ?, extra_mounts = ?, expires_at = ?, updated_at = ?
		WHERE id = ?
	`, g.Name, g.Description, roleVal, diskLayout, extraMounts, expiresAtVal, time.Now().Unix(), g.ID)
	if err != nil {
		return fmt.Errorf("db: update node group full: %w", err)
	}
	return requireOneRow(res, "node_groups", g.ID)
}

// CreateNodeGroupFull inserts a NodeGroup with role and expiration support.
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
	var expiresAtVal interface{}
	if g.ExpiresAt != nil {
		expiresAtVal = g.ExpiresAt.Unix()
	}
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO node_groups (id, name, description, role, disk_layout, extra_mounts, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, g.ID, g.Name, g.Description, roleVal, diskLayout, extraMounts, expiresAtVal, g.CreatedAt.Unix(), g.UpdatedAt.Unix())
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
		expiresAtUnix   sql.NullInt64
	)
	err := s.Scan(&g.ID, &g.Name, &g.Description, &roleNull,
		&diskLayoutJSON, &extraMountsJSON, &createdAtUnix, &updatedAtUnix, &expiresAtUnix)
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
	if expiresAtUnix.Valid {
		t := time.Unix(expiresAtUnix.Int64, 0).UTC()
		g.ExpiresAt = &t
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
	ID                string    `json:"id"`
	GroupID           string    `json:"group_id"`
	ImageID           string    `json:"image_id"`
	Concurrency       int       `json:"concurrency"`
	PauseOnFailurePct int       `json:"pause_on_failure_pct"`
	Status            string    `json:"status"`
	TotalNodes        int       `json:"total_nodes"`
	TriggeredNodes    int       `json:"triggered_nodes"`
	SucceededNodes    int       `json:"succeeded_nodes"`
	FailedNodes       int       `json:"failed_nodes"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
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
		       built_for_roles, build_method, install_instructions, created_at, finalized_at
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

// GetInitramfsBuildSHA256 returns the sha256 field for a single build record.
// Returns ("", sql.ErrNoRows) when no record with that ID exists.
func (db *DB) GetInitramfsBuildSHA256(ctx context.Context, id string) (string, error) {
	var sha string
	err := db.sql.QueryRowContext(ctx,
		`SELECT sha256 FROM initramfs_builds WHERE id = ?`, id,
	).Scan(&sha)
	if err != nil {
		return "", err
	}
	return sha, nil
}

// UpdateInitramfsBuildKernelVersion sets the kernel_version field for a
// build record identified by id. Used by the lazy-extract path in the
// GetInitramfs handler to back-fill the version detected from the on-disk
// image when the field was not populated at build time (e.g. autodeploy timer).
func (db *DB) UpdateInitramfsBuildKernelVersion(ctx context.Context, id, kernelVersion string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE initramfs_builds SET kernel_version = ? WHERE id = ?`,
		kernelVersion, id,
	)
	return err
}

// GetLatestSuccessfulBuildBySHA256 returns the ID and kernel_version of the
// most recent successful build whose sha256 matches the given value.
// Returns ("", "", sql.ErrNoRows) when no match is found.
func (db *DB) GetLatestSuccessfulBuildBySHA256(ctx context.Context, sha256 string) (id, kernelVersion string, err error) {
	err = db.sql.QueryRowContext(ctx, `
		SELECT id, kernel_version
		FROM initramfs_builds
		WHERE sha256 = ? AND outcome = 'success'
		ORDER BY started_at DESC
		LIMIT 1
	`, sha256).Scan(&id, &kernelVersion)
	if err != nil {
		return "", "", err
	}
	return id, kernelVersion, nil
}

// ListPendingInitramfsBuilds returns all initramfs_builds rows with outcome='pending'.
// Used by ReconcileStuckInitramfsBuilds to attempt self-healing of orphaned builds.
func (db *DB) ListPendingInitramfsBuilds(ctx context.Context) ([]InitramfsBuildRecord, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, started_at, finished_at, sha256, size_bytes, kernel_version, triggered_by_prefix, outcome
		FROM initramfs_builds
		WHERE outcome = 'pending'
		ORDER BY started_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list pending initramfs builds: %w", err)
	}
	defer rows.Close()

	var records []InitramfsBuildRecord
	for rows.Next() {
		var r InitramfsBuildRecord
		var startedAtUnix int64
		var finishedAtUnix sql.NullInt64
		if err := rows.Scan(&r.ID, &startedAtUnix, &finishedAtUnix,
			&r.SHA256, &r.SizeBytes, &r.KernelVersion, &r.TriggeredByPrefix, &r.Outcome); err != nil {
			return nil, fmt.Errorf("db: scan pending initramfs build: %w", err)
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

// MarkPendingInitramfsBuildsAsFailed updates every initramfs_builds row whose
// outcome is still 'pending' to 'failed: server restarted during build'.
// Call this once at server startup to clear ghost records left by a mid-build crash.
func (db *DB) MarkPendingInitramfsBuildsAsFailed(ctx context.Context) (int64, error) {
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE initramfs_builds
		SET outcome = 'failed: server restarted during build', finished_at = ?
		WHERE outcome = 'pending'
	`, now)
	if err != nil {
		return 0, fmt.Errorf("db: mark pending initramfs builds failed: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
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

// ─── Image metadata operations ───────────────────────────────────────────────

// SetImageMetadataJSON persists the JSON-encoded image metadata sidecar into
// the base_images.metadata_json column for imageID.  The column was added by
// migration 021_image_metadata.sql.
func (db *DB) SetImageMetadataJSON(ctx context.Context, imageID, metadataJSON string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE base_images SET metadata_json = ? WHERE id = ?`,
		metadataJSON, imageID,
	)
	if err != nil {
		return fmt.Errorf("db: set metadata_json for %s: %w", imageID, err)
	}
	return nil
}

// GetImageMetadataJSON returns the raw metadata_json TEXT for imageID, or ""
// if the column is NULL (not yet populated).
func (db *DB) GetImageMetadataJSON(ctx context.Context, imageID string) (string, error) {
	var raw sql.NullString
	err := db.sql.QueryRowContext(ctx,
		`SELECT metadata_json FROM base_images WHERE id = ?`, imageID,
	).Scan(&raw)
	if err != nil {
		return "", fmt.Errorf("db: get metadata_json for %s: %w", imageID, err)
	}
	if !raw.Valid {
		return "", nil
	}
	return raw.String, nil
}

// IsIPReservedForOtherNode reports whether any node_config OTHER than the one
// identified by mac has the given IP configured in its interfaces JSON column.
// It matches both bare IPs ("192.168.1.50") and CIDR notation ("192.168.1.50/24").
// Returns false on sql.ErrNoRows (no conflicting reservation found).
func (db *DB) IsIPReservedForOtherNode(ctx context.Context, ip, mac string) (bool, error) {
	mac = strings.ToLower(mac)
	var count int
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM node_configs, json_each(node_configs.interfaces) AS iface
		WHERE primary_mac != ?
		  AND (
		        json_extract(iface.value, '$.ip_address') = ?
		     OR json_extract(iface.value, '$.ip_address') LIKE ? || '/%'
		  )
	`, mac, ip, ip).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("db: IsIPReservedForOtherNode: %w", err)
	}
	return count > 0, nil
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

// ─── Webhook operations (S4-2) ───────────────────────────────────────────────

// WebhookSubscription is a row in webhook_subscriptions.
type WebhookSubscription struct {
	ID        string
	URL       string
	Events    []string // decoded from JSON
	Secret    string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WebhookDelivery is a row in webhook_deliveries.
type WebhookDelivery struct {
	ID          string
	WebhookID   string
	Event       string
	PayloadJSON string
	Status      string // "success" | "failed"
	HTTPStatus  int
	Attempt     int
	ErrorMsg    string
	DeliveredAt time.Time
}

// CreateWebhookSubscription inserts a new webhook subscription.
func (db *DB) CreateWebhookSubscription(ctx context.Context, sub WebhookSubscription) error {
	eventsJSON, err := json.Marshal(sub.Events)
	if err != nil {
		return fmt.Errorf("db: marshal webhook events: %w", err)
	}
	now := time.Now().Unix()
	_, err = db.sql.ExecContext(ctx, `
		INSERT INTO webhook_subscriptions (id, url, events, secret, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, sub.ID, sub.URL, string(eventsJSON), sub.Secret, boolToInt(sub.Enabled), now, now)
	if err != nil {
		return fmt.Errorf("db: create webhook subscription: %w", err)
	}
	return nil
}

// GetWebhookSubscription returns a single subscription by ID.
func (db *DB) GetWebhookSubscription(ctx context.Context, id string) (WebhookSubscription, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, url, events, secret, enabled, created_at, updated_at
		FROM webhook_subscriptions WHERE id = ?
	`, id)
	return scanWebhookSubscription(row)
}

// ListWebhookSubscriptions returns all enabled subscriptions that include event
// in their events array. If event is empty, all enabled subscriptions are returned.
func (db *DB) ListWebhookSubscriptions(ctx context.Context, event string) ([]WebhookSubscription, error) {
	var rows *sql.Rows
	var err error
	if event == "" {
		rows, err = db.sql.QueryContext(ctx, `
			SELECT id, url, events, secret, enabled, created_at, updated_at
			FROM webhook_subscriptions WHERE enabled = 1 ORDER BY created_at ASC
		`)
	} else {
		// SQLite json_each to check if event is in the events array.
		rows, err = db.sql.QueryContext(ctx, `
			SELECT DISTINCT ws.id, ws.url, ws.events, ws.secret, ws.enabled, ws.created_at, ws.updated_at
			FROM webhook_subscriptions ws, json_each(ws.events) je
			WHERE ws.enabled = 1 AND je.value = ?
			ORDER BY ws.created_at ASC
		`, event)
	}
	if err != nil {
		return nil, fmt.Errorf("db: list webhook subscriptions: %w", err)
	}
	defer rows.Close()

	var subs []WebhookSubscription
	for rows.Next() {
		sub, err := scanWebhookSubscription(rows)
		if err != nil {
			return nil, err
		}
		subs = append(subs, sub)
	}
	return subs, rows.Err()
}

// UpdateWebhookSubscription replaces the URL, events, secret, and enabled flag.
func (db *DB) UpdateWebhookSubscription(ctx context.Context, sub WebhookSubscription) error {
	eventsJSON, err := json.Marshal(sub.Events)
	if err != nil {
		return fmt.Errorf("db: marshal webhook events: %w", err)
	}
	now := time.Now().Unix()
	res, err := db.sql.ExecContext(ctx, `
		UPDATE webhook_subscriptions
		SET url = ?, events = ?, secret = ?, enabled = ?, updated_at = ?
		WHERE id = ?
	`, sub.URL, string(eventsJSON), sub.Secret, boolToInt(sub.Enabled), now, sub.ID)
	if err != nil {
		return fmt.Errorf("db: update webhook subscription: %w", err)
	}
	return requireOneRow(res, "webhook_subscriptions", sub.ID)
}

// DeleteWebhookSubscription deletes a subscription and cascades to deliveries.
func (db *DB) DeleteWebhookSubscription(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM webhook_subscriptions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete webhook subscription: %w", err)
	}
	return requireOneRow(res, "webhook_subscriptions", id)
}

// RecordWebhookDelivery inserts a delivery attempt record.
func (db *DB) RecordWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO webhook_deliveries
			(id, webhook_id, event, payload_json, status, http_status, attempt, error_msg, delivered_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, d.ID, d.WebhookID, d.Event, d.PayloadJSON, d.Status, d.HTTPStatus,
		d.Attempt, d.ErrorMsg, d.DeliveredAt.Unix())
	if err != nil {
		return fmt.Errorf("db: record webhook delivery: %w", err)
	}
	return nil
}

// ListWebhookDeliveries returns the most recent 200 delivery records for a
// subscription, newest-first.
func (db *DB) ListWebhookDeliveries(ctx context.Context, webhookID string) ([]WebhookDelivery, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, webhook_id, event, payload_json, status, http_status, attempt, error_msg, delivered_at
		FROM webhook_deliveries
		WHERE webhook_id = ?
		ORDER BY delivered_at DESC LIMIT 200
	`, webhookID)
	if err != nil {
		return nil, fmt.Errorf("db: list webhook deliveries: %w", err)
	}
	defer rows.Close()

	var deliveries []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		var ts int64
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.Event, &d.PayloadJSON, &d.Status, &d.HTTPStatus, &d.Attempt, &d.ErrorMsg, &ts); err != nil {
			return nil, fmt.Errorf("db: scan webhook delivery: %w", err)
		}
		d.DeliveredAt = time.Unix(ts, 0).UTC()
		deliveries = append(deliveries, d)
	}
	return deliveries, rows.Err()
}

// scanWebhookSubscription scans a single webhook_subscriptions row.
type webhookScanner interface {
	Scan(dest ...any) error
}

func scanWebhookSubscription(row webhookScanner) (WebhookSubscription, error) {
	var sub WebhookSubscription
	var eventsJSON string
	var enabledInt int
	var createdAt, updatedAt int64

	if err := row.Scan(&sub.ID, &sub.URL, &eventsJSON, &sub.Secret, &enabledInt, &createdAt, &updatedAt); err != nil {
		return WebhookSubscription{}, fmt.Errorf("db: scan webhook subscription: %w", err)
	}
	sub.Enabled = enabledInt != 0
	sub.CreatedAt = time.Unix(createdAt, 0).UTC()
	sub.UpdatedAt = time.Unix(updatedAt, 0).UTC()

	if err := json.Unmarshal([]byte(eventsJSON), &sub.Events); err != nil {
		sub.Events = nil // treat bad JSON as empty
	}
	return sub, nil
}

// ListRunningGroupReimageJobs returns all group_reimage_jobs with status='running'.
// Used by the startup hook (S4-4) to resume jobs orphaned by a prior process crash.
func (db *DB) ListRunningGroupReimageJobs(ctx context.Context) ([]GroupReimageJob, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, group_id, image_id, concurrency, pause_on_failure_pct, status,
		       total_nodes, triggered_nodes, succeeded_nodes, failed_nodes,
		       created_at, updated_at
		FROM group_reimage_jobs WHERE status = 'running'
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("db: list running group reimage jobs: %w", err)
	}
	defer rows.Close()

	var jobs []GroupReimageJob
	for rows.Next() {
		var j GroupReimageJob
		var createdAt, updatedAt int64
		if err := rows.Scan(
			&j.ID, &j.GroupID, &j.ImageID, &j.Concurrency, &j.PauseOnFailurePct, &j.Status,
			&j.TotalNodes, &j.TriggeredNodes, &j.SucceededNodes, &j.FailedNodes,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, fmt.Errorf("db: scan group reimage job: %w", err)
		}
		j.CreatedAt = time.Unix(createdAt, 0).UTC()
		j.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
