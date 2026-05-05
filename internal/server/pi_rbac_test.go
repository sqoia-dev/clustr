package server

// pi_rbac_test.go — RBAC tests for the PI role (Sprint C.5 — C5-1-5)
//
// Covers:
//   - PI login scope maps to api.KeyScope("pi")
//   - requirePI() middleware: pi scope passes, viewer/readonly blocked
//   - PI cannot reach admin routes (requireScope(true) blocks "pi" scope)
//   - PI can be assigned to a NodeGroup; ownership check works
//   - Expansion requests and member requests are createable

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sqoia-dev/clustr/internal/db"
	"github.com/sqoia-dev/clustr/pkg/api"
)

// TestPIScope verifies the requirePI middleware correctly gates by scope.
func TestPIScope(t *testing.T) {
	// A handler that returns 200 if reached.
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	piMW := requirePI()

	tests := []struct {
		name     string
		scope    api.KeyScope
		wantCode int
	}{
		{"admin", api.KeyScopeAdmin, http.StatusOK},
		{"operator", api.KeyScopeOperator, http.StatusOK},
		{"pi", api.KeyScope("pi"), http.StatusOK},
		{"readonly", api.KeyScope("readonly"), http.StatusForbidden},
		{"viewer", api.KeyScope("viewer"), http.StatusForbidden},
		{"no scope", api.KeyScope(""), http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/portal/pi/groups", nil)
			if tt.scope != "" {
				ctx := context.WithValue(req.Context(), ctxKeyScope{}, tt.scope)
				req = req.WithContext(ctx)
			}
			rr := httptest.NewRecorder()
			piMW(ok).ServeHTTP(rr, req)

			if rr.Code != tt.wantCode {
				t.Errorf("scope=%q: got %d, want %d", tt.scope, rr.Code, tt.wantCode)
			}
		})
	}
}

// TestPICannotReachAdmin verifies that a pi-scoped session cannot reach admin routes
// (requireScope(adminOnly=true) blocks "pi" scope).
func TestPICannotReachAdmin(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mw := requireScope(true) // adminOnly=true

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/users", nil)
	ctx := context.WithValue(req.Context(), ctxKeyScope{}, api.KeyScope("pi"))
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	mw(ok).ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("pi scope should not reach admin route: got %d", rr.Code)
	}
}

// TestPINodeGroupOwnership verifies the DB ownership check.
//
// SKIPPED — #250: migration 103 dropped node_groups.pi_user_id (dangling FK to
// _users_old).  The PI workflow tested here (SetNodeGroupPI / IsNodeGroupOwnedByPI)
// is dead code per the founder's "wiped scope stays wiped" directive; this test
// and its sibling TestPIMemberRequestCreate exercise that code path and break
// when the column is dropped.  Removing the dead Go code is a separate scope
// (identity-model redesign-by-another-name) deliberately deferred from #250.
func TestPINodeGroupOwnership(t *testing.T) {
	t.Skip("PI workflow wiped 2026-04-29; column dropped in migration 103; dead Go code awaits follow-up cleanup")
	database := newTestDB(t)

	// Create two users.
	piUser1ID := "pi-user-001"
	piUser2ID := "pi-user-002"
	for _, uid := range []string{piUser1ID, piUser2ID} {
		if err := database.CreateUser(context.Background(), db.UserRecord{
			ID:           uid,
			Username:     uid,
			PasswordHash: "x",
			Role:         db.UserRolePI,
			CreatedAt:    time.Now(),
		}); err != nil {
			t.Fatalf("create user %s: %v", uid, err)
		}
	}

	// Create a node group with pi1 as PI.
	groupID := "grp-001"
	if err := database.CreateNodeGroupFull(context.Background(), api.NodeGroup{
		ID:   groupID,
		Name: "compute",
	}); err != nil {
		t.Fatalf("create node group: %v", err)
	}
	if err := database.SetNodeGroupPI(context.Background(), groupID, piUser1ID); err != nil {
		t.Fatalf("set PI: %v", err)
	}

	// pi1 should own the group.
	owned, err := database.IsNodeGroupOwnedByPI(context.Background(), groupID, piUser1ID)
	if err != nil {
		t.Fatalf("IsNodeGroupOwnedByPI: %v", err)
	}
	if !owned {
		t.Error("pi1 should own the group")
	}

	// pi2 should NOT own the group.
	owned, err = database.IsNodeGroupOwnedByPI(context.Background(), groupID, piUser2ID)
	if err != nil {
		t.Fatalf("IsNodeGroupOwnedByPI: %v", err)
	}
	if owned {
		t.Error("pi2 should not own the group")
	}
}

// TestPIMemberRequestCreate verifies creating and resolving a PI member request.
//
// SKIPPED — see TestPINodeGroupOwnership above.  Same dead-workflow scope.
func TestPIMemberRequestCreate(t *testing.T) {
	t.Skip("PI workflow wiped 2026-04-29; column dropped in migration 103; dead Go code awaits follow-up cleanup")
	database := newTestDB(t)

	piID := "pi-user-001"
	if err := database.CreateUser(context.Background(), db.UserRecord{
		ID:           piID,
		Username:     "piuser",
		PasswordHash: "x",
		Role:         db.UserRolePI,
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("create pi user: %v", err)
	}

	groupID := "grp-001"
	if err := database.CreateNodeGroupFull(context.Background(), api.NodeGroup{
		ID:   groupID,
		Name: "compute",
	}); err != nil {
		t.Fatalf("create node group: %v", err)
	}
	if err := database.SetNodeGroupPI(context.Background(), groupID, piID); err != nil {
		t.Fatalf("set PI: %v", err)
	}

	// Create a member request.
	req := db.PIMemberRequest{
		ID:           "req-001",
		GroupID:      groupID,
		PIUserID:     piID,
		LDAPUsername: "jsmith",
		RequestedAt:  time.Now(),
	}
	if err := database.CreatePIMemberRequest(context.Background(), req); err != nil {
		t.Fatalf("CreatePIMemberRequest: %v", err)
	}

	// List pending requests.
	reqs, err := database.ListPIMemberRequests(context.Background(), groupID, "pending")
	if err != nil {
		t.Fatalf("ListPIMemberRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 pending request, got %d", len(reqs))
	}
	if reqs[0].LDAPUsername != "jsmith" {
		t.Errorf("want jsmith, got %s", reqs[0].LDAPUsername)
	}

	// Resolve to approved.
	if err := database.ResolvePIMemberRequest(context.Background(), "req-001", "approved", "admin-001"); err != nil {
		t.Fatalf("ResolvePIMemberRequest: %v", err)
	}

	// Approved request should not appear in pending list.
	pending, err := database.ListPIMemberRequests(context.Background(), groupID, "pending")
	if err != nil {
		t.Fatalf("ListPIMemberRequests after resolve: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("want 0 pending, got %d", len(pending))
	}

	// Should appear in approved list.
	approved, err := database.ListPIMemberRequests(context.Background(), groupID, "approved")
	if err != nil {
		t.Fatalf("ListPIMemberRequests approved: %v", err)
	}
	if len(approved) != 1 {
		t.Fatalf("want 1 approved, got %d", len(approved))
	}
}

// TestPIExpansionRequestCreate verifies creating a PI expansion request.
func TestPIExpansionRequestCreate(t *testing.T) {
	database := newTestDB(t)

	piID := "pi-user-001"
	if err := database.CreateUser(context.Background(), db.UserRecord{
		ID:           piID,
		Username:     "piuser",
		PasswordHash: "x",
		Role:         db.UserRolePI,
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("create pi user: %v", err)
	}

	groupID := "grp-001"
	if err := database.CreateNodeGroupFull(context.Background(), api.NodeGroup{
		ID:   groupID,
		Name: "compute",
	}); err != nil {
		t.Fatalf("create node group: %v", err)
	}

	req := db.PIExpansionRequest{
		ID:            "exp-001",
		GroupID:       groupID,
		PIUserID:      piID,
		Justification: "Need more nodes for grant XYZ computation",
		RequestedAt:   time.Now(),
	}
	if err := database.CreatePIExpansionRequest(context.Background(), req); err != nil {
		t.Fatalf("CreatePIExpansionRequest: %v", err)
	}

	reqs, err := database.ListPIExpansionRequests(context.Background(), groupID, "pending")
	if err != nil {
		t.Fatalf("ListPIExpansionRequests: %v", err)
	}
	if len(reqs) != 1 {
		t.Fatalf("want 1 expansion request, got %d", len(reqs))
	}
	if !strings.Contains(reqs[0].Justification, "grant XYZ") {
		t.Errorf("justification mismatch: %s", reqs[0].Justification)
	}
}

// TestPIUtilizationQuery verifies the utilization aggregation returns without error
// for an empty group (fixture with 0 nodes).
func TestPIUtilizationQuery(t *testing.T) {
	database := newTestDB(t)

	groupID := "grp-util-001"
	if err := database.CreateNodeGroupFull(context.Background(), api.NodeGroup{
		ID:   groupID,
		Name: "compute",
	}); err != nil {
		t.Fatalf("create node group: %v", err)
	}

	util, err := database.GetPIGroupUtilization(context.Background(), groupID)
	if err != nil {
		t.Fatalf("GetPIGroupUtilization: %v", err)
	}
	if util.GroupID != groupID {
		t.Errorf("want group_id=%s, got %s", groupID, util.GroupID)
	}
	if util.NodeCount != 0 {
		t.Errorf("want 0 nodes, got %d", util.NodeCount)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustMarshal(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func newJSONRequest(method, path string, body interface{}) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}
