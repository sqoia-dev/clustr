package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// UserRole is the access level of a user account.
type UserRole string

const (
	UserRoleAdmin    UserRole = "admin"
	UserRoleOperator UserRole = "operator"
	UserRoleReadonly UserRole = "readonly"
)

// UserRecord is the persisted representation of a user account.
// password_hash is never returned to callers — see UserResponse for the
// safe display type.
type UserRecord struct {
	ID                 string
	Username           string
	PasswordHash       string // bcrypt hash; never exposed in API responses
	Role               UserRole
	MustChangePassword bool
	CreatedAt          time.Time
	LastLoginAt        *time.Time
	DisabledAt         *time.Time
}

// IsDisabled returns true when the account has been soft-deleted.
func (u UserRecord) IsDisabled() bool {
	return u.DisabledAt != nil
}

// ErrUserNotFound is returned by GetUserByUsername / GetUser when no match exists.
var ErrUserNotFound = fmt.Errorf("db: user not found")

// ErrUserDisabled is returned by GetUserByUsername when the account is disabled.
var ErrUserDisabled = fmt.Errorf("db: user account disabled")

// CountUsers returns the total number of user records regardless of disabled state.
func (db *DB) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: count users: %w", err)
	}
	return n, nil
}

// CountActiveAdmins returns the number of enabled admin accounts.
// Used by the last-admin guard before disabling or changing the role of an admin.
func (db *DB) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled_at IS NULL`,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("db: count active admins: %w", err)
	}
	return n, nil
}

// CreateUser inserts a new user record. The caller must supply a bcrypt hash
// for PasswordHash — this function never hashes passwords itself.
func (db *DB) CreateUser(ctx context.Context, rec UserRecord) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, role, must_change_password, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		rec.ID,
		strings.ToLower(strings.TrimSpace(rec.Username)),
		rec.PasswordHash,
		string(rec.Role),
		boolToInt(rec.MustChangePassword),
		rec.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: create user: %w", err)
	}
	return nil
}

// GetUserByUsername looks up a user by username (case-insensitive via COLLATE NOCASE).
// Returns ErrUserNotFound when no match exists.
// Does NOT filter disabled users — the caller checks IsDisabled() to give a
// specific error message.
func (db *DB) GetUserByUsername(ctx context.Context, username string) (UserRecord, error) {
	return db.scanUser(ctx,
		`SELECT id, username, password_hash, role, must_change_password, created_at, last_login_at, disabled_at
		 FROM users WHERE username = ?`,
		strings.ToLower(strings.TrimSpace(username)),
	)
}

// GetUser returns a single user by primary key ID.
// Returns ErrUserNotFound when no match exists.
func (db *DB) GetUser(ctx context.Context, id string) (UserRecord, error) {
	return db.scanUser(ctx,
		`SELECT id, username, password_hash, role, must_change_password, created_at, last_login_at, disabled_at
		 FROM users WHERE id = ?`,
		id,
	)
}

// ListUsers returns all user records ordered by created_at DESC.
func (db *DB) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, username, password_hash, role, must_change_password, created_at, last_login_at, disabled_at
		 FROM users ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("db: list users: %w", err)
	}
	defer rows.Close()

	var out []UserRecord
	for rows.Next() {
		u, err := db.scanUserRow(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserRole changes a user's role. Use before the last-admin guard so the
// guard can run in the caller.
func (db *DB) UpdateUserRole(ctx context.Context, id string, role UserRole) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE users SET role = ? WHERE id = ?`, string(role), id,
	)
	if err != nil {
		return fmt.Errorf("db: update user role: %w", err)
	}
	return requireOneRow(res, "users", id)
}

// DisableUser soft-deletes a user account by setting disabled_at = now.
// A non-nil disabled_at blocks login. Use the last-admin guard before calling.
func (db *DB) DisableUser(ctx context.Context, id string) error {
	res, err := db.sql.ExecContext(ctx,
		`UPDATE users SET disabled_at = ? WHERE id = ? AND disabled_at IS NULL`,
		time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("db: disable user: %w", err)
	}
	return requireOneRow(res, "users", id)
}

// SetUserPassword updates the bcrypt hash for a user and optionally clears the
// must_change_password flag. The caller supplies the already-hashed value.
func (db *DB) SetUserPassword(ctx context.Context, id, hash string, clearForceChange bool) error {
	var query string
	if clearForceChange {
		query = `UPDATE users SET password_hash = ?, must_change_password = 0 WHERE id = ?`
	} else {
		query = `UPDATE users SET password_hash = ?, must_change_password = 1 WHERE id = ?`
	}
	res, err := db.sql.ExecContext(ctx, query, hash, id)
	if err != nil {
		return fmt.Errorf("db: set user password: %w", err)
	}
	return requireOneRow(res, "users", id)
}

// SetLastLogin updates last_login_at to now. Called on successful login.
func (db *DB) SetLastLogin(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE users SET last_login_at = ? WHERE id = ?`,
		time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("db: set last login: %w", err)
	}
	return nil
}

// ─── internal helpers ──────────────────────────────────────────────────────────

// scanUser executes query with args and scans a single UserRecord.
func (db *DB) scanUser(ctx context.Context, query string, args ...any) (UserRecord, error) {
	row := db.sql.QueryRowContext(ctx, query, args...)
	u, err := db.scanUserRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UserRecord{}, ErrUserNotFound
	}
	return u, err
}

// rowScanner abstracts *sql.Row and *sql.Rows so we can share the scan logic.
type rowScanner interface {
	Scan(dest ...any) error
}

func (db *DB) scanUserRow(row rowScanner) (UserRecord, error) {
	var u UserRecord
	var role string
	var mustChange int
	var createdAt int64
	var lastLogin sql.NullInt64
	var disabledAt sql.NullInt64

	err := row.Scan(
		&u.ID,
		&u.Username,
		&u.PasswordHash,
		&role,
		&mustChange,
		&createdAt,
		&lastLogin,
		&disabledAt,
	)
	if err != nil {
		return UserRecord{}, err
	}

	u.Role = UserRole(role)
	u.MustChangePassword = mustChange != 0
	u.CreatedAt = time.Unix(createdAt, 0)
	if lastLogin.Valid {
		t := time.Unix(lastLogin.Int64, 0)
		u.LastLoginAt = &t
	}
	if disabledAt.Valid {
		t := time.Unix(disabledAt.Int64, 0)
		u.DisabledAt = &t
	}
	return u, nil
}

