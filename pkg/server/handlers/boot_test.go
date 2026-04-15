package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sqoia-dev/clonr/pkg/bootassets"
)

// TestServeIPXEEFI_EmbeddedBinary verifies that GET /api/v1/boot/ipxe.efi
// returns 200, Content-Type: application/efi, and the exact embedded binary
// without requiring any on-disk file in TFTPDir.
func TestServeIPXEEFI_EmbeddedBinary(t *testing.T) {
	h := &BootHandler{
		TFTPDir:   "/nonexistent/tftp", // must NOT be read — binary is embedded
		ServerURL: "http://192.168.1.151:8080",
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/boot/ipxe.efi", nil)
	w := httptest.NewRecorder()

	h.ServeIPXEEFI(w, req)

	resp := w.Result()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ServeIPXEEFI: got status %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "application/efi" {
		t.Errorf("ServeIPXEEFI: Content-Type = %q, want %q", ct, "application/efi")
	}

	if resp.ContentLength != int64(len(bootassets.IPXEEFI)) {
		t.Errorf("ServeIPXEEFI: Content-Length = %d, want %d", resp.ContentLength, len(bootassets.IPXEEFI))
	}

	body := w.Body.Bytes()
	if len(body) != len(bootassets.IPXEEFI) {
		t.Errorf("ServeIPXEEFI: body length = %d, want %d", len(body), len(bootassets.IPXEEFI))
	}
	for i := range body {
		if body[i] != bootassets.IPXEEFI[i] {
			t.Errorf("ServeIPXEEFI: body mismatch at byte %d (got 0x%02x, want 0x%02x)", i, body[i], bootassets.IPXEEFI[i])
			break
		}
	}
}
