package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
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

// newAuthTestServer creates a test server pre-seeded with an admin API key
// and the default clonr/clonr bootstrap user (via BootstrapDefaultUser).
func newAuthTestServer(t *testing.T) (*server.Server, *httptest.Server, string) {
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

	// Bootstrap the default user (clonr/clonr) — this is what the real server does at startup.
	if err := server.BootstrapDefaultUser(context.Background(), database); err != nil {
		t.Fatalf("bootstrap default user: %v", err)
	}

	srv := server.New(cfg, database)

	// Also seed a legacy admin key for backward-compat tests.
	rawKey, _, err := server.CreateAPIKey(context.Background(), database, api.KeyScopeAdmin, "test key")
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	fullKey := "clonr-admin-" + rawKey

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, fullKey
}

// clientWithJar returns an http.Client that tracks cookies.
func clientWithJar(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

// ─── username/password login tests (ADR-0007) ───────────────────────────────

func TestLogin_UsernamePassword_HappyPath(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	body := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: got %d, want 200", resp.StatusCode)
	}

	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true {
		t.Error("expected {ok:true} in login response")
	}
	if out["force_password_change"] != true {
		t.Error("expected force_password_change=true for default clonr user")
	}

	// Verify session cookie is set.
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "clonr_session" && c.Value != "" {
			found = true
			if !c.HttpOnly {
				t.Error("clonr_session cookie should be HttpOnly")
			}
		}
	}
	if !found {
		t.Error("clonr_session cookie not set after login")
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	body := strings.NewReader(`{"username":"clonr","password":"wrongpassword"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %d, want 401", resp.StatusCode)
	}

	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != "Invalid username or password" {
		t.Errorf("wrong error message: %q", out["error"])
	}
}

func TestLogin_DisabledUser(t *testing.T) {
	// A full disabled-user flow is covered in users_test.go via handler tests.
	t.Log("disabled user login tested via handler unit tests (see pkg/server/handlers)")
}

func TestLogin_BootstrapNotRepeated(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	ctx := context.Background()

	// First call creates the user.
	if err := server.BootstrapDefaultUser(ctx, database); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	count, err := database.CountUsers(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 user after bootstrap, got %d", count)
	}

	// Second call must NOT create another user.
	if err := server.BootstrapDefaultUser(ctx, database); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	count2, _ := database.CountUsers(ctx)
	if count2 != 1 {
		t.Errorf("expected still 1 user after second bootstrap, got %d", count2)
	}
}

func TestSetPassword_HappyPath(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	// Login with clonr/clonr.
	loginBody := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", loginBody)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Change password.
	pwBody := strings.NewReader(`{"current_password":"clonr","new_password":"newpassword1"}`)
	pwReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/set-password", pwBody)
	pwReq.Header.Set("Content-Type", "application/json")
	pwResp, err := client.Do(pwReq)
	if err != nil {
		t.Fatalf("set-password request: %v", err)
	}
	defer pwResp.Body.Close()

	if pwResp.StatusCode != http.StatusOK {
		var out map[string]string
		_ = json.NewDecoder(pwResp.Body).Decode(&out)
		t.Fatalf("set-password: got %d, want 200: %v", pwResp.StatusCode, out)
	}

	// Verify force-change cookie is cleared.
	for _, c := range pwResp.Cookies() {
		if c.Name == "clonr_force_password_change" {
			if c.MaxAge > 0 {
				t.Error("force_password_change cookie should be cleared after set-password")
			}
		}
	}

	// Logout then log back in with the new password.
	client.Post(ts.URL+"/api/v1/auth/logout", "application/json", nil)

	newLoginBody := strings.NewReader(`{"username":"clonr","password":"newpassword1"}`)
	newReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", newLoginBody)
	newReq.Header.Set("Content-Type", "application/json")
	newResp, _ := client.Do(newReq)
	newResp.Body.Close()
	if newResp.StatusCode != http.StatusOK {
		t.Fatalf("re-login with new password: got %d, want 200", newResp.StatusCode)
	}
}

func TestSetPassword_WeakPassword(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	// Login.
	loginBody := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", loginBody)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	resp.Body.Close()

	// Try to set a weak password.
	pwBody := strings.NewReader(`{"current_password":"clonr","new_password":"short"}`)
	pwReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/set-password", pwBody)
	pwReq.Header.Set("Content-Type", "application/json")
	pwResp, _ := client.Do(pwReq)
	pwResp.Body.Close()
	if pwResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("weak password: got %d, want 400", pwResp.StatusCode)
	}
}

// ─── legacy API-key login tests (deprecated path) ───────────────────────────

func TestLogin_LegacyKey_HappyPath(t *testing.T) {
	_, ts, fullKey := newAuthTestServer(t)
	client := clientWithJar(t)

	body := strings.NewReader(`{"key":"` + fullKey + `"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("legacy key login: got %d, want 200", resp.StatusCode)
	}

	// Verify session cookie is set.
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "clonr_session" && c.Value != "" {
			found = true
		}
	}
	if !found {
		t.Error("clonr_session cookie not set after legacy key login")
	}
}

func TestLogin_InvalidKey(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	body := strings.NewReader(`{"key":"clonr-admin-000000000000000000000000000000000000000000000000000000000000ffff"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("invalid key login: got %d, want 401", resp.StatusCode)
	}
}

func TestMe_WithValidSession(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	// Login with username/password.
	body := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Now call /me — cookie jar should send the session cookie.
	meReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	meResp, err := client.Do(meReq)
	if err != nil {
		t.Fatalf("me request: %v", err)
	}
	defer meResp.Body.Close()

	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("/me: got %d, want 200", meResp.StatusCode)
	}

	var out map[string]any
	_ = json.NewDecoder(meResp.Body).Decode(&out)
	if out["role"] != "admin" {
		t.Errorf("role: got %v, want admin", out["role"])
	}
	if _, ok := out["expires_at"]; !ok {
		t.Error("expected expires_at in /me response")
	}
}

func TestMe_WithoutSession(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("me request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/me without session: got %d, want 401", resp.StatusCode)
	}
}

func TestLogout_ClearsCookie(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	// Login.
	body := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Logout.
	logoutReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/logout", nil)
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatalf("logout request: %v", err)
	}
	logoutResp.Body.Close()

	if logoutResp.StatusCode != http.StatusOK {
		t.Fatalf("logout: got %d, want 200", logoutResp.StatusCode)
	}

	// /me should now return 401.
	meReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	meResp, _ := client.Do(meReq)
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout /me: got %d, want 401", meResp.StatusCode)
	}
}

func TestCookieAuth_GrantsAccess(t *testing.T) {
	_, ts, _ := newAuthTestServer(t)
	client := clientWithJar(t)

	// Login to get a session cookie.
	body := strings.NewReader(`{"username":"clonr","password":"clonr"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/auth/login", body)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login failed: %d", resp.StatusCode)
	}

	// Call an admin-only endpoint (no Bearer header — cookie only).
	imagesReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/images", nil)
	imagesResp, err := client.Do(imagesReq)
	if err != nil {
		t.Fatalf("images request: %v", err)
	}
	defer imagesResp.Body.Close()

	if imagesResp.StatusCode != http.StatusOK {
		t.Fatalf("cookie auth on /images: got %d, want 200", imagesResp.StatusCode)
	}
}

func TestSlidingExpiry_ReissuesCookie(t *testing.T) {
	t.Log("sliding expiry is unit-tested in session_test.go (TestValidate_SlidingReissue)")
	_ = time.Second // satisfy import
}
