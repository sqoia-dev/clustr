package handlers

// api_keys.go — admin API key management endpoints.
//
// GET    /api/v1/admin/api-keys              — list all non-revoked keys (hash prefix only)
// POST   /api/v1/admin/api-keys              — create a new key (returns raw key ONCE)
// DELETE /api/v1/admin/api-keys/{id}         — revoke (soft delete)
// POST   /api/v1/admin/api-keys/{id}/rotate  — atomically rotate: revoke old, mint new

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// APIKeysHandler handles all /admin/api-keys endpoints.
// MintKey and supporting funcs are injected from the server package to avoid circular imports.
type APIKeysHandler struct {
	DB *db.DB

	// MintKey generates a new raw key, persists its hash, and returns:
	//   rawKey  — the bearer token suffix (never stored, shown once)
	//   id      — the uuid of the new record
	//   keyHash — the sha256 hash stored in the DB (used for hash prefix display)
	MintKey func(r *http.Request, scope api.KeyScope, nodeID, label, createdBy string, expiresAt *time.Time) (rawKey, id, keyHash string, err error)

	// GetActorLabel returns the label of the key/session making this request.
	// Used to populate created_by on newly minted keys.
	GetActorLabel func(r *http.Request) string
}

// apiKeyResponse is the wire format for a key in list/get responses.
// Never includes the raw key — only the first 12 chars of the hash as a display prefix.
type apiKeyResponse struct {
	ID         string  `json:"id"`
	Scope      string  `json:"scope"`
	NodeID     string  `json:"node_id,omitempty"`
	Label      string  `json:"label,omitempty"`
	CreatedBy  string  `json:"created_by,omitempty"`
	HashPrefix string  `json:"hash_prefix"` // first 12 chars of sha256 hash
	CreatedAt  string  `json:"created_at"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
	LastUsedAt *string `json:"last_used_at,omitempty"`
}

// createKeyRequest is the JSON body for POST /admin/api-keys.
type createKeyRequest struct {
	Scope     string `json:"scope"`      // "admin" or "node"
	Label     string `json:"label"`      // human-readable label
	NodeID    string `json:"node_id"`    // required when scope=node
	ExpiresAt string `json:"expires_at"` // optional ISO8601 timestamp
}

// createKeyResponse includes the raw key (shown ONCE) plus the record.
type createKeyResponse struct {
	Key    string         `json:"key"` // full bearer token, shown once
	APIKey apiKeyResponse `json:"api_key"`
}

// HandleList handles GET /api/v1/admin/api-keys.
func (h *APIKeysHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	keys, err := h.DB.ListAPIKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to list keys", Code: "internal_error"})
		return
	}

	out := make([]apiKeyResponse, 0, len(keys))
	for _, k := range keys {
		out = append(out, toAPIKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_keys": out})
}

// HandleCreate handles POST /api/v1/admin/api-keys.
func (h *APIKeysHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "request body must be valid JSON")
		return
	}

	scope := api.KeyScope(strings.TrimSpace(req.Scope))
	if scope != api.KeyScopeAdmin && scope != api.KeyScopeNode {
		writeValidationError(w, fmt.Sprintf("scope must be %q or %q", api.KeyScopeAdmin, api.KeyScopeNode))
		return
	}
	nodeID := strings.TrimSpace(req.NodeID)
	if scope == api.KeyScopeNode && nodeID == "" {
		writeValidationError(w, "node_id is required when scope is \"node\"")
		return
	}

	label := strings.TrimSpace(req.Label)

	var expiresAt *time.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			writeValidationError(w, "expires_at must be an ISO8601 timestamp (e.g. 2026-12-31T00:00:00Z)")
			return
		}
		if t.Before(time.Now()) {
			writeValidationError(w, "expires_at must be in the future")
			return
		}
		expiresAt = &t
	}

	createdBy := h.GetActorLabel(r)

	rawKey, id, keyHash, err := h.MintKey(r, scope, nodeID, label, createdBy, expiresAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to create key", Code: "internal_error"})
		return
	}

	hashPrefix := keyHash
	if len(hashPrefix) > 12 {
		hashPrefix = hashPrefix[:12]
	}

	var bearerPrefix string
	if scope == api.KeyScopeAdmin {
		bearerPrefix = "clonr-admin-"
	} else {
		bearerPrefix = "clonr-node-"
	}

	resp := createKeyResponse{
		Key: bearerPrefix + rawKey,
		APIKey: apiKeyResponse{
			ID:         id,
			Scope:      string(scope),
			NodeID:     nodeID,
			Label:      label,
			CreatedBy:  createdBy,
			HashPrefix: hashPrefix,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	if expiresAt != nil {
		s := expiresAt.UTC().Format(time.RFC3339)
		resp.APIKey.ExpiresAt = &s
	}
	writeJSON(w, http.StatusCreated, resp)
}

// HandleRevoke handles DELETE /api/v1/admin/api-keys/{id}.
func (h *APIKeysHandler) HandleRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeValidationError(w, "key id is required")
		return
	}

	// Load the key to check its scope before blocking the revoke.
	rec, err := h.DB.GetAPIKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "api key not found", Code: "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "internal error", Code: "internal_error"})
		return
	}

	if rec.RevokedAt != nil {
		writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "api key not found or already revoked", Code: "not_found"})
		return
	}

	// Last-admin-key guard: prevent locking out all admin access.
	if rec.Scope == api.KeyScopeAdmin {
		count, err := h.DB.CountAPIKeysByScope(r.Context(), api.KeyScopeAdmin)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to check key count", Code: "internal_error"})
			return
		}
		if count <= 1 {
			writeJSON(w, http.StatusConflict, api.ErrorResponse{
				Error: "cannot revoke last admin key — create a new one first",
				Code:  "last_admin_key",
			})
			return
		}
	}

	if err := h.DB.RevokeAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "api key not found or already revoked", Code: "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to revoke key", Code: "internal_error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// HandleRotate handles POST /api/v1/admin/api-keys/{id}/rotate.
// Atomically: mint a new key (same label/scope/node_id), then revoke the old key.
// The new key is persisted first so there is no auth gap.
func (h *APIKeysHandler) HandleRotate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeValidationError(w, "key id is required")
		return
	}

	old, err := h.DB.GetAPIKey(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, api.ErrorResponse{Error: "api key not found", Code: "not_found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "internal error", Code: "internal_error"})
		return
	}

	if old.RevokedAt != nil {
		writeJSON(w, http.StatusConflict, api.ErrorResponse{Error: "api key is already revoked", Code: "already_revoked"})
		return
	}

	createdBy := h.GetActorLabel(r)
	if createdBy == "" {
		createdBy = old.Label
	}

	rawKey, newID, newHash, err := h.MintKey(r, old.Scope, old.NodeID, old.Label, createdBy, old.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to mint replacement key", Code: "internal_error"})
		return
	}

	// Revoke the old key after the new one is safely stored.
	if err := h.DB.RevokeAPIKey(r.Context(), id); err != nil && !errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, api.ErrorResponse{Error: "failed to revoke old key", Code: "internal_error"})
		return
	}

	hashPrefix := newHash
	if len(hashPrefix) > 12 {
		hashPrefix = hashPrefix[:12]
	}

	var bearerPrefix string
	if old.Scope == api.KeyScopeAdmin {
		bearerPrefix = "clonr-admin-"
	} else {
		bearerPrefix = "clonr-node-"
	}

	resp := createKeyResponse{
		Key: bearerPrefix + rawKey,
		APIKey: apiKeyResponse{
			ID:         newID,
			Scope:      string(old.Scope),
			NodeID:     old.NodeID,
			Label:      old.Label,
			CreatedBy:  createdBy,
			HashPrefix: hashPrefix,
			CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		},
	}
	if old.ExpiresAt != nil {
		s := old.ExpiresAt.UTC().Format(time.RFC3339)
		resp.APIKey.ExpiresAt = &s
	}
	writeJSON(w, http.StatusOK, resp)
}

// toAPIKeyResponse converts a db.APIKeyRecord to the wire response type.
func toAPIKeyResponse(rec db.APIKeyRecord) apiKeyResponse {
	prefix := rec.KeyHash
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	r := apiKeyResponse{
		ID:         rec.ID,
		Scope:      string(rec.Scope),
		NodeID:     rec.NodeID,
		Label:      rec.Label,
		CreatedBy:  rec.CreatedBy,
		HashPrefix: prefix,
		CreatedAt:  rec.CreatedAt.UTC().Format(time.RFC3339),
	}
	if rec.ExpiresAt != nil {
		s := rec.ExpiresAt.UTC().Format(time.RFC3339)
		r.ExpiresAt = &s
	}
	if rec.LastUsedAt != nil {
		s := rec.LastUsedAt.UTC().Format(time.RFC3339)
		r.LastUsedAt = &s
	}
	return r
}
