package hardware

import (
	"os"
	"testing"
)

func TestParseMdstat_RAID1Active(t *testing.T) {
	raw, err := os.ReadFile("testdata/mdstat_raid1_active")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	arrays, err := parseMdstat(raw)
	if err != nil {
		t.Fatalf("parseMdstat: %v", err)
	}

	if len(arrays) != 1 {
		t.Fatalf("expected 1 array, got %d", len(arrays))
	}

	arr := arrays[0]
	if arr.Name != "md1" {
		t.Errorf("expected md1, got %q", arr.Name)
	}
	if arr.Path != "/dev/md1" {
		t.Errorf("expected /dev/md1, got %q", arr.Path)
	}
	if arr.Level != "raid1" {
		t.Errorf("expected raid1, got %q", arr.Level)
	}
	if arr.State != "active" {
		t.Errorf("expected active, got %q", arr.State)
	}
	// 1953382400 blocks * 1024 = 2000263577600 bytes
	if arr.Size != 1953382400*1024 {
		t.Errorf("unexpected size: %d", arr.Size)
	}
	if len(arr.Members) != 2 {
		t.Fatalf("expected 2 members, got %d: %v", len(arr.Members), arr.Members)
	}
	if arr.Members[0] != "/dev/sdb1" && arr.Members[0] != "/dev/sda1" {
		t.Errorf("unexpected member[0]: %q", arr.Members[0])
	}
}

func TestParseMdstat_RAID0Stripe(t *testing.T) {
	raw, err := os.ReadFile("testdata/mdstat_raid0_stripe")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	arrays, err := parseMdstat(raw)
	if err != nil {
		t.Fatalf("parseMdstat: %v", err)
	}

	if len(arrays) != 1 {
		t.Fatalf("expected 1 array, got %d", len(arrays))
	}

	arr := arrays[0]
	if arr.Name != "md0" {
		t.Errorf("expected md0, got %q", arr.Name)
	}
	if arr.Level != "raid0" {
		t.Errorf("expected raid0, got %q", arr.Level)
	}
	if len(arr.Members) != 4 {
		t.Fatalf("expected 4 members for raid0 stripe, got %d: %v", len(arr.Members), arr.Members)
	}
	if arr.ChunkKB != 512 {
		t.Errorf("expected 512K chunk, got %d", arr.ChunkKB)
	}
}

func TestParseMdstat_Degraded(t *testing.T) {
	raw, err := os.ReadFile("testdata/mdstat_degraded")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}

	arrays, err := parseMdstat(raw)
	if err != nil {
		t.Fatalf("parseMdstat: %v", err)
	}

	if len(arrays) != 1 {
		t.Fatalf("expected 1 array, got %d", len(arrays))
	}

	arr := arrays[0]
	if arr.Name != "md5" {
		t.Errorf("expected md5, got %q", arr.Name)
	}
	if arr.Level != "raid5" {
		t.Errorf("expected raid5, got %q", arr.Level)
	}
	// State comes from the first token after colon — "active" in mdstat;
	// sysfs enrichment would set it to "degraded" but we're testing raw parse.
	if arr.State != "active" {
		t.Errorf("expected active from raw mdstat parse, got %q", arr.State)
	}
	// Degraded array with a failed member — should still list all 3 devices.
	if len(arr.Members) != 3 {
		t.Fatalf("expected 3 members (including failed), got %d: %v", len(arr.Members), arr.Members)
	}
}

func TestParseMdstat_Empty(t *testing.T) {
	raw := []byte("Personalities : []\nunused devices: <none>\n")
	arrays, err := parseMdstat(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(arrays) != 0 {
		t.Errorf("expected 0 arrays, got %d", len(arrays))
	}
}

func TestNormalizeArrayState(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"active", "active"},
		{"active-idle", "active"},
		{"clean", "active"},
		{"degraded", "degraded"},
		{"resyncing", "rebuilding"},
		{"recovering", "rebuilding"},
		{"reshaping", "rebuilding"},
		{"check", "rebuilding"},
		{"repair", "rebuilding"},
		{"unknown-state", "unknown-state"},
	}
	for _, c := range cases {
		got := normalizeArrayState(c.in)
		if got != c.want {
			t.Errorf("normalizeArrayState(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDiscoverMDArrays_NoMdstat(t *testing.T) {
	// Simulate an environment with no /proc/mdstat (e.g., no md module loaded).
	arrays, err := discoverMDArraysFromSources(
		func() ([]byte, error) {
			return nil, os.ErrNotExist
		},
		func() ([]byte, error) {
			return []byte(`{"blockdevices":[]}`), nil
		},
	)
	if err != nil {
		t.Fatalf("unexpected error when mdstat missing: %v", err)
	}
	if len(arrays) != 0 {
		t.Errorf("expected 0 arrays, got %d", len(arrays))
	}
}
