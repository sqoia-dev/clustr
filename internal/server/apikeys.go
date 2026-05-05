package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
	"github.com/sqoia-dev/clustr/pkg/api"
	"github.com/sqoia-dev/clustr/internal/db"
)

// generateRawKey generates a cryptographically secure 32-byte random key
// and returns its hex encoding (64 chars). This is the value the operator
// stores; only the SHA-256 hash is persisted.
func generateRawKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate key: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// BootstrapDefaultUser creates the default clustr/clustr admin account on first run
// (ADR-0007). Only runs when the users table is completely empty.
// Logs a SECURITY warning to stderr — operator must change the password on first login.
func BootstrapDefaultUser(ctx context.Context, database *db.DB) error {
	count, err := database.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("bootstrap default user: count: %w", err)
	}
	if count > 0 {
		return nil // users already exist; do not re-create
	}

	// Hash "clustr" with bcrypt cost 12.
	hash, err := bcrypt.GenerateFromPassword([]byte("clustr"), 12)
	if err != nil {
		return fmt.Errorf("bootstrap default user: bcrypt: %w", err)
	}

	rec := db.UserRecord{
		ID:                 uuid.New().String(),
		Username:           "clustr",
		PasswordHash:       string(hash),
		Role:               db.UserRoleAdmin,
		MustChangePassword: false,
		CreatedAt:          time.Now(),
	}
	if err := database.CreateUser(ctx, rec); err != nil {
		return fmt.Errorf("bootstrap default user: insert: %w", err)
	}

	log.Warn().
		Str("username", "clustr").
		Str("role", "admin").
		Msg("SECURITY: default credentials clustr/clustr are active — change the password via Settings when ready")

	return nil
}

// WarnIfDefaultAdminMissing logs a WARN line if the default "clustr" admin account
// does not exist. This surfaces the absence loudly in logs so operators notice when
// bootstrap-admin --username X was run without creating the default account, or when
// it was explicitly deleted. We do NOT auto-recreate — that would hide operator intent.
// The web UI gets the same signal via the default_admin_present field in /api/v1/auth/status.
func WarnIfDefaultAdminMissing(ctx context.Context, database *db.DB) {
	_, err := database.GetUserByUsername(ctx, "clustr")
	if err != nil {
		log.Warn().
			Str("username", "clustr").
			Msg("startup: default admin 'clustr' is absent — run 'clustr-serverd bootstrap-admin' to re-create it, or log in with your custom admin account")
	}
}

// BootstrapAdminKey checks whether any admin key exists in the database.
// If none exists, it generates one, persists the hash, and prints the raw
// key to stdout ONCE. The operator must capture it immediately.
// Called during server startup before accepting traffic.
//
// The minted key's user_id is bound to the bootstrap admin user (looked up by
// resolveBootstrapAdminID below).  Migration 103 made user_id NOT NULL; this
// preserves that invariant for the auto-generated bootstrap key.
func BootstrapAdminKey(ctx context.Context, database *db.DB) error {
	count, err := database.CountAPIKeysByScope(ctx, api.KeyScopeAdmin)
	if err != nil {
		return fmt.Errorf("bootstrap admin key: %w", err)
	}
	if count > 0 {
		return nil // keys already exist, nothing to do
	}

	userID, err := resolveBootstrapAdminID(ctx, database)
	if err != nil {
		return fmt.Errorf("bootstrap admin key: %w", err)
	}

	raw, err := generateRawKey()
	if err != nil {
		return err
	}

	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       api.KeyScopeAdmin,
		KeyHash:     sha256Hex(raw),
		Label:       "bootstrap",
		Description: "bootstrap admin key (auto-generated on first start)",
		UserID:      userID,
		CreatedAt:   time.Now(),
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return fmt.Errorf("bootstrap admin key: persist: %w", err)
	}

	// Print to stdout (operator captures this) and log a warning.
	// Only ever printed once — there is no recovery path if lost; rotate with apikey create.
	fmt.Fprintf(os.Stdout, "\n"+
		"╔══════════════════════════════════════════════════════════════════╗\n"+
		"║              CLUSTR BOOTSTRAP ADMIN API KEY                      ║\n"+
		"║  Save this key — it will NOT be shown again.                    ║\n"+
		"╠══════════════════════════════════════════════════════════════════╣\n"+
		"║  clustr-admin-%s  ║\n"+
		"╚══════════════════════════════════════════════════════════════════╝\n\n",
		raw,
	)
	log.Warn().
		Str("key_id", rec.ID).
		Str("scope", string(rec.Scope)).
		Msg("bootstrap: generated initial admin API key — capture it from stdout now")

	return nil
}

// CreateAPIKey generates a new key for the given scope, persists the hash,
// and returns the raw key to the caller (CLI prints it once).
//
// CLI callers (clustr-serverd apikey create) get user_id resolved to the
// bootstrap admin since there's no session context.  Web/programmatic callers
// should use CreateAPIKeyFull and pass the operator's user_id explicitly.
func CreateAPIKey(ctx context.Context, database *db.DB, scope api.KeyScope, description string) (rawKey string, id string, err error) {
	userID, err := resolveBootstrapAdminID(ctx, database)
	if err != nil {
		return "", "", fmt.Errorf("create api key: %w", err)
	}

	raw, err := generateRawKey()
	if err != nil {
		return "", "", err
	}

	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       scope,
		KeyHash:     sha256Hex(raw),
		Description: description,
		UserID:      userID,
		CreatedAt:   time.Now(),
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return "", "", fmt.Errorf("create api key: %w", err)
	}

	return raw, rec.ID, nil
}

// CreateAPIKeyFull generates a new key with label, created_by, and optional expiry.
// Returns the raw key (never stored), the record ID, and the full record for the response.
//
// userID must reference an extant users(id) (NOT NULL since migration 103).
// Empty userID is rejected by the underlying DB layer.
func CreateAPIKeyFull(ctx context.Context, database *db.DB, scope api.KeyScope, nodeID, label, createdBy, userID string, expiresAt *time.Time) (rawKey string, rec db.APIKeyRecord, err error) {
	raw, err := generateRawKey()
	if err != nil {
		return "", db.APIKeyRecord{}, err
	}

	rec = db.APIKeyRecord{
		ID:        uuid.New().String(),
		Scope:     scope,
		NodeID:    nodeID,
		KeyHash:   sha256Hex(raw),
		Label:     label,
		CreatedBy: createdBy,
		UserID:    userID,
		CreatedAt: time.Now(),
		ExpiresAt: expiresAt,
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return "", db.APIKeyRecord{}, fmt.Errorf("create api key: %w", err)
	}

	return raw, rec, nil
}

// CreateNodeScopedKey mints a fresh node-scoped API key bound to nodeID with a
// 24h TTL. Any existing node-scoped keys for the same node are revoked atomically
// in the same database transaction as the insert, eliminating the window between
// revoke and create where the node would temporarily have no valid key.
//
// Returns the raw key (prefix: clustr-node-<raw>) for embedding in the iPXE cmdline.
// The raw key is never stored — only its SHA-256 hash is persisted.
//
// TTL: 24h per founder directive (#250). The token sweeper goroutine reaps any
// rows whose expires_at has passed, so the api_keys table doesn't accumulate
// dead node-scope tokens. Admin-scope keys keep their long-lived NULL TTL until
// rotation UX exists.
//
// user_id ownership: every row in api_keys references users(id) NOT NULL since
// migration 103. The boot handler hits this code path with no session context;
// we resolve to the bootstrap admin user and bind the token to that operator.
// Once the boot handler grows session attribution (e.g. via the operator who
// stamped reimage_pending) we can thread the actual operator's id through.
func CreateNodeScopedKey(ctx context.Context, database *db.DB, nodeID string) (rawKey string, err error) {
	userID, err := resolveBootstrapAdminID(ctx, database)
	if err != nil {
		return "", fmt.Errorf("create node scoped key: %w", err)
	}

	raw, err := generateRawKey()
	if err != nil {
		return "", err
	}

	exp := time.Now().Add(24 * time.Hour)
	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       api.KeyScopeNode,
		NodeID:      nodeID,
		KeyHash:     sha256Hex(raw),
		Label:       "node-deploy-token",
		Description: "node-scoped deploy token (auto-generated at PXE serve time)",
		UserID:      userID,
		CreatedAt:   time.Now(),
		ExpiresAt:   &exp,
	}

	// Revoke old keys and insert the new one atomically — no window where the
	// node is left without a valid key between the two operations.
	if err := database.RevokeAndCreateNodeScopedKey(ctx, nodeID, rec); err != nil {
		return "", fmt.Errorf("create node scoped key: %w", err)
	}

	log.Info().
		Str("node_id", nodeID).
		Str("key_id", rec.ID).
		Str("user_id", userID).
		Time("expires_at", exp).
		Msg("node-scoped deploy token minted")

	return raw, nil
}

// resolveBootstrapAdminID returns the users(id) that owns server-minted system
// tokens (boot handler, CLI apikey create, BootstrapAdminKey).  Resolution
// preference (matches migration 103's _bootstrap_admin temp-table heuristic):
//
//  1. username='clustr' AND role='admin' AND not disabled
//  2. any role='admin' AND not disabled
//  3. any not-disabled user
//  4. any user (including disabled, as last resort)
//
// If the users table is completely empty (e.g. the CLI was invoked against a
// fresh DB before the web bootstrap path ever ran), creates the default
// clustr/clustr admin via BootstrapDefaultUser and returns its id.  This
// matches the operator-visible behaviour: `clustr-serverd apikey create
// --scope admin` should "just work" on a brand-new install without forcing a
// separate `bootstrap-admin` invocation first.
func resolveBootstrapAdminID(ctx context.Context, database *db.DB) (string, error) {
	id, err := lookupBootstrapAdminID(ctx, database)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("resolve bootstrap admin id: %w", err)
	}
	// users table is empty — auto-bootstrap the clustr/clustr default and retry.
	if berr := BootstrapDefaultUser(ctx, database); berr != nil {
		return "", fmt.Errorf("resolve bootstrap admin id: auto-bootstrap: %w", berr)
	}
	id, err = lookupBootstrapAdminID(ctx, database)
	if err != nil {
		return "", fmt.Errorf("resolve bootstrap admin id: post-bootstrap: %w", err)
	}
	return id, nil
}

// lookupBootstrapAdminID is the read-only half of resolveBootstrapAdminID.
// Returns sql.ErrNoRows when the users table has no rows.
func lookupBootstrapAdminID(ctx context.Context, database *db.DB) (string, error) {
	var id string
	err := database.SQL().QueryRowContext(ctx, `
		SELECT id FROM users
		ORDER BY
			CASE WHEN LOWER(username) = 'clustr' AND role = 'admin' AND disabled_at IS NULL THEN 0
			     WHEN role = 'admin' AND disabled_at IS NULL THEN 1
			     WHEN disabled_at IS NULL THEN 2
			     ELSE 3 END,
			created_at ASC
		LIMIT 1
	`).Scan(&id)
	return id, err
}
