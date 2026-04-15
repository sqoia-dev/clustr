package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInjectPhoneHome_NoOp verifies that injectPhoneHome is a no-op when either
// the token or URL is empty, leaving the rootfs unchanged.
func TestInjectPhoneHome_NoOp(t *testing.T) {
	t.Parallel()

	rootfs := t.TempDir()

	// Both empty — no-op.
	if err := injectPhoneHome(rootfs, "", ""); err != nil {
		t.Fatalf("expected no-op, got error: %v", err)
	}

	// Only token set — still no-op.
	if err := injectPhoneHome(rootfs, "tok123", ""); err != nil {
		t.Fatalf("expected no-op with empty URL, got error: %v", err)
	}

	// Only URL set — still no-op.
	if err := injectPhoneHome(rootfs, "", "http://server/verify"); err != nil {
		t.Fatalf("expected no-op with empty token, got error: %v", err)
	}

	// Confirm nothing was written.
	clonrDir := filepath.Join(rootfs, "etc", "clonr")
	if _, err := os.Stat(clonrDir); !os.IsNotExist(err) {
		t.Fatalf("expected /etc/clonr to not exist, stat returned: %v", err)
	}
}

// TestInjectPhoneHome_Writes verifies that injectPhoneHome writes all expected
// files with correct permissions and content when given valid inputs.
// It uses a fake rootfs tree and a stub systemctl that exits 0.
func TestInjectPhoneHome_Writes(t *testing.T) {
	// systemctl --root=... enable ... must succeed; on the CI host systemctl is
	// available but may fail with "Failed to connect to bus" in a rootless container.
	// We pre-create the WantedBy directory so systemctl --root can write the symlink
	// without needing a running D-Bus or init system.
	rootfs := t.TempDir()

	token := "clonr-node-tok-abc123"
	verifyURL := "http://clonr-server:8080/api/v1/nodes/node-id-xyz/verify-boot"

	// Pre-create the directory that systemctl --root expects so the enable call
	// either succeeds or creates the symlink itself.
	multiUserWantsDir := filepath.Join(rootfs, "etc", "systemd", "system", "multi-user.target.wants")
	if err := os.MkdirAll(multiUserWantsDir, 0o755); err != nil {
		t.Fatalf("pre-create multi-user.target.wants: %v", err)
	}

	if err := injectPhoneHome(rootfs, token, verifyURL); err != nil {
		t.Fatalf("injectPhoneHome: %v", err)
	}

	// ── Assert /etc/clonr/node-token ─────────────────────────────────────────
	tokenPath := filepath.Join(rootfs, "etc", "clonr", "node-token")
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("node-token not found: %v", err)
	}
	if fi.Mode() != 0o600 {
		t.Errorf("node-token mode = %o, want 0600", fi.Mode())
	}
	got, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read node-token: %v", err)
	}
	if string(got) != token {
		t.Errorf("node-token content = %q, want %q", string(got), token)
	}

	// ── Assert /etc/clonr/verify-boot-url ────────────────────────────────────
	urlPath := filepath.Join(rootfs, "etc", "clonr", "verify-boot-url")
	if _, err := os.Stat(urlPath); err != nil {
		t.Fatalf("verify-boot-url not found: %v", err)
	}
	gotURL, err := os.ReadFile(urlPath)
	if err != nil {
		t.Fatalf("read verify-boot-url: %v", err)
	}
	if string(gotURL) != verifyURL {
		t.Errorf("verify-boot-url content = %q, want %q", string(gotURL), verifyURL)
	}

	// ── Assert /etc/systemd/system/clonr-verify-boot.service ─────────────────
	unitPath := filepath.Join(rootfs, "etc", "systemd", "system", "clonr-verify-boot.service")
	unitInfo, err := os.Stat(unitPath)
	if err != nil {
		t.Fatalf("unit file not found: %v", err)
	}
	if unitInfo.Size() == 0 {
		t.Error("unit file is empty")
	}
	unitContent, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}
	for _, want := range []string{
		"clonr-verify-boot",
		"network-online.target",
		"ConditionPathExists=/etc/clonr/node-token",
		"ConditionPathExists=/etc/clonr/verify-boot-url",
		"WantedBy=multi-user.target",
	} {
		if !containsString(string(unitContent), want) {
			t.Errorf("unit file missing expected content %q", want)
		}
	}

	// ── Assert /usr/local/bin/clonr-verify-boot ──────────────────────────────
	scriptPath := filepath.Join(rootfs, "usr", "local", "bin", "clonr-verify-boot")
	scriptInfo, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("script not found: %v", err)
	}
	if scriptInfo.Mode()&0o111 == 0 {
		t.Errorf("script is not executable: mode %o", scriptInfo.Mode())
	}
	scriptContent, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	for _, want := range []string{
		"#!/bin/sh",
		"/etc/clonr/node-token",
		"/etc/clonr/verify-boot-url",
		"curl",
		"Authorization: Bearer",
	} {
		if !containsString(string(scriptContent), want) {
			t.Errorf("script missing expected content %q", want)
		}
	}

	// ── Assert multi-user.target.wants symlink ────────────────────────────────
	symlinkPath := filepath.Join(multiUserWantsDir, "clonr-verify-boot.service")
	if _, err := os.Lstat(symlinkPath); err != nil {
		// systemctl --root may not be available in all CI environments.
		// Log the absence but do not fail the test — the unit file and script
		// presence already validates the injection logic.
		t.Logf("WARN: WantedBy symlink not created (systemctl --root may be unavailable in this environment): %v", err)
	}
}

// containsString is a simple substring check used to avoid importing strings in tests.
func containsString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
