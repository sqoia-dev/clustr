package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/config"
	"github.com/sqoia-dev/clonr/pkg/db"
	"github.com/sqoia-dev/clonr/pkg/server"
)

func newTestServer(t *testing.T) (*server.Server, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	cfg := config.ServerConfig{
		ListenAddr: ":0",
		ImageDir:   filepath.Join(dir, "images"),
		DBPath:     filepath.Join(dir, "test.db"),
		AuthToken:  "test-token",
		LogLevel:   "error",
	}

	srv := server.New(cfg, database)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func authHeader() string {
	return "Bearer test-token"
}

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body any, out any) *http.Response {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(b)
	} else {
		bodyReader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, bodyReader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
	return resp
}

func TestHealth(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/health", nil)
	req.Header.Set("Authorization", authHeader())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: got %d want 200", resp.StatusCode)
	}
	var h api.HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Status != "ok" {
		t.Errorf("status field: got %s", h.Status)
	}
}

func TestAuth_RequiresToken(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %d want 401", resp.StatusCode)
	}
}

func TestImages_CreateAndList(t *testing.T) {
	_, ts := newTestServer(t)

	createReq := api.CreateImageRequest{
		Name:    "rocky9-test",
		Version: "1.0.0",
		OS:      "Rocky Linux 9.3",
		Arch:    "x86_64",
		Format:  api.ImageFormatFilesystem,
		Tags:    []string{"test"},
	}

	var created api.BaseImage
	resp := doJSON(t, ts, http.MethodPost, "/api/v1/images", createReq, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status: got %d want 201", resp.StatusCode)
	}
	if created.ID == "" {
		t.Error("created image should have an ID")
	}
	if created.Name != "rocky9-test" {
		t.Errorf("name: got %s", created.Name)
	}
	if created.Status != api.ImageStatusBuilding {
		t.Errorf("status: got %s want building", created.Status)
	}

	// List should contain our image.
	var list api.ListImagesResponse
	doJSON(t, ts, http.MethodGet, "/api/v1/images", nil, &list)
	if list.Total != 1 {
		t.Errorf("total: got %d want 1", list.Total)
	}

	// Get by ID.
	var got api.BaseImage
	resp2 := doJSON(t, ts, http.MethodGet, "/api/v1/images/"+created.ID, nil, &got)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("get status: got %d want 200", resp2.StatusCode)
	}
	if got.ID != created.ID {
		t.Errorf("id mismatch: got %s want %s", got.ID, created.ID)
	}
}

func TestImages_NotFound(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images/does-not-exist", nil)
	req.Header.Set("Authorization", authHeader())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d want 404", resp.StatusCode)
	}
}

func TestImages_Archive(t *testing.T) {
	_, ts := newTestServer(t)

	createReq := api.CreateImageRequest{Name: "to-archive", Format: api.ImageFormatBlock}
	var created api.BaseImage
	doJSON(t, ts, http.MethodPost, "/api/v1/images", createReq, &created)

	resp := doJSON(t, ts, http.MethodDelete, "/api/v1/images/"+created.ID, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("archive status: got %d want 204", resp.StatusCode)
	}

	var got api.BaseImage
	doJSON(t, ts, http.MethodGet, "/api/v1/images/"+created.ID, nil, &got)
	if got.Status != api.ImageStatusArchived {
		t.Errorf("status after archive: got %s want archived", got.Status)
	}
}

func TestImages_ValidationError(t *testing.T) {
	_, ts := newTestServer(t)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/images",
		strings.NewReader(`{"version":"1.0"}`)) // missing name
	req.Header.Set("Authorization", authHeader())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: got %d want 400", resp.StatusCode)
	}
}

func TestNodes_CreateAndGet(t *testing.T) {
	_, ts := newTestServer(t)

	// First create an image to reference.
	var img api.BaseImage
	doJSON(t, ts, http.MethodPost, "/api/v1/images", api.CreateImageRequest{
		Name: "base", Format: api.ImageFormatFilesystem,
	}, &img)

	nodeReq := api.CreateNodeConfigRequest{
		Hostname:    "compute-01",
		FQDN:        "compute-01.hpc.local",
		PrimaryMAC:  "aa:bb:cc:dd:ee:01",
		BaseImageID: img.ID,
		Groups:      []string{"compute"},
		CustomVars:  map[string]string{"role": "worker"},
	}
	var node api.NodeConfig
	resp := doJSON(t, ts, http.MethodPost, "/api/v1/nodes", nodeReq, &node)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create node status: got %d want 201", resp.StatusCode)
	}
	if node.ID == "" {
		t.Error("node should have ID")
	}

	// Get by ID.
	var got api.NodeConfig
	doJSON(t, ts, http.MethodGet, "/api/v1/nodes/"+node.ID, nil, &got)
	if got.Hostname != "compute-01" {
		t.Errorf("hostname: got %s", got.Hostname)
	}

	// Get by MAC.
	var byMAC api.NodeConfig
	resp2 := doJSON(t, ts, http.MethodGet, "/api/v1/nodes/by-mac/aa:bb:cc:dd:ee:01", nil, &byMAC)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("by-mac status: got %d want 200", resp2.StatusCode)
	}
	if byMAC.ID != node.ID {
		t.Errorf("by-mac id: got %s want %s", byMAC.ID, node.ID)
	}
}

func TestNodes_Update(t *testing.T) {
	_, ts := newTestServer(t)

	var img api.BaseImage
	doJSON(t, ts, http.MethodPost, "/api/v1/images", api.CreateImageRequest{
		Name: "base", Format: api.ImageFormatFilesystem,
	}, &img)

	var node api.NodeConfig
	doJSON(t, ts, http.MethodPost, "/api/v1/nodes", api.CreateNodeConfigRequest{
		Hostname: "old-name", PrimaryMAC: "aa:bb:cc:dd:ee:ff", BaseImageID: img.ID,
	}, &node)

	updateReq := api.UpdateNodeConfigRequest{
		Hostname:    "new-name",
		PrimaryMAC:  "aa:bb:cc:dd:ee:ff",
		BaseImageID: img.ID,
	}
	var updated api.NodeConfig
	resp := doJSON(t, ts, http.MethodPut, "/api/v1/nodes/"+node.ID, updateReq, &updated)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status: got %d want 200", resp.StatusCode)
	}
	if updated.Hostname != "new-name" {
		t.Errorf("hostname: got %s", updated.Hostname)
	}
}

func TestNodes_Delete(t *testing.T) {
	_, ts := newTestServer(t)

	var img api.BaseImage
	doJSON(t, ts, http.MethodPost, "/api/v1/images", api.CreateImageRequest{
		Name: "base", Format: api.ImageFormatFilesystem,
	}, &img)

	var node api.NodeConfig
	doJSON(t, ts, http.MethodPost, "/api/v1/nodes", api.CreateNodeConfigRequest{
		Hostname: "to-delete", PrimaryMAC: "de:ad:be:ef:00:01", BaseImageID: img.ID,
	}, &node)

	resp := doJSON(t, ts, http.MethodDelete, "/api/v1/nodes/"+node.ID, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status: got %d want 204", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/nodes/"+node.ID, nil)
	req.Header.Set("Authorization", authHeader())
	resp2, err2 := http.DefaultClient.Do(req)
	if err2 != nil {
		t.Fatalf("after delete request: %v", err2)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("after delete: got %d want 404", resp2.StatusCode)
	}
}

func TestImages_Status(t *testing.T) {
	_, ts := newTestServer(t)

	var img api.BaseImage
	doJSON(t, ts, http.MethodPost, "/api/v1/images", api.CreateImageRequest{
		Name: "status-test", Format: api.ImageFormatFilesystem,
	}, &img)

	var status map[string]any
	resp := doJSON(t, ts, http.MethodGet, "/api/v1/images/"+img.ID+"/status", nil, &status)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if status["status"] != string(api.ImageStatusBuilding) {
		t.Errorf("status field: got %v", status["status"])
	}
}
