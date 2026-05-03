package collector

import (
	"context"
	"os"
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
