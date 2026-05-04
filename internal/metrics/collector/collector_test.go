package collector

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectMemory(t *testing.T) {
	c := &Collector{
		procMeminfo:  "testdata/meminfo",
		procMounts:   "testdata/mounts",
		procPressure: "testdata/pressure",
		prevDiskIO:   make(map[string]diskIO),
	}

	if err := os.MkdirAll("testdata", 0755); err != nil {
		t.Fatal(err)
	}
	// Write a minimal /proc/meminfo fixture.
	meminfoContent := `MemTotal:       16384 kB
MemFree:         4096 kB
MemAvailable:    8192 kB
Buffers:          512 kB
Cached:          2048 kB
SReclaimable:     256 kB
`
	if err := os.WriteFile("testdata/meminfo", []byte(meminfoContent), 0644); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll("testdata")

	// Write a minimal /proc/mounts fixture.
	mountsContent := "/ / ext4 rw 0 0\n"
	if err := os.WriteFile("testdata/mounts", []byte(mountsContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Write minimal PSI fixtures.
	if err := os.MkdirAll("testdata/pressure", 0755); err != nil {
		t.Fatal(err)
	}
	psiContent := "some avg10=1.50 avg60=2.00 avg300=3.00 total=12345\nfull avg10=0.50 avg60=0.75 avg300=1.00 total=5678\n"
	for _, res := range []string{"memory", "io", "cpu"} {
		if err := os.WriteFile(filepath.Join("testdata/pressure", res), []byte(psiContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	samples := c.collectMemory(time.Now().UTC())
	if len(samples) == 0 {
		t.Fatal("expected memory samples, got none")
	}

	// Check total and used_pct are present.
	sensorSet := make(map[string]float64)
	for _, s := range samples {
		sensorSet[s.Sensor] = s.Value
	}
	if _, ok := sensorSet["total"]; !ok {
		t.Error("missing memory.total sample")
	}
	if _, ok := sensorSet["used_pct"]; !ok {
		t.Error("missing memory.used_pct sample")
	}
	if pct := sensorSet["used_pct"]; pct < 0 || pct > 100 {
		t.Errorf("used_pct out of range: %v", pct)
	}
}

func TestCollectPSI(t *testing.T) {
	if err := os.MkdirAll("testdata/pressure", 0755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll("testdata")

	psiContent := "some avg10=5.75 avg60=3.20 avg300=1.10 total=99999\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	for _, res := range []string{"memory", "io", "cpu"} {
		if err := os.WriteFile(filepath.Join("testdata/pressure", res), []byte(psiContent), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := &Collector{
		prevDiskIO:   make(map[string]diskIO),
		procPressure: "testdata/pressure",
	}
	samples := c.collectPSI(time.Now().UTC())
	if len(samples) == 0 {
		t.Fatal("expected PSI samples, got none")
	}
	for _, s := range samples {
		if s.Plugin != "psi" {
			t.Errorf("unexpected plugin: %s", s.Plugin)
		}
		if !strings.Contains(s.Sensor, "avg10") {
			t.Errorf("expected avg10 sensor, got: %s", s.Sensor)
		}
		if s.Value < 0 {
			t.Errorf("PSI value < 0: %v", s.Value)
		}
	}
}

func TestCollectCertExpiry(t *testing.T) {
	// Self-signed cert for testing.  We just confirm the function doesn't crash
	// on a missing file.
	samples := CollectCertExpiry([]string{"/nonexistent/cert.pem"}, time.Now().UTC())
	if len(samples) != 0 {
		t.Errorf("expected 0 samples for missing cert, got %d", len(samples))
	}
}

func TestTouchHeartbeat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "selfmon.heartbeat")

	TouchHeartbeat(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("heartbeat file not created: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Error("heartbeat file is empty")
	}
}

func TestCollectSystemd_NoSystemctl(t *testing.T) {
	// In CI systemctl may not exist; the call must be graceful.
	c := New()
	// Override PATH to ensure systemctl is not found.
	orig := os.Getenv("PATH")
	os.Setenv("PATH", t.TempDir())
	defer os.Setenv("PATH", orig)

	samples := c.collectSystemd(context.Background(), time.Now().UTC())
	// Either no samples (systemctl not found) or valid samples — must not panic.
	for _, s := range samples {
		if s.Plugin != "systemd" {
			t.Errorf("unexpected plugin: %s", s.Plugin)
		}
	}
}

// TestCollectSystemd_Timeout verifies that collectSystemd returns within the
// 5-second deadline even when the underlying systemctl binary hangs indefinitely
// (simulated by pointing PATH at a directory containing a "systemctl" wrapper
// that is just /bin/sleep 30).
//
// The test asserts:
//   - The call completes in well under 6 seconds (not at 30s when sleep would
//     finish naturally).
//   - The return value is nil (zero values — don't propagate the error).
//   - The goroutine is not left blocked after return (checked implicitly by the
//     test timeout).
func TestCollectSystemd_Timeout(t *testing.T) {
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not found; skipping timeout test")
	}

	// Create a temp directory with a fake "systemctl" that delegates to sleep 30.
	fakeDir := t.TempDir()
	fakeSystemctl := filepath.Join(fakeDir, "systemctl")
	script := "#!/bin/sh\nexec " + sleepPath + " 30\n"
	if err := os.WriteFile(fakeSystemctl, []byte(script), 0755); err != nil {
		t.Fatalf("write fake systemctl: %v", err)
	}

	// Prepend fakeDir to PATH so exec.LookPath finds our stub first.
	orig := os.Getenv("PATH")
	os.Setenv("PATH", fakeDir+":"+orig)
	defer os.Setenv("PATH", orig)

	c := New()

	start := time.Now()
	samples := c.collectSystemd(context.Background(), start)
	elapsed := time.Since(start)

	// Must return within 6 seconds (5s timeout + 1s margin).
	if elapsed > 6*time.Second {
		t.Errorf("collectSystemd took %v; expected to return within 6s after timeout", elapsed)
	}

	// On timeout, zero values are returned (nil slice).
	if len(samples) != 0 {
		t.Errorf("expected nil samples on timeout, got %d", len(samples))
	}
}

// TestSelfmonHeartbeat_TouchedBeforeCollect verifies that TouchHeartbeat can be
// called before any collection work, so a hung collector sub-function cannot
// delay the heartbeat past WatchdogSec.
//
// This is a unit test for the TouchHeartbeat helper itself: it must succeed on
// a path that does not yet exist and produce a non-empty timestamp file.
func TestSelfmonHeartbeat_TouchedBeforeCollect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "selfmon.heartbeat")

	// Call TouchHeartbeat simulating the "start of tick" pattern.
	before := time.Now()
	TouchHeartbeat(path)
	after := time.Now()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("heartbeat file not created: %v", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		t.Fatal("heartbeat file is empty")
	}
	// The file should contain a Unix timestamp within the window of the call.
	var ts int64
	if _, err := fmt.Sscanf(content, "%d", &ts); err != nil {
		t.Fatalf("heartbeat content is not a unix timestamp: %q", content)
	}
	if ts < before.Unix() || ts > after.Unix()+1 {
		t.Errorf("heartbeat timestamp %d is outside expected range [%d, %d]",
			ts, before.Unix(), after.Unix()+1)
	}
}
