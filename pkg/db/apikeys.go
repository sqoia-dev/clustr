package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// APIKeyRecord is the persisted representation of an API key (hash only, never the raw key).
type APIKeyRecord struct {
	ID          string
	Scope       api.KeyScope
	NodeID      string // non-empty for node-scoped keys; identifies the bound node
	KeyHash     string
	Description string
	CreatedAt   time.Time
	ExpiresAt   *time.Time // nil = no expiry
	LastUsedAt  *time.Time
}

// APIKeyLookupResult is returned by LookupAPIKey.
type APIKeyLookupResult struct {
	Scope  api.KeyScope
	NodeID string // set only for node-scoped keys
}

// CreateAPIKey inserts a new hashed API key record.
func (db *DB) CreateAPIKey(ctx context.Context, rec APIKeyRecord) error {
	var expiresAt interface{}
	if rec.ExpiresAt != nil {
		expiresAt = rec.ExpiresAt.Unix()
	}
	var nodeID interface{}
	if rec.NodeID != "" {
		nodeID = rec.NodeID
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO api_keys (id, scope, node_id, key_hash, description, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, string(rec.Scope), nodeID, rec.KeyHash, rec.Description, rec.CreatedAt.Unix(), expiresAt,
	)
	if err != nil {
		return fmt.Errorf("db: create api_key: %w", err)
	}
	return nil
}

// LookupAPIKey finds an API key by its SHA-256 hash.
// Returns ErrNoRows when not found. Returns ErrExpired when found but past its TTL.
// On success, updates last_used_at asynchronously.
func (db *DB) LookupAPIKey(ctx context.Context, keyHash string) (APIKeyLookupResult, error) {
	var scope string
	var nodeID sql.NullString
	var expiresAt sql.NullInt64

	err := db.sql.QueryRowContext(ctx,
		`SELECT scope, node_id, expires_at FROM api_keys WHERE key_hash = ?`, keyHash,
	).Scan(&scope, &nodeID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKeyLookupResult{}, sql.ErrNoRows
	}
	if err != nil {
		return APIKeyLookupResult{}, fmt.Errorf("db: lookup api_key: %w", err)
	}

	// Enforce TTL if set.
	if expiresAt.Valid && time.Now().Unix() > expiresAt.Int64 {
		return APIKeyLookupResult{}, ErrExpired
	}

	// Touch last_used_at asynchronously — don't let a write failure block the request.
	go func() {
		_, _ = db.sql.Exec(
			`UPDATE api_keys SET last_used_at = ? WHERE key_hash = ?`,
			time.Now().Unix(), keyHash,
		)
	}()

	result := APIKeyLookupResult{Scope: api.KeyScope(scope)}
	if nodeID.Valid {
		result.NodeID = nodeID.String
	}
	return result, nil
}

// RevokeNodeScopedKeys deletes all node-scoped keys bound to the given nodeID.
// Called when a new node-scoped key is minted so that only one live token exists
// per node at any time.
func (db *DB) RevokeNodeScopedKeys(ctx context.Context, nodeID string) error {
	_, err := db.sql.ExecContext(ctx,
		`DELETE FROM api_keys WHERE node_id = ? AND scope = 'node'`, nodeID,
	)
	if err != nil {
		return fmt.Errorf("db: revoke node scoped keys: %w", err)
	}
	return nil
}

// CountAPIKeysByScope returns the number of active keys for the given scope.
func (db *DB) CountAPIKeysByScope(ctx context.Context, scope api.KeyScope) (int, error) {
	var count int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM api_keys WHERE scope = ?`, string(scope),
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("db: count api_keys: %w", err)
	}
	return count, nil
}

// ListAPIKeys returns all API key records (without the hash, for display).
func (db *DB) ListAPIKeys(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, scope, node_id, key_hash, description, created_at, expires_at, last_used_at
		 FROM api_keys ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("db: list api_keys: %w", err)
	}
	defer rows.Close()

	var out []APIKeyRecord
	for rows.Next() {
		var rec APIKeyRecord
		var scope string
		var nodeID sql.NullString
		var createdAt int64
		var expiresAt sql.NullInt64
		var lastUsedAt sql.NullInt64
		if err := rows.Scan(&rec.ID, &scope, &nodeID, &rec.KeyHash, &rec.Description, &createdAt, &expiresAt, &lastUsedAt); err != nil {
			return nil, fmt.Errorf("db: scan api_key: %w", err)
		}
		rec.Scope = api.KeyScope(scope)
		rec.CreatedAt = time.Unix(createdAt, 0)
		if nodeID.Valid {
			rec.NodeID = nodeID.String
		}
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			rec.ExpiresAt = &t
		}
		if lastUsedAt.Valid {
			t := time.Unix(lastUsedAt.Int64, 0)
			rec.LastUsedAt = &t
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteAPIKey removes a key by ID.
func (db *DB) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("db: delete api_key: %w", err)
	}
	return nil
}
