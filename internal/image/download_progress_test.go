package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func init() {
	// Allow loopback URLs so httptest.NewServer targets pass validatePullURL.
	_ = os.Setenv("CLUSTR_ALLOW_PRIVATE_URLS", "true")
}

// TestDownloadURLWithProgress_KnownContentLength verifies that progress
// callbacks are emitted with the correct (done, total) pair throughout a
// download when Content-Length is provided by the server.
func TestDownloadURLWithProgress_KnownContentLength(t *testing.T) {
	// Create 3 chunks worth of data — large enough that the loop fires at
	// least twice given the 256 KB read buffer, but small enough for a unit test.
	payload := bytes.Repeat([]byte("x"), 512*1024) // 512 KB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		// Write in small chunks so the client loop fires multiple times.
		for i := 0; i < len(payload); i += 64 * 1024 {
			end := i + 64*1024
			if end > len(payload) {
				end = len(payload)
			}
			_, _ = w.Write(payload[i:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	dst, err := os.CreateTemp(t.TempDir(), "dl-test-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(dst.Name())

	type tick struct{ done, total int64 }
	var ticks []tick
	callCount := 0

	err = downloadURLWithProgress(context.Background(), srv.URL, dst, func(done, total int64) {
		ticks = append(ticks, tick{done, total})
		callCount++
	})
	if err != nil {
		t.Fatalf("downloadURLWithProgress: %v", err)
	}

	if callCount == 0 {
		t.Fatal("no progress callbacks emitted")
	}

	// The final callback must report total bytes downloaded.
	last := ticks[len(ticks)-1]
	if last.done != int64(len(payload)) {
		t.Errorf("final done=%d, want %d", last.done, len(payload))
	}
	if last.total != int64(len(payload)) {
		t.Errorf("final total=%d, want %d", last.total, len(payload))
	}

	// Every intermediate callback must have done <= total.
	for i, tk := range ticks {
		if tk.done > tk.total && tk.total >= 0 {
			t.Errorf("tick[%d]: done=%d > total=%d", i, tk.done, tk.total)
		}
	}

	// Progress must be monotonically non-decreasing.
	for i := 1; i < len(ticks); i++ {
		if ticks[i].done < ticks[i-1].done {
			t.Errorf("tick[%d].done=%d < tick[%d].done=%d (non-monotonic)",
				i, ticks[i].done, i-1, ticks[i-1].done)
		}
	}
}

// TestDownloadURLWithProgress_NoContentLength verifies that total is -1 when
// the server omits Content-Length, and that done is still reported correctly.
func TestDownloadURLWithProgress_NoContentLength(t *testing.T) {
	payload := bytes.Repeat([]byte("y"), 128*1024) // 128 KB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately omit Content-Length (streaming / chunked transfer).
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dst, err := os.CreateTemp(t.TempDir(), "dl-test-noct-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(dst.Name())

	var finalDone, finalTotal int64
	err = downloadURLWithProgress(context.Background(), srv.URL, dst, func(done, total int64) {
		finalDone = done
		finalTotal = total
	})
	if err != nil {
		t.Fatalf("downloadURLWithProgress: %v", err)
	}

	if finalDone != int64(len(payload)) {
		t.Errorf("finalDone=%d, want %d", finalDone, len(payload))
	}
	// When Content-Length is absent, total should be -1.
	if finalTotal != -1 {
		t.Errorf("finalTotal=%d, want -1 (no Content-Length)", finalTotal)
	}
}

// TestDownloadURLWithResume_FullDownload verifies that a resume with
// offset=0 behaves identically to a plain download: all bytes are written and
// progress callbacks are emitted.
func TestDownloadURLWithResume_FullDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("z"), 300*1024) // 300 KB

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	dst, err := os.CreateTemp(t.TempDir(), "dl-resume-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(dst.Name())

	var calls []int64
	err = downloadURLWithResume(context.Background(), srv.URL, dst, 0, func(done, _ int64) {
		calls = append(calls, done)
	})
	if err != nil {
		t.Fatalf("downloadURLWithResume: %v", err)
	}

	if len(calls) == 0 {
		t.Fatal("no progress callbacks emitted")
	}

	last := calls[len(calls)-1]
	if last != int64(len(payload)) {
		t.Errorf("final done=%d, want %d", last, len(payload))
	}

	// Verify the file contents match the payload.
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got, err := io.ReadAll(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("file content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

// TestDownloadURLWithResume_ResumeFrom verifies that a 206 Partial Content
// response causes bytes to be appended from the resume offset rather than
// rewriting the file.
func TestDownloadURLWithResume_ResumeFrom(t *testing.T) {
	full := bytes.Repeat([]byte("a"), 200*1024) // 200 KB total
	resumeAt := int64(100 * 1024)               // first 100 KB already written
	remaining := full[resumeAt:]

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHdr := r.Header.Get("Range")
		if rangeHdr == fmt.Sprintf("bytes=%d-", resumeAt) {
			// Server honours the range request.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(remaining)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(remaining)
		} else {
			// Unexpected — full response.
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(full)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(full)
		}
	}))
	defer srv.Close()

	// Pre-write the first half of the file.
	dst, err := os.CreateTemp(t.TempDir(), "dl-resume-partial-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(dst.Name())
	if _, err := dst.Write(full[:resumeAt]); err != nil {
		t.Fatalf("pre-write partial: %v", err)
	}

	var finalDone int64
	err = downloadURLWithResume(context.Background(), srv.URL, dst, resumeAt, func(done, _ int64) {
		finalDone = done
	})
	if err != nil {
		t.Fatalf("downloadURLWithResume resume: %v", err)
	}

	// Final done should equal the full file length (startBytes + added).
	if finalDone != int64(len(full)) {
		t.Errorf("finalDone=%d, want %d", finalDone, len(full))
	}

	// Verify the complete file content.
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	got, err := io.ReadAll(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("file content mismatch after resume: got %d bytes, want %d", len(got), len(full))
	}
}

// TestDownloadURLWithProgress_HTTP4xx verifies that a 4xx response returns
// an error and does not call onProgress.
func TestDownloadURLWithProgress_HTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dst, err := os.CreateTemp(t.TempDir(), "dl-err-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer os.Remove(dst.Name())

	called := false
	err = downloadURLWithProgress(context.Background(), srv.URL, dst, func(_, _ int64) {
		called = true
	})
	if err == nil {
		t.Fatal("expected error from 404 response, got nil")
	}
	if called {
		t.Error("onProgress should not be called on HTTP error")
	}
}
