package db_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/pkg/api"
)

// TestSweepExpiredAPIKeys validates the #250 Q2 token sweeper:
//
//   - rows whose expires_at is strictly less than `now` are deleted
//   - rows whose expires_at is in the future are preserved
//   - rows with NULL expires_at (long-lived admin keys) are preserved
//
// The sweep is destructive (DELETE, not UPDATE revoked_at) — past-expiry rows
// are useless to authentication and accumulating them costs index space on
// every login lookup.
func TestSweepExpiredAPIKeys(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "sweep.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	// Seed a user so api_keys.user_id NOT NULL is satisfiable.
	user := db.UserRecord{
		ID:           uuid.New().String(),
		Username:     "sweepowner",
		PasswordHash: "$2a$12$Knr1q.K6rh1bb8YtNKp0CexCN7NVA8ZswlmbmqzQjuoKHi3AEK7Mq",
		Role:         db.UserRoleAdmin,
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if err := d.CreateUser(ctx, user); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// Three keys: past-expiry, future-expiry, never-expire.
	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(24 * time.Hour)
	mk := func(label string, exp *time.Time) string {
		id := uuid.New().String()
		rec := db.APIKeyRecord{
			ID:        id,
			Scope:     api.KeyScopeAdmin,
			KeyHash:   id, // unique per row; sweeper doesn't care about hash content
			UserID:    user.ID,
			Label:     label,
			CreatedAt: time.Now(),
			ExpiresAt: exp,
		}
		if err := d.CreateAPIKey(ctx, rec); err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		return id
	}
	pastID := mk("past", &past)
	futureID := mk("future", &future)
	neverID := mk("never", nil)

	n, err := d.SweepExpiredAPIKeys(ctx, time.Now())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row swept, got %d", n)
	}

	// past key gone; others remain.
	if _, err := d.GetAPIKey(ctx, pastID); err == nil {
		t.Error("past-expiry key was not deleted")
	} else if err != sql.ErrNoRows {
		t.Errorf("past-expiry key lookup: expected ErrNoRows, got %v", err)
	}
	if _, err := d.GetAPIKey(ctx, futureID); err != nil {
		t.Errorf("future-expiry key was deleted: %v", err)
	}
	if _, err := d.GetAPIKey(ctx, neverID); err != nil {
		t.Errorf("never-expire key was deleted: %v", err)
	}

	// Idempotent: a second sweep at the same time deletes 0 rows.
	n2, err := d.SweepExpiredAPIKeys(ctx, time.Now())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep should be a no-op, deleted %d rows", n2)
	}
}

// TestCreateAPIKey_RequiresUserID confirms the post-103 invariant: an INSERT
// without user_id is rejected at the Go layer (and would be rejected at the
// SQLite layer too, redundantly).  This test depends on no schema introspection
// — it just calls the public API and asserts an error.
func TestCreateAPIKey_RequiresUserID(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "userid.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	ctx := context.Background()

	rec := db.APIKeyRecord{
		ID:        uuid.New().String(),
		Scope:     api.KeyScopeAdmin,
		KeyHash:   "deadbeef",
		CreatedAt: time.Now(),
		// UserID intentionally omitted.
	}
	err = d.CreateAPIKey(ctx, rec)
	if err == nil {
		t.Fatalf("expected error from CreateAPIKey without UserID, got nil")
	}
}
