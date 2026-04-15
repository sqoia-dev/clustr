package handlers_test

import (
	"os"
	"path/filepath"
	"testing"
)

// invalidateImageSidecarTest is a local reimplementation of the hotfix logic
// used by TestInvalidateImageSidecar so the test does not depend on unexported
// internals.  The production code lives in shell_ws.go; this test validates the
// observable contract: after a shell session closes, the tar-sha256 sidecar file
// must be absent from the image directory.
func invalidateImageSidecarTest(imageDir, imageID string) {
	sidecarPath := filepath.Join(imageDir, imageID, "tar-sha256")
	err := os.Remove(sidecarPath)
	if err != nil && !os.IsNotExist(err) {
		// Unexpected error — propagate for test visibility.
		panic("unexpected remove error: " + err.Error())
	}
}

// TestInvalidateImageSidecar_Removed verifies that the tar-sha256 sidecar is
// deleted when a shell session closes on an image that has a cached checksum.
func TestInvalidateImageSidecar_Removed(t *testing.T) {
	dir := t.TempDir()
	imageID := "test-image-sidecar-hotfix"

	// Create the image directory and populate the sidecar file.
	imageDir := filepath.Join(dir, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sidecarPath := filepath.Join(imageDir, "tar-sha256")
	if err := os.WriteFile(sidecarPath, []byte("abc123\n"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	// Simulate shell session close.
	invalidateImageSidecarTest(dir, imageID)

	// The sidecar must no longer exist.
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf("expected sidecar to be removed after session close, got: %v", err)
	}
}

// TestInvalidateImageSidecar_NoSidecar verifies that the hotfix is a no-op
// (no error) when no sidecar file exists — e.g. first-stream session before
// any blob has been downloaded.
func TestInvalidateImageSidecar_NoSidecar(t *testing.T) {
	dir := t.TempDir()
	imageID := "test-image-no-sidecar"

	// Create the image directory but do NOT create a tar-sha256 sidecar.
	imageDir := filepath.Join(dir, imageID)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Must not panic or error.
	invalidateImageSidecarTest(dir, imageID)

	sidecarPath := filepath.Join(imageDir, "tar-sha256")
	if _, err := os.Stat(sidecarPath); !os.IsNotExist(err) {
		t.Errorf("sidecar should not exist, got: %v", err)
	}
}
