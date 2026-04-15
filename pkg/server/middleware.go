package server

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// ctxKeyScope is the context key used to store the resolved API key scope.
type ctxKeyScope struct{}

// ctxKeyNodeID is the context key used to store the node ID bound to a node-scoped key.
type ctxKeyNodeID struct{}

// ctxKeyKeyID is the context key for the api_keys.id of the authenticated key.
// Set for Bearer-token auth; empty for session-cookie auth unless the session
// carries a key ID (future work).
type ctxKeyKeyID struct{}

// ctxKeyKeyLabel is the context key for the human-readable label of the authenticated key.
// Used for audit attribution (created_by on newly minted keys).
type ctxKeyKeyLabel struct{}

// ctxKeyUserID is the context key for the users.id of the session-authenticated user.
type ctxKeyUserID struct{}

// ctxKeyUserRole is the context key for the role of the session-authenticated user.
type ctxKeyUserRole struct{}

// scopeFromContext returns the KeyScope stored in the request context, or "".
func scopeFromContext(ctx context.Context) api.KeyScope {
	v, _ := ctx.Value(ctxKeyScope{}).(api.KeyScope)
	return v
}

// nodeIDFromContext returns the node ID stored in the request context, or "".
// Only set for requests authenticated with a node-scoped key.
func nodeIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyNodeID{}).(string)
	return v
}

// keyIDFromContext returns the api_keys.id of the authenticated key, or "".
// Empty for session-cookie auth.
func keyIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyKeyID{}).(string)
	return v
}

// keyLabelFromContext returns the label of the authenticated key, or "".
func keyLabelFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyKeyLabel{}).(string)
	return v
}

// userIDFromContext returns the users.id of the session-authenticated user, or "".
func userIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUserID{}).(string)
	return v
}

// userRoleFromContext returns the role of the session-authenticated user, or "".
func userRoleFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyUserRole{}).(string)
	return v
}

// actorLabel returns a human-readable actor for audit log attribution.
// Returns "user:<userID>" for session auth, "key:<label>" for Bearer auth.
func actorLabel(ctx context.Context) string {
	if uid := userIDFromContext(ctx); uid != "" {
		return "user:" + uid
	}
	if label := keyLabelFromContext(ctx); label != "" {
		return "key:" + label
	}
	return "unknown"
}

// apiKeyAuth returns a middleware that resolves the auth scope from either:
//  1. The session cookie (clonr_session) validated via HMAC — cookie takes precedence.
//  2. The Authorization: Bearer token — SHA-256 hash lookup against api_keys table.
//
// This middleware does NOT reject unauthenticated requests — it is a resolver.
// Use requireScope to enforce a minimum scope on specific route groups.
//
// When a session cookie is valid and needs sliding, the middleware re-signs and
// re-issues the cookie transparently before passing to the next handler.
//
// Dev-mode escape hatch: if CLONR_AUTH_DEV_MODE=1 is explicitly set,
// all requests are treated as admin scope. Never the default.
func apiKeyAuth(database *db.DB, devMode bool, sessionSecret []byte, sessionSecure bool) func(http.Handler) http.Handler {
	const cookieName = "clonr_session"
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if devMode {
				ctx := context.WithValue(r.Context(), ctxKeyScope{}, api.KeyScopeAdmin)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// --- Source 1: session cookie ---
			if len(sessionSecret) > 0 {
				if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
					result, verr := validateSessionToken(sessionSecret, c.Value)
					if verr == nil {
						// Valid session — slide if needed.
						if result.needsReissue {
							slid := slideSessionPayload(result.payload)
							if newToken, serr := signSessionToken(sessionSecret, slid); serr == nil {
								cookieExp := time.Unix(slid.EXP, 0)
								http.SetCookie(w, &http.Cookie{
									Name:     cookieName,
									Value:    newToken,
									Path:     "/",
									HttpOnly: true,
									Secure:   sessionSecure,
									SameSite: http.SameSiteStrictMode,
									MaxAge:   int(time.Until(cookieExp).Seconds()),
								})
							}
						}
						// Map the user role to a scope for existing requireScope middleware.
						// admin → admin scope; operator/readonly → admin scope (read path)
						// Fine-grained gating uses requireRole middleware.
						roleScope := api.KeyScopeAdmin
						if result.payload.Role == "readonly" {
							// readonly maps to a sentinel string that requireScope(adminOnly=true) will block.
							// We keep the real role in ctxKeyUserRole for requireRole checks.
							roleScope = api.KeyScope("readonly")
						}
						ctx := context.WithValue(r.Context(), ctxKeyScope{}, roleScope)
						ctx = context.WithValue(ctx, ctxKeyUserID{}, result.payload.Sub)
						ctx = context.WithValue(ctx, ctxKeyUserRole{}, result.payload.Role)
						next.ServeHTTP(w, r.WithContext(ctx))
						return
					}
					// Cookie present but invalid/expired — clear it and fall through.
					http.SetCookie(w, &http.Cookie{
						Name:     cookieName,
						Value:    "",
						Path:     "/",
						HttpOnly: true,
						Secure:   sessionSecure,
						SameSite: http.SameSiteStrictMode,
						MaxAge:   -1,
					})
				}
			}

			// --- Source 2: Bearer token ---
			raw := extractBearerToken(r)
			if raw == "" {
				// No auth provided — pass through with empty scope.
				// requireScope will reject if the route needs auth.
				next.ServeHTTP(w, r)
				return
			}

			// Strip the typed prefix (clonr-admin- / clonr-node-) before hashing.
			// The DB stores sha256(<raw-hex>) where raw-hex is the bare entropy;
			// the full Bearer token is clonr-<scope>-<raw-hex>, so we strip the
			// well-known prefixes before computing the lookup hash.
			hashInput := raw
			for _, pfx := range []string{"clonr-admin-", "clonr-node-"} {
				if strings.HasPrefix(raw, pfx) {
					hashInput = strings.TrimPrefix(raw, pfx)
					break
				}
			}
			hash := sha256Hex(hashInput)
			lookupResult, err := database.LookupAPIKey(r.Context(), hash)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeUnauthorized(w, "invalid API key")
					return
				}
				if errors.Is(err, db.ErrExpired) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "api key expired", Code: "key_expired"})
					return
				}
				if errors.Is(err, db.ErrRevoked) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "api key revoked", Code: "key_revoked"})
					return
				}
				log.Error().Err(err).Msg("api key auth: db lookup failed")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "internal server error", Code: "internal_error"})
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyScope{}, lookupResult.Scope)
			if lookupResult.NodeID != "" {
				ctx = context.WithValue(ctx, ctxKeyNodeID{}, lookupResult.NodeID)
			}
			if lookupResult.ID != "" {
				ctx = context.WithValue(ctx, ctxKeyKeyID{}, lookupResult.ID)
			}
			if lookupResult.Label != "" {
				ctx = context.WithValue(ctx, ctxKeyKeyLabel{}, lookupResult.Label)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireScope returns a middleware that enforces a minimum scope on the route.
// It must be placed after apiKeyAuth in the middleware chain (which populates the context).
// adminOnly=true → only admin keys pass; adminOnly=false → both admin and node keys pass.
//
// Unauthenticated requests (empty scope) always get 401.
// Authenticated requests with insufficient scope get 403.
func requireScope(adminOnly bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := scopeFromContext(r.Context())
			// No credentials at all → 401 Unauthorized.
			if scope == "" {
				writeUnauthorized(w, "authentication required")
				return
			}
			// Valid scope but insufficient level → 403 Forbidden.
			if adminOnly && scope != api.KeyScopeAdmin {
				writeForbidden(w, "this route requires an admin-scope API key or admin/operator user")
				return
			}
			if scope != api.KeyScopeAdmin && scope != api.KeyScopeNode && scope != api.KeyScope("readonly") {
				writeForbidden(w, "unrecognized scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireRole returns a middleware that enforces a minimum user role.
// Role hierarchy: admin > operator > readonly.
// API-key Bearer auth (non-session) is treated as admin for backward compat
// unless the key carries an explicit scope that doesn't qualify.
//
// minimum: "admin" | "operator" | "readonly"
func requireRole(minimum string) func(http.Handler) http.Handler {
	roleRank := map[string]int{
		"readonly": 1,
		"operator": 2,
		"admin":    3,
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := scopeFromContext(r.Context())
			if scope == "" {
				writeUnauthorized(w, "authentication required")
				return
			}

			// Session cookie path: if a user role is set, enforce it strictly.
			// The scope for operators is mapped to KeyScopeAdmin for requireScope compat,
			// but requireRole must use the real role, not the mapped scope.
			if role := userRoleFromContext(r.Context()); role != "" {
				if roleRank[role] < roleRank[minimum] {
					writeForbidden(w, "insufficient role: requires "+minimum+" or higher")
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			// Bearer token path: admin-scoped API keys always pass.
			if scope == api.KeyScopeAdmin {
				next.ServeHTTP(w, r)
				return
			}

			writeForbidden(w, "insufficient permissions")
		})
	}
}

// requireNodeOwnership returns a middleware that ensures the authenticated node key
// matches the {id} URL parameter. Admin keys always pass. Node keys are only allowed
// if their bound node_id matches the URL {id} parameter.
//
// Use this on deploy-complete, deploy-failed, and other node-self-report routes.
func requireNodeOwnership(nodeIDParam string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := scopeFromContext(r.Context())
			if scope == "" {
				writeUnauthorized(w, "authentication required")
				return
			}
			if scope == api.KeyScopeAdmin {
				next.ServeHTTP(w, r)
				return
			}
			if scope != api.KeyScopeNode {
				writeForbidden(w, "unrecognized scope")
				return
			}
			// Node scope: the key's bound node_id must match the URL parameter.
			tokenNodeID := nodeIDFromContext(r.Context())
			urlNodeID := chi.URLParam(r, nodeIDParam)
			if tokenNodeID == "" || tokenNodeID != urlNodeID {
				writeForbidden(w, "node key is not authorized for this node")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireImageAccess returns a middleware that allows either:
//   - Admin-scoped keys: always allowed.
//   - Node-scoped keys: allowed only if the node's currently-assigned base_image_id
//     matches the imageID URL parameter. This prevents a node from fetching images
//     other than the one it is supposed to deploy.
//
// Use this on GET /images/{id} and GET /images/{id}/blob.
func requireImageAccess(imageIDParam string, database *db.DB) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := scopeFromContext(r.Context())
			if scope == "" {
				writeUnauthorized(w, "authentication required")
				return
			}
			if scope == api.KeyScopeAdmin {
				next.ServeHTTP(w, r)
				return
			}
			if scope != api.KeyScopeNode {
				writeForbidden(w, "unrecognized scope")
				return
			}
			// Node scope: look up the node's assigned image and compare.
			tokenNodeID := nodeIDFromContext(r.Context())
			if tokenNodeID == "" {
				writeForbidden(w, "node key has no bound node ID")
				return
			}
			nodeCfg, err := database.GetNodeConfig(r.Context(), tokenNodeID)
			if err != nil {
				log.Error().Err(err).Str("node_id", tokenNodeID).Msg("requireImageAccess: lookup node")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "internal server error", Code: "internal_error"})
				return
			}
			urlImageID := chi.URLParam(r, imageIDParam)
			if nodeCfg.BaseImageID == "" || nodeCfg.BaseImageID != urlImageID {
				writeForbidden(w, "node key is not authorized to access this image")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// extractBearerToken pulls the raw token from Authorization: Bearer <token>.
// Falls back to ?token= query param for WebSocket compatibility.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return r.URL.Query().Get("token")
}

// sha256Hex returns the lowercase hex-encoded SHA-256 of s.
func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// writeUnauthorized writes a 401 JSON response.
func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: msg, Code: "unauthorized"})
}

// writeForbidden writes a 403 JSON response.
func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: msg, Code: "forbidden"})
}

// apiVersionHeader returns a middleware that sets API-Version: v1 on all responses
// under /api/v1/* and enforces Accept header tolerance (accepts both
// application/vnd.clonr.v1+json and the standard application/json).
func apiVersionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1") {
			w.Header().Set("API-Version", "v1")

			// Tolerate both the versioned vendor MIME type and plain application/json.
			// Also tolerate text/event-stream (SSE endpoints) and text/plain.
			// Only reject requests that explicitly advertise an Accept that we
			// cannot satisfy with a JSON or event-stream response.
			accept := r.Header.Get("Accept")
			if accept != "" &&
				accept != "*/*" &&
				!strings.Contains(accept, "application/json") &&
				!strings.Contains(accept, "application/vnd.clonr.v1+json") &&
				!strings.Contains(accept, "text/event-stream") &&
				!strings.Contains(accept, "text/plain") &&
				!strings.Contains(accept, "*/*") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotAcceptable)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{
					Error: "Accept header must include application/json or application/vnd.clonr.v1+json",
					Code:  "not_acceptable",
				})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requestLogger logs each request with method, path, status, and duration.
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.status).
			Dur("duration", time.Since(start)).
			Msg("request")
	})
}

// panicRecovery converts panics into 500 responses and logs them.
func panicRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error().Interface("panic", rec).Str("path", r.URL.Path).Msg("panic recovered")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{
					Error: "internal server error",
					Code:  "internal_error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the underlying ResponseWriter if it supports http.Flusher.
// Required for SSE endpoints — without this, http.Flusher type assertion fails.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter if it supports http.Hijacker.
// Required for WebSocket upgrades.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
