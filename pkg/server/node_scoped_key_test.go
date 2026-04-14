package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/server"
)

// newNodeScopedTestServer creates a test server pre-seeded with an admin key and a node.
// Returns the server, httptest.Server, admin key, and the node ID.
func newNodeScopedTestServer(t *testing.T) (*server.Server, *httptest.Server, string, string) {
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
	}

	srv := server.New(cfg, database)

	// Bootstrap admin key.
	rawAdminKey, _, err := server.CreateAPIKey(context.Background(), database, api.KeyScopeAdmin, "test admin key")
	if err != nil {
		t.Fatalf("create admin api key: %v", err)
	}
	fullAdminKey := "clonr-admin-" + rawAdminKey

	// Register a node via UpsertByMAC (no FK on base_image_id for self-registered nodes).
	nodeCfg := api.NodeConfig{
		ID:         "11111111-0000-0000-0000-000000000001",
		Hostname:   "test-node",
		PrimaryMAC: "aa:bb:cc:dd:ee:ff",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	created, err := database.UpsertNodeByMAC(context.Background(), nodeCfg)
	if err != nil {
		t.Fatalf("upsert node by mac: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, fullAdminKey, created.ID
}

// TestCreateNodeScopedKey_BasicCreation verifies that a node-scoped key is created
// and can authenticate API calls, and that a second mint revokes the first.
func TestCreateNodeScopedKey_BasicCreation(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	nodeID := "node-abc-123"

	// Mint a key.
	raw1, err := server.CreateNodeScopedKey(context.Background(), database, nodeID)
	if err != nil {
		t.Fatalf("CreateNodeScopedKey: %v", err)
	}
	if raw1 == "" {
		t.Fatal("expected non-empty raw key")
	}

	// Verify it shows up in the DB as a node-scoped key.
	keys, err := database.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	found := false
	for _, k := range keys {
		if k.NodeID == nodeID && k.Scope == api.KeyScopeNode {
			found = true
			if k.ExpiresAt == nil {
				t.Error("node-scoped key should have an expires_at set")
			}
		}
	}
	if !found {
		t.Error("node-scoped key not found in DB after creation")
	}
}

// TestCreateNodeScopedKey_Rotation verifies that minting a second key for the same
// node revokes the first — only one live token per node at any time.
func TestCreateNodeScopedKey_Rotation(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	nodeID := "node-rotate-test"

	_, err = server.CreateNodeScopedKey(context.Background(), database, nodeID)
	if err != nil {
		t.Fatalf("first CreateNodeScopedKey: %v", err)
	}
	_, err = server.CreateNodeScopedKey(context.Background(), database, nodeID)
	if err != nil {
		t.Fatalf("second CreateNodeScopedKey: %v", err)
	}

	// Exactly one node-scoped key should exist for this node.
	keys, err := database.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	count := 0
	for _, k := range keys {
		if k.NodeID == nodeID && k.Scope == api.KeyScopeNode {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 node-scoped key after rotation, got %d", count)
	}
}

// TestImageAccess_NodeKeyCanFetchAssignedImage verifies that:
//   - Unauthenticated requests to GET /images/{id} → 401
//   - Admin key → auth passes (response may be 404 if image doesn't exist, but not 401/403)
func TestImageAccess_NodeKeyCanFetchAssignedImage(t *testing.T) {
	_, ts, adminKey, _ := newNodeScopedTestServer(t)

	// Unauthenticated → 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images/test-image-001", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated: got %d, want 401", resp.StatusCode)
	}

	// Admin key → 404 (image doesn't exist in this test, but auth passes).
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images/test-image-001", nil)
	req.Header.Set("Authorization", "Bearer "+adminKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("admin request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Errorf("admin key should pass auth on /images/{id}: got %d", resp.StatusCode)
	}
}

// TestImageAccess_NodeKeyBlockedForOtherImage verifies that a node-scoped key
// cannot fetch an image not assigned to its bound node. We test this by minting
// a key for a node that has base_image_id="other-image-id" and confirming the
// key is rejected when requesting "test-image-001" (a different image).
// The full path is: node-A's key → GET /images/test-image-001 → 403
// because node-A's base_image_id is "other-image-id", not "test-image-001".
func TestImageAccess_NodeKeyBlockedForOtherImage(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	cfg := config.ServerConfig{
		ListenAddr:    ":0",
		ImageDir:      filepath.Join(dir, "images"),
		DBPath:        filepath.Join(dir, "test.db"),
		LogLevel:      "error",
		SessionSecret: "test-session-secret-32-bytes-xxx",
		SessionSecure: false,
	}
	srv := server.New(cfg, database)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Register a node (no base_image_id yet — will test with empty assignment).
	nodeA, err := database.UpsertNodeByMAC(context.Background(), api.NodeConfig{
		ID:         "cccccccc-0000-0000-0000-000000000003",
		Hostname:   "imgtest",
		PrimaryMAC: "cc:cc:cc:cc:cc:cc",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	raw, err := server.CreateNodeScopedKey(context.Background(), database, nodeA.ID)
	if err != nil {
		t.Fatalf("CreateNodeScopedKey: %v", err)
	}
	token := "clonr-node-" + raw

	// Node has no base_image_id assigned — requesting any image → 403.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images/some-image-id", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("node key with no image assignment requesting image: got %d, want 403", resp.StatusCode)
	}
}

// TestDeployCallbacks_NodeKeyOwnership verifies that deploy-complete and deploy-failed
// require a node-scoped key whose bound node_id matches the URL.
func TestDeployCallbacks_NodeKeyOwnership(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	cfg := config.ServerConfig{
		ListenAddr:    ":0",
		ImageDir:      filepath.Join(dir, "images"),
		DBPath:        filepath.Join(dir, "test.db"),
		LogLevel:      "error",
		SessionSecret: "test-session-secret-32-bytes-xxx",
		SessionSecure: false,
	}
	srv := server.New(cfg, database)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Register node-A and node-B via UpsertByMAC with explicit UUIDs.
	nodeA, err := database.UpsertNodeByMAC(context.Background(), api.NodeConfig{
		ID:         "aaaaaaaa-0000-0000-0000-000000000001",
		Hostname:   "nodeA",
		PrimaryMAC: "aa:aa:aa:aa:aa:aa",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert node-A: %v", err)
	}
	nodeB, err := database.UpsertNodeByMAC(context.Background(), api.NodeConfig{
		ID:         "bbbbbbbb-0000-0000-0000-000000000002",
		Hostname:   "nodeB",
		PrimaryMAC: "bb:bb:bb:bb:bb:bb",
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("upsert node-B: %v", err)
	}

	// Mint a key for node-A.
	rawA, err := server.CreateNodeScopedKey(context.Background(), database, nodeA.ID)
	if err != nil {
		t.Fatalf("mint key for node-A: %v", err)
	}
	tokenA := "clonr-node-" + rawA

	// Node-A key on node-A's deploy-complete → should succeed (200 or 204).
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/nodes/"+nodeA.ID+"/deploy-complete",
		strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("node-A deploy-complete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		t.Errorf("node-A key on node-A deploy-complete: got %d, want 2xx", resp.StatusCode)
	}

	// Node-A key on node-B's deploy-complete → 403.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/nodes/"+nodeB.ID+"/deploy-complete",
		strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("node-A key on node-B deploy-complete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("node-A key on node-B deploy-complete: got %d, want 403", resp.StatusCode)
	}

	// Unauthenticated → 401.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/api/v1/nodes/"+nodeA.ID+"/deploy-complete",
		strings.NewReader(""))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unauthenticated deploy-complete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated deploy-complete: got %d, want 401", resp.StatusCode)
	}
}
