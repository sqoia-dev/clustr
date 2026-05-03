package handlers

// images_download_timeout_test.go — tests for blob download timeout helper.
//
// downloadFromURL was removed when from-url was wired through Factory.PullImage.
// The blobDownloadTimeout helper is still used by the blob-stream endpoint
// (DownloadBlob), so its env-var parsing is tested here.

import (
	"testing"
	"time"
)

// TestBlobDownloadTimeout_Default verifies the default timeout is 6 hours.
func TestBlobDownloadTimeout_Default(t *testing.T) {
	t.Setenv("CLUSTR_BLOB_DOWNLOAD_TIMEOUT", "")
	got := blobDownloadTimeout()
	if got != 6*time.Hour {
		t.Errorf("blobDownloadTimeout default: got %v, want 6h", got)
	}
}

// TestBlobDownloadTimeout_EnvVar verifies the env var override is respected.
func TestBlobDownloadTimeout_EnvVar(t *testing.T) {
	t.Setenv("CLUSTR_BLOB_DOWNLOAD_TIMEOUT", "2h")
	got := blobDownloadTimeout()
	if got != 2*time.Hour {
		t.Errorf("blobDownloadTimeout env: got %v, want 2h", got)
	}
}

// TestBlobDownloadTimeout_BelowMin verifies that values below the 1-minute hard
// minimum fall back to the default rather than causing instant failure.
func TestBlobDownloadTimeout_BelowMin(t *testing.T) {
	t.Setenv("CLUSTR_BLOB_DOWNLOAD_TIMEOUT", "30s")
	got := blobDownloadTimeout()
	if got != defaultBlobDownloadTimeout {
		t.Errorf("blobDownloadTimeout below-min: got %v, want default %v", got, defaultBlobDownloadTimeout)
	}
}

// TestBlobDownloadTimeout_InvalidDuration verifies that an unparseable env var
// falls back to the default.
func TestBlobDownloadTimeout_InvalidDuration(t *testing.T) {
	t.Setenv("CLUSTR_BLOB_DOWNLOAD_TIMEOUT", "not-a-duration")
	got := blobDownloadTimeout()
	if got != defaultBlobDownloadTimeout {
		t.Errorf("blobDownloadTimeout invalid: got %v, want default %v", got, defaultBlobDownloadTimeout)
	}
}
