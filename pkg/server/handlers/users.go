package handlers

// users.go — admin user management endpoints (ADR-0007).
//
// GET    /api/v1/admin/users              — list all users
// POST   /api/v1/admin/users              — create user
// PUT    /api/v1/admin/users/{id}         — update role / disable
// POST   /api/v1/admin/users/{id}/reset-password — admin sets temp password
// DELETE /api/v1/admin/users/{id}         — soft delete (sets disabled_at)
//
// All routes require admin role. Last-admin guard blocks disable/delete of
// the sole remaining enabled admin.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"github.com/sqoia-dev/clonr/pkg/db"
)

// UsersHandler handles the admin user CRUD endpoints.
type UsersHandler struct {
	// DB is the database handle.
	DB *db.DB

	// HashPassword returns the bcrypt hash of the given plaintext password.
	// Lives here as a function field to avoid importing golang.org/x/crypto
	// directly in the handlers package (kept in server package instead).
	HashPassword func(plaintext string) (string, error)
}

// userResponse is the safe wire type for a user — never includes password_hash.
type userResponse struct {
	ID                 string  `json:"id"`
	Username           string  `json:"username"`
	Role               string  `json:"role"`
	MustChangePassword bool    `json:"must_change_password"`
	Disabled           bool    `json:"disabled"`
	CreatedAt          string  `json:"created_at"`
	LastLoginAt        *string `json:"last_login_at,omitempty"`
}

func toUserResponse(u db.UserRecord) userResponse {
	r := userResponse{
		ID:                 u.ID,
		Username:           u.Username,
		Role:               string(u.Role),
		MustChangePassword: u.MustChangePassword,
		Disabled:           u.IsDisabled(),
		CreatedAt:          u.CreatedAt.UTC().Format(time.RFC3339),
	}
	if u.LastLoginAt != nil {
		s := u.LastLoginAt.UTC().Format(time.RFC3339)
		r.LastLoginAt = &s
	}
	return r
}

// HandleList handles GET /api/v1/admin/users.
func (h *UsersHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	users, err := h.DB.ListUsers(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("users: list failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error", "code": "internal_error",
		})
		return
	}
	out := make([]userResponse, len(users))
	for i, u := range users {
		out[i] = toUserResponse(u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// createUserRequest is the body for POST /api/v1/admin/users.
type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// HandleCreate handles POST /api/v1/admin/users.
func (h *UsersHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "request body must be valid JSON")
		return
	}

	if req.Username == "" || req.Password == "" || req.Role == "" {
		writeValidationError(w, "username, password, and role are required")
		return
	}
	if !validRole(req.Role) {
		writeValidationError(w, "role must be one of: admin, operator, readonly")
		return
	}
	if len(req.Password) < 8 {
		writeValidationError(w, "Password must be at least 8 characters")
		return
	}

	hash, err := h.HashPassword(req.Password)
	if err != nil {
		log.Error().Err(err).Msg("users: hash password failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error", "code": "internal_error",
		})
		return
	}

	id := uuid.New().String()
	rec := db.UserRecord{
		ID:           id,
		Username:     req.Username,
		PasswordHash: hash,
		Role:         db.UserRole(req.Role),
		CreatedAt:    time.Now(),
	}
	if err := h.DB.CreateUser(r.Context(), rec); err != nil {
		// SQLite UNIQUE constraint violation — username already taken.
		if isSQLiteUniqueErr(err) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "username already exists",
				"code":  "conflict",
			})
			return
		}
		log.Error().Err(err).Msg("users: create failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error", "code": "internal_error",
		})
		return
	}

	created, err := h.DB.GetUser(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	writeJSON(w, http.StatusCreated, toUserResponse(created))
}

// updateUserRequest is the body for PUT /api/v1/admin/users/{id}.
type updateUserRequest struct {
	Role     string `json:"role,omitempty"`
	Disabled *bool  `json:"disabled,omitempty"`
}

// HandleUpdate handles PUT /api/v1/admin/users/{id}.
func (h *UsersHandler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeValidationError(w, "user id is required")
		return
	}

	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeValidationError(w, "request body must be valid JSON")
		return
	}

	user, err := h.DB.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrUserNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "user not found", "code": "not_found",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error", "code": "internal_error",
		})
		return
	}

	// Role change.
	if req.Role != "" && req.Role != string(user.Role) {
		if !validRole(req.Role) {
			writeValidationError(w, "role must be one of: admin, operator, readonly")
			return
		}
		// Last-admin guard: if demoting an admin, ensure another admin exists.
		if user.Role == db.UserRoleAdmin && req.Role != "admin" {
			if err := h.enforceLastAdminGuard(r, w); err != nil {
				return
			}
		}
		if err := h.DB.UpdateUserRole(r.Context(), id, db.UserRole(req.Role)); err != nil {
			if errors.Is(err, db.ErrUserNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
				return
			}
			log.Error().Err(err).Msg("users: update role failed")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
			return
		}
	}

	// Disable.
	if req.Disabled != nil && *req.Disabled && !user.IsDisabled() {
		if user.Role == db.UserRoleAdmin {
			if err := h.enforceLastAdminGuard(r, w); err != nil {
				return
			}
		}
		if err := h.DB.DisableUser(r.Context(), id); err != nil {
			if errors.Is(err, db.ErrUserNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
				return
			}
			log.Error().Err(err).Msg("users: disable failed")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
			return
		}
	}

	updated, err := h.DB.GetUser(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"id": id})
		return
	}
	writeJSON(w, http.StatusOK, toUserResponse(updated))
}

// resetPasswordRequest is the body for POST /api/v1/admin/users/{id}/reset-password.
type resetPasswordRequest struct {
	Password string `json:"password"`
}

// HandleResetPassword handles POST /api/v1/admin/users/{id}/reset-password.
func (h *UsersHandler) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeValidationError(w, "user id is required")
		return
	}

	var req resetPasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeValidationError(w, "password is required")
		return
	}
	if len(req.Password) < 8 {
		writeValidationError(w, "Password must be at least 8 characters")
		return
	}

	hash, err := h.HashPassword(req.Password)
	if err != nil {
		log.Error().Err(err).Msg("users: hash password failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
		return
	}

	// clearForceChange=false: admin reset always forces the user to change again.
	if err := h.DB.SetUserPassword(r.Context(), id, hash, false); err != nil {
		if errors.Is(err, db.ErrUserNotFound) || errors.Is(err, sql.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
			return
		}
		log.Error().Err(err).Msg("users: reset password failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// HandleDelete handles DELETE /api/v1/admin/users/{id} — soft delete.
func (h *UsersHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		writeValidationError(w, "user id is required")
		return
	}

	user, err := h.DB.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrUserNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
		return
	}

	if user.Role == db.UserRoleAdmin {
		if err := h.enforceLastAdminGuard(r, w); err != nil {
			return
		}
	}

	if err := h.DB.DisableUser(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found", "code": "not_found"})
			return
		}
		log.Error().Err(err).Msg("users: delete failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// enforceLastAdminGuard checks that at least 2 active admins exist before
// allowing a role-change or disable on an admin account.
// Writes a 409 and returns a non-nil error if the guard fires.
func (h *UsersHandler) enforceLastAdminGuard(r *http.Request, w http.ResponseWriter) error {
	count, err := h.DB.CountActiveAdmins(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("users: count admins failed")
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "code": "internal_error"})
		return err
	}
	if count <= 1 {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "cannot disable or demote the last admin user",
			"code":  "last_admin_user",
		})
		return errors.New("last admin")
	}
	return nil
}

// validRole returns true for the three allowed role strings.
func validRole(role string) bool {
	switch role {
	case "admin", "operator", "readonly":
		return true
	}
	return false
}

// isSQLiteUniqueErr returns true when err is a SQLite UNIQUE constraint violation.
func isSQLiteUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
