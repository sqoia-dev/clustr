package server_test

// ADR-0008: Post-Reboot Verification — server-side endpoint and state model tests.
//
// Covers:
//   - POST /nodes/{id}/verify-boot with valid node-scoped token → 204, fields updated
//   - POST /nodes/{id}/verify-boot with admin token bound to a different node → 403
//   - POST /nodes/{id}/verify-boot with no token → 401
//   - Heartbeat: second call updates last_seen_at but not deploy_verified_booted_at
//   - Timeout scanner: node with old deploy_completed_preboot_at gets timeout recorded
//   - Migration 022: dual-write and back-compat state derivation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/server"
)

// newVerifyBootServer creates a test server with auth enabled and a seeded node.
// Returns (httptest.Server, database, nodeID, node-scoped raw key for that node).
func newVerifyBootServer(t *testing.T) (*httptest.Server, *db.DB, string, string) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.ServerConfig{
		ListenAddr:    ":0",
		ImageDir:      filepath.Join(dir, "images"),
		DBPath:        filepath.Join(dir, "test.db"),
		LogLevel:      "error",
		SessionSecret: "test-session-secret-32-bytes-xxx",
		SessionSecure: false,
		VerifyTimeout: 5 * time.Minute,
	}
	srv := server.New(cfg, database)

	// Seed one node.
	nodeID := "verify-boot-test-node"
	nodeCfg := api.NodeConfig{
		ID:         nodeID,
		Hostname:   "seabios-vm206",
		PrimaryMAC: "aa:00:11:22:33:44",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := database.UpsertNodeByMAC(context.Background(), nodeCfg); err != nil {
		t.Fatalf("upsert node: %v", err)
	}
	// Put node into deployed_preboot state.
	if err := database.RecordDeploySucceeded(context.Background(), nodeID); err != nil {
		t.Fatalf("RecordDeploySucceeded: %v", err)
	}

	// Mint a node-scoped key for this node.
	raw, err := server.CreateNodeScopedKey(context.Background(), database, nodeID)
	if err != nil {
		t.Fatalf("CreateNodeScopedKey: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, database, nodeID, raw
}

// doVerifyBoot sends POST /nodes/{nodeID}/verify-boot with the given Bearer token.
func doVerifyBoot(t *testing.T, ts *httptest.Server, nodeID, bearerToken string, payload api.VerifyBootRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/nodes/"+nodeID+"/verify-boot",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// ─── HTTP endpoint tests ──────────────────────────────────────────────────────

func TestVerifyBoot_ValidNodeToken_Returns204AndUpdatesFields(t *testing.T) {
	ts, database, nodeID, rawNodeKey := newVerifyBootServer(t)
	nodeKey := "clonr-node-" + rawNodeKey

	payload := api.VerifyBootRequest{
		Hostname:       "seabios-vm206",
		KernelVersion:  "6.12.0-124.8.1.el10_1.x86_64",
		UptimeSeconds:  47.0,
		SystemctlState: "running",
		OSRelease:      "Rocky Linux 10.1 (Red Quartz)",
	}

	resp := doVerifyBoot(t, ts, nodeID, nodeKey, payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("verify-boot: got %d want 204", resp.StatusCode)
	}

	// Verify DB fields.
	got, err := database.GetNodeConfig(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetNodeConfig: %v", err)
	}
	if got.DeployVerifiedBootedAt == nil {
		t.Error("deploy_verified_booted_at should be set")
	}
	if got.LastSeenAt == nil {
		t.Error("last_seen_at should be set")
	}
	if got.State() != api.NodeStateDeployedVerified {
		t.Errorf("state: got %s want deployed_verified", got.State())
	}
}

func TestVerifyBoot_NoToken_Returns401(t *testing.T) {
	ts, _, nodeID, _ := newVerifyBootServer(t)
	payload := api.VerifyBootRequest{Hostname: "seabios-vm206"}

	resp := doVerifyBoot(t, ts, nodeID, "", payload)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("verify-boot no token: got %d want 401", resp.StatusCode)
	}
}

func TestVerifyBoot_WrongNodeToken_Returns403(t *testing.T) {
	// Mint a key bound to a DIFFERENT node and try to use it on nodeID.
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.ServerConfig{
		ListenAddr:    ":0",
		ImageDir:      filepath.Join(dir, "images"),
		DBPath:        filepath.Join(dir, "test.db"),
		LogLevel:      "error",
		SessionSecret: "test-session-secret-32-bytes-xxx",
		VerifyTimeout: 5 * time.Minute,
	}
	srv := server.New(cfg, database)

	node1 := api.NodeConfig{ID: "n1", Hostname: "node1", PrimaryMAC: "aa:00:00:00:00:01", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	node2 := api.NodeConfig{ID: "n2", Hostname: "node2", PrimaryMAC: "aa:00:00:00:00:02", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	for _, nc := range []api.NodeConfig{node1, node2} {
		if _, err := database.UpsertNodeByMAC(context.Background(), nc); err != nil {
			t.Fatalf("upsert %s: %v", nc.ID, err)
		}
	}

	// Key for node2, used on node1's endpoint.
	rawNode2, err := server.CreateNodeScopedKey(context.Background(), database, node2.ID)
	if err != nil {
		t.Fatalf("CreateNodeScopedKey: %v", err)
	}
	wrongKey := "clonr-node-" + rawNode2

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := doVerifyBoot(t, ts, node1.ID, wrongKey, api.VerifyBootRequest{Hostname: "node1"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("verify-boot wrong node: got %d want 403", resp.StatusCode)
	}
}

// ─── DB-layer tests ───────────────────────────────────────────────────────────

func TestVerifyBoot_Heartbeat_UpdatesLastSeenAtOnly(t *testing.T) {
	// Two consecutive RecordVerifyBooted calls: first sets deploy_verified_booted_at,
	// second leaves it unchanged but updates last_seen_at.
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "heartbeat.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	nodeCfg := api.NodeConfig{
		ID:         "heartbeat-node",
		Hostname:   "hb-node",
		PrimaryMAC: "bb:cc:dd:ee:ff:01",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := database.UpsertNodeByMAC(ctx, nodeCfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := database.RecordDeploySucceeded(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("RecordDeploySucceeded: %v", err)
	}

	// First call.
	if err := database.RecordVerifyBooted(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("first RecordVerifyBooted: %v", err)
	}
	n1, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get after first: %v", err)
	}
	if n1.DeployVerifiedBootedAt == nil {
		t.Fatal("deploy_verified_booted_at should be set after first call")
	}
	firstVerified := *n1.DeployVerifiedBootedAt

	// Wait long enough for timestamps to differ (SQLite stores unix seconds).
	time.Sleep(1100 * time.Millisecond)

	// Second call (heartbeat on reboot or retry).
	if err := database.RecordVerifyBooted(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("second RecordVerifyBooted: %v", err)
	}
	n2, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get after second: %v", err)
	}

	// deploy_verified_booted_at must be unchanged.
	if !n2.DeployVerifiedBootedAt.Equal(firstVerified) {
		t.Errorf("deploy_verified_booted_at changed: was %v now %v",
			firstVerified, *n2.DeployVerifiedBootedAt)
	}

	// last_seen_at must be more recent than first verified timestamp.
	if n2.LastSeenAt == nil || !n2.LastSeenAt.After(firstVerified) {
		t.Errorf("last_seen_at should be newer: last_seen=%v first_verified=%v",
			n2.LastSeenAt, firstVerified)
	}
}

func TestVerifyBoot_TimeoutScanner_SetsDeployVerifyTimeoutAt(t *testing.T) {
	// Verify the DB helpers used by the background scanner:
	// ListNodesAwaitingVerification returns stale nodes, RecordVerifyTimeout marks them.
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "scanner.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	nodeCfg := api.NodeConfig{
		ID:         "scanner-test-node",
		Hostname:   "stalled-node",
		PrimaryMAC: "cc:dd:ee:ff:00:01",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := database.UpsertNodeByMAC(ctx, nodeCfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Write deploy_completed_preboot_at = 10 minutes ago directly via SQL (simulates
	// a deploy that completed long before the scanner runs).
	tenMinsAgo := time.Now().Add(-10 * time.Minute).Unix()
	if _, err := database.SQL().ExecContext(ctx,
		`UPDATE node_configs SET deploy_completed_preboot_at = ?, reimage_pending = 0 WHERE id = ?`,
		tenMinsAgo, nodeCfg.ID,
	); err != nil {
		t.Fatalf("set deploy_completed_preboot_at: %v", err)
	}

	// Scanner cutoff is 5 minutes ago — this node qualifies.
	cutoff := time.Now().Add(-5 * time.Minute)
	awaiting, err := database.ListNodesAwaitingVerification(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListNodesAwaitingVerification: %v", err)
	}
	if len(awaiting) != 1 || awaiting[0].ID != nodeCfg.ID {
		t.Fatalf("expected 1 awaiting node, got %d: %+v", len(awaiting), awaiting)
	}

	// Record the timeout.
	if err := database.RecordVerifyTimeout(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("RecordVerifyTimeout: %v", err)
	}

	got, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.DeployVerifyTimeoutAt == nil {
		t.Error("deploy_verify_timeout_at should be set")
	}
	if got.State() != api.NodeStateDeployVerifyTimeout {
		t.Errorf("state: got %s want deploy_verify_timeout", got.State())
	}

	// Node must no longer appear in awaiting list.
	awaiting2, err := database.ListNodesAwaitingVerification(ctx, cutoff)
	if err != nil {
		t.Fatalf("second ListNodesAwaitingVerification: %v", err)
	}
	for _, n := range awaiting2 {
		if n.ID == nodeCfg.ID {
			t.Error("timed-out node should not appear in awaiting-verification list")
		}
	}
}

func TestMigration022_DualWrite_BackCompat(t *testing.T) {
	// Verify the dual-write path and back-compat state logic.
	ctx := context.Background()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "migration022.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	nodeCfg := api.NodeConfig{
		ID:         "migration022-test-node",
		Hostname:   "legacy-node",
		PrimaryMAC: "dd:ee:ff:00:01:02",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if _, err := database.UpsertNodeByMAC(ctx, nodeCfg); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Step 1: Simulate a legacy row (only last_deploy_succeeded_at set, no new fields).
	legacyTs := time.Now().Add(-1 * time.Hour).Unix()
	if _, err := database.SQL().ExecContext(ctx,
		`UPDATE node_configs SET last_deploy_succeeded_at = ?, reimage_pending = 0 WHERE id = ?`,
		legacyTs, nodeCfg.ID,
	); err != nil {
		t.Fatalf("set legacy timestamp: %v", err)
	}
	legacy, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	// Back-compat: legacy row → deployed_verified (full success was proven before ADR-0008).
	if legacy.State() != api.NodeStateDeployedVerified {
		t.Errorf("legacy back-compat state: got %s want deployed_verified", legacy.State())
	}
	if legacy.LastDeploySucceededAt == nil {
		t.Error("legacy: last_deploy_succeeded_at should be non-nil")
	}

	// Step 2: Use new dual-write path.
	if err := database.RecordDeploySucceeded(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("RecordDeploySucceeded: %v", err)
	}
	afterDualWrite, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get after dual-write: %v", err)
	}
	if afterDualWrite.DeployCompletedPrebootAt == nil {
		t.Error("deploy_completed_preboot_at should be set after RecordDeploySucceeded")
	}
	if afterDualWrite.LastDeploySucceededAt == nil {
		t.Error("last_deploy_succeeded_at back-compat field should also be set (dual-write)")
	}
	// After dual-write, the node is in deployed_preboot (waiting for OS phone-home).
	if afterDualWrite.State() != api.NodeStateDeployedPreboot {
		t.Errorf("dual-write state: got %s want deployed_preboot", afterDualWrite.State())
	}

	// Step 3: Phone home — transition to deployed_verified.
	if err := database.RecordVerifyBooted(ctx, nodeCfg.ID); err != nil {
		t.Fatalf("RecordVerifyBooted: %v", err)
	}
	final, err := database.GetNodeConfig(ctx, nodeCfg.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if final.State() != api.NodeStateDeployedVerified {
		t.Errorf("final state: got %s want deployed_verified", final.State())
	}
}
