package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
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

// BootstrapAdminKey checks whether any admin key exists in the database.
// If none exists, it generates one, persists the hash, and prints the raw
// key to stdout ONCE. The operator must capture it immediately.
// Called during server startup before accepting traffic.
func BootstrapAdminKey(ctx context.Context, database *db.DB) error {
	count, err := database.CountAPIKeysByScope(ctx, api.KeyScopeAdmin)
	if err != nil {
		return fmt.Errorf("bootstrap admin key: %w", err)
	}
	if count > 0 {
		return nil // keys already exist, nothing to do
	}

	raw, err := generateRawKey()
	if err != nil {
		return err
	}

	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       api.KeyScopeAdmin,
		KeyHash:     sha256Hex(raw),
		Description: "bootstrap admin key (auto-generated on first start)",
		CreatedAt:   time.Now(),
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return fmt.Errorf("bootstrap admin key: persist: %w", err)
	}

	// Print to stdout (operator captures this) and log a warning.
	// Only ever printed once — there is no recovery path if lost; rotate with apikey create.
	fmt.Fprintf(os.Stdout, "\n"+
		"╔══════════════════════════════════════════════════════════════════╗\n"+
		"║              CLONR BOOTSTRAP ADMIN API KEY                      ║\n"+
		"║  Save this key — it will NOT be shown again.                    ║\n"+
		"╠══════════════════════════════════════════════════════════════════╣\n"+
		"║  clonr-admin-%s  ║\n"+
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
func CreateAPIKey(ctx context.Context, database *db.DB, scope api.KeyScope, description string) (rawKey string, id string, err error) {
	raw, err := generateRawKey()
	if err != nil {
		return "", "", err
	}

	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       scope,
		KeyHash:     sha256Hex(raw),
		Description: description,
		CreatedAt:   time.Now(),
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return "", "", fmt.Errorf("create api key: %w", err)
	}

	return raw, rec.ID, nil
}

// CreateNodeScopedKey mints a fresh node-scoped API key bound to nodeID with a
// 1-hour TTL. Any existing node-scoped keys for the same node are revoked first
// (clean rotation: exactly one live token per node at any given time).
//
// Returns the raw key (prefix: clonr-node-<raw>) for embedding in the iPXE cmdline.
// The raw key is never stored — only its SHA-256 hash is persisted.
func CreateNodeScopedKey(ctx context.Context, database *db.DB, nodeID string) (rawKey string, err error) {
	// Revoke any existing node-scoped keys for this node before minting a new one.
	if err := database.RevokeNodeScopedKeys(ctx, nodeID); err != nil {
		return "", fmt.Errorf("create node scoped key: revoke old keys: %w", err)
	}

	raw, err := generateRawKey()
	if err != nil {
		return "", err
	}

	exp := time.Now().Add(1 * time.Hour)
	rec := db.APIKeyRecord{
		ID:          uuid.New().String(),
		Scope:       api.KeyScopeNode,
		NodeID:      nodeID,
		KeyHash:     sha256Hex(raw),
		Description: "node-scoped deploy token (auto-generated at PXE serve time)",
		CreatedAt:   time.Now(),
		ExpiresAt:   &exp,
	}
	if err := database.CreateAPIKey(ctx, rec); err != nil {
		return "", fmt.Errorf("create node scoped key: persist: %w", err)
	}

	log.Info().
		Str("node_id", nodeID).
		Str("key_id", rec.ID).
		Time("expires_at", exp).
		Msg("node-scoped deploy token minted")

	return raw, nil
}
