package server_test

// buildprogress_test.go — unit tests for BuildProgressStore (#248).
//
// Verifies:
//   - Get returns cumulative state after progress updates (reconnect snapshot correctness).
//   - Get returns serial ring contents accumulated before the call.
//   - Subscribe after events are fired still receives incremental events.
//   - Two simultaneous subscribers each receive events independently.
//   - Get returns (zero, false) for unknown image IDs.

import (
	"testing"
	"time"

	"github.com/sqoia-dev/clustr/internal/server"
)

// TestBuildProgressStore_GetReflectsCumulativeState is the core regression test
// for bug #248: a new subscriber connecting mid-build must see a snapshot that
// reflects all progress accumulated before the connect, not just the initial state.
func TestBuildProgressStore_GetReflectsCumulativeState(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	h := s.Start("img-001")

	// Simulate mid-download state.
	h.SetPhase("downloading_iso")
	h.SetProgress(500*1024*1024, 2*1024*1024*1024) // 500 MB of 2 GB

	state, ok := s.Get("img-001")
	if !ok {
		t.Fatal("Get returned not-found for in-progress image")
	}
	if state.Phase != "downloading_iso" {
		t.Errorf("phase: got %q, want %q", state.Phase, "downloading_iso")
	}
	if state.BytesDone != 500*1024*1024 {
		t.Errorf("bytes_done: got %d, want %d", state.BytesDone, 500*1024*1024)
	}
	if state.BytesTotal != 2*1024*1024*1024 {
		t.Errorf("bytes_total: got %d, want %d", state.BytesTotal, 2*1024*1024*1024)
	}
}

// TestBuildProgressStore_GetReturnsSerialRingSnapshot verifies that Get captures
// all serial lines added before the call. This is what a reconnecting subscriber
// receives as the initial snapshot payload.
func TestBuildProgressStore_GetReturnsSerialRingSnapshot(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	h := s.Start("img-002")
	h.SetPhase("installing")

	lines := []string{"anaconda started", "fetching packages", "installing kernel"}
	for _, l := range lines {
		h.AddSerialLine(l)
	}

	state, ok := s.Get("img-002")
	if !ok {
		t.Fatal("Get returned not-found")
	}
	if len(state.SerialTail) != len(lines) {
		t.Fatalf("serial_tail length: got %d, want %d", len(state.SerialTail), len(lines))
	}
	for i, want := range lines {
		if state.SerialTail[i] != want {
			t.Errorf("serial_tail[%d]: got %q, want %q", i, state.SerialTail[i], want)
		}
	}
}

// TestBuildProgressStore_GetUnknownImageReturnsFalse verifies the not-found path.
func TestBuildProgressStore_GetUnknownImageReturnsFalse(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	_, ok := s.Get("does-not-exist")
	if ok {
		t.Error("Get returned ok=true for unknown image ID")
	}
}

// TestBuildProgressStore_SubscribeAfterEventsReceivesIncrementalEvents verifies
// that a subscriber who connects AFTER progress has been recorded still receives
// subsequent incremental events. The snapshot path (used by the HTTP handler)
// is covered by Get tests above; this confirms pub/sub still works post-connect.
func TestBuildProgressStore_SubscribeAfterEventsReceivesIncrementalEvents(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	h := s.Start("img-003")
	h.SetPhase("downloading_iso")
	h.SetProgress(100*1024*1024, 2*1024*1024*1024)

	// Subscribe AFTER the above events (the existing events are already past).
	ch, cancel := s.Subscribe()
	defer cancel()

	// Fire a new incremental event AFTER subscribing.
	h.SetProgress(200*1024*1024, 2*1024*1024*1024)

	select {
	case ev := <-ch:
		if ev.ImageID != "img-003" {
			t.Errorf("event ImageID: got %q, want %q", ev.ImageID, "img-003")
		}
		if ev.BytesDone != 200*1024*1024 {
			t.Errorf("event BytesDone: got %d, want %d", ev.BytesDone, 200*1024*1024)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: subscriber did not receive incremental event after Subscribe()")
	}
}

// TestBuildProgressStore_TwoSimultaneousSubscribersReceiveIndependentEvents
// verifies that two subscribers on the same image each get their own copy of
// every event (no single-receive fan-out bug).
func TestBuildProgressStore_TwoSimultaneousSubscribersReceiveIndependentEvents(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	h := s.Start("img-004")

	ch1, cancel1 := s.Subscribe()
	defer cancel1()
	ch2, cancel2 := s.Subscribe()
	defer cancel2()

	// Fire the event after both are subscribed.
	h.SetPhase("creating_disk")

	type result struct {
		sub   int
		phase string
	}
	done := make(chan result, 2)

	go func() {
		select {
		case ev := <-ch1:
			done <- result{1, ev.Phase}
		case <-time.After(500 * time.Millisecond):
			done <- result{1, ""}
		}
	}()
	go func() {
		select {
		case ev := <-ch2:
			done <- result{2, ev.Phase}
		case <-time.After(500 * time.Millisecond):
			done <- result{2, ""}
		}
	}()

	for i := 0; i < 2; i++ {
		r := <-done
		if r.phase != "creating_disk" {
			t.Errorf("subscriber %d: got phase %q, want %q", r.sub, r.phase, "creating_disk")
		}
	}
}

// TestBuildProgressStore_TerminalStateInSnapshot verifies that Get reflects
// the failed phase and error message (terminal state snapshot for reconnect).
func TestBuildProgressStore_TerminalStateInSnapshot(t *testing.T) {
	s := server.NewBuildProgressStore("")
	defer s.Stop()

	h := s.Start("img-005")
	h.SetProgress(200*1024*1024, 2*1024*1024*1024)
	h.Fail("QEMU exited with code 1")

	state, ok := s.Get("img-005")
	if !ok {
		t.Fatal("Get returned not-found for failed build")
	}
	if state.Phase != "failed" {
		t.Errorf("phase: got %q, want %q", state.Phase, "failed")
	}
	if state.ErrorMessage != "QEMU exited with code 1" {
		t.Errorf("error_message: got %q, want %q", state.ErrorMessage, "QEMU exited with code 1")
	}
	// Progress is preserved in terminal state.
	if state.BytesDone != 200*1024*1024 {
		t.Errorf("bytes_done: got %d, want %d", state.BytesDone, 200*1024*1024)
	}
}
