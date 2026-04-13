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

	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// ctxKeyScope is the context key used to store the resolved API key scope.
type ctxKeyScope struct{}

// scopeFromContext returns the KeyScope stored in the request context, or "".
func scopeFromContext(ctx context.Context) api.KeyScope {
	v, _ := ctx.Value(ctxKeyScope{}).(api.KeyScope)
	return v
}

// apiKeyAuth returns a middleware that resolves the API key scope from the
// Bearer token, stores it in the context, and continues.
//
// This middleware does NOT reject unauthenticated requests — it is a resolver.
// Use requireScope to enforce a minimum scope on specific route groups.
//
// Dev-mode escape hatch: if CLONR_AUTH_DEV_MODE=1 is explicitly set,
// all requests are treated as admin scope. Never the default.
func apiKeyAuth(database *db.DB, devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if devMode {
				ctx := context.WithValue(r.Context(), ctxKeyScope{}, api.KeyScopeAdmin)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			raw := extractBearerToken(r)
			if raw == "" {
				// No key provided — pass through with empty scope.
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
			scope, err := database.LookupAPIKey(r.Context(), hash)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeUnauthorized(w, "invalid API key")
					return
				}
				log.Error().Err(err).Msg("api key auth: db lookup failed")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: "internal server error", Code: "internal_error"})
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyScope{}, scope)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireScope returns a middleware that enforces a minimum scope on the route.
// It must be placed after apiKeyAuth in the middleware chain (which populates the context).
// adminOnly=true → only admin keys pass; adminOnly=false → both admin and node keys pass.
func requireScope(adminOnly bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scope := scopeFromContext(r.Context())
			if adminOnly && scope != api.KeyScopeAdmin {
				writeForbidden(w, "this route requires an admin-scope API key")
				return
			}
			if scope != api.KeyScopeAdmin && scope != api.KeyScopeNode {
				writeUnauthorized(w, "unrecognized scope")
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
			// Only enforce on non-GET, non-HEAD requests that actually send a body.
			accept := r.Header.Get("Accept")
			if accept != "" &&
				accept != "*/*" &&
				!strings.Contains(accept, "application/json") &&
				!strings.Contains(accept, "application/vnd.clonr.v1+json") &&
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
