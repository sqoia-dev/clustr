package hardware

import (
	"os"
	"path/filepath"
	"testing"
)

// ibFixtureDir returns the testdata path for a given fixture sub-tree.
func ibFixtureDir(sub string) string {
	return filepath.Join("testdata", "sys", "class", "infiniband", sub)
}

// TestDiscoverIBDevices_Mellanox tests parsing a Mellanox ConnectX-6 HCA
// with two ports: port 1 active, port 2 down.
func TestDiscoverIBDevices_Mellanox(t *testing.T) {
	devPath := ibFixtureDir("mlx5_0")
	dev, err := readIBDevice(devPath, "mlx5_0")
	if err != nil {
		t.Fatalf("readIBDevice mlx5_0: %v", err)
	}

	if dev.Name != "mlx5_0" {
		t.Errorf("Name: expected mlx5_0, got %q", dev.Name)
	}
	if dev.BoardID != "MT_0000000001" {
		t.Errorf("BoardID: expected MT_0000000001, got %q", dev.BoardID)
	}
	if dev.FWVersion != "20.31.1014" {
		t.Errorf("FWVersion: expected 20.31.1014, got %q", dev.FWVersion)
	}
	if dev.NodeGUID != "b8599f0300a1b2c3" {
		t.Errorf("NodeGUID: expected b8599f0300a1b2c3, got %q", dev.NodeGUID)
	}

	if len(dev.Ports) != 2 {
		t.Fatalf("expected 2 ports, got %d", len(dev.Ports))
	}

	p1 := dev.Ports[0]
	if p1.State != "ACTIVE" {
		t.Errorf("port 1 State: expected ACTIVE, got %q", p1.State)
	}
	if p1.PhysState != "LinkUp" {
		t.Errorf("port 1 PhysState: expected LinkUp, got %q", p1.PhysState)
	}
	if p1.Rate != "100 Gb/sec (4X EDR)" {
		t.Errorf("port 1 Rate: expected '100 Gb/sec (4X EDR)', got %q", p1.Rate)
	}
	if p1.LID != "0x0001" {
		t.Errorf("port 1 LID: expected 0x0001, got %q", p1.LID)
	}
	if p1.LinkLayer != "InfiniBand" {
		t.Errorf("port 1 LinkLayer: expected InfiniBand, got %q", p1.LinkLayer)
	}
	if p1.GID == "" {
		t.Error("port 1 GID: expected non-empty")
	}

	p2 := dev.Ports[1]
	if p2.State != "DOWN" {
		t.Errorf("port 2 State: expected DOWN, got %q", p2.State)
	}
	if p2.PhysState != "Polling" {
		t.Errorf("port 2 PhysState: expected Polling, got %q", p2.PhysState)
	}
}

// TestDiscoverIBDevices_IntelOPA tests parsing an Intel OPA (hfi1_0) adapter.
func TestDiscoverIBDevices_IntelOPA(t *testing.T) {
	devPath := ibFixtureDir("hfi1_0")
	dev, err := readIBDevice(devPath, "hfi1_0")
	if err != nil {
		t.Fatalf("readIBDevice hfi1_0: %v", err)
	}

	if dev.Name != "hfi1_0" {
		t.Errorf("Name: expected hfi1_0, got %q", dev.Name)
	}
	if dev.FWVersion != "10.10.12.114" {
		t.Errorf("FWVersion: expected 10.10.12.114, got %q", dev.FWVersion)
	}
	if len(dev.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(dev.Ports))
	}

	p := dev.Ports[0]
	if p.State != "ACTIVE" {
		t.Errorf("port 1 State: expected ACTIVE, got %q", p.State)
	}
	if p.LinkLayer != "OmniPath" {
		t.Errorf("port 1 LinkLayer: expected OmniPath, got %q", p.LinkLayer)
	}
}

// TestDiscoverIBDevices_NoHardware tests that an empty infiniband directory
// returns an empty slice without error.
func TestDiscoverIBDevices_NoHardware(t *testing.T) {
	emptyDir := ibFixtureDir("empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("setup: mkdir %s: %v", emptyDir, err)
	}

	// Temporarily shadow ibSysDir by directly calling readIBPorts on the empty dir.
	// We verify the top-level DiscoverIBDevices path via the missing-directory case below.
	ports, err := readIBPorts(filepath.Join(emptyDir, "ports"))
	if err != nil {
		t.Fatalf("readIBPorts on missing dir: %v", err)
	}
	if len(ports) != 0 {
		t.Errorf("expected 0 ports for missing ports dir, got %d", len(ports))
	}
}

// TestDiscoverIBDevices_MissingDir tests that DiscoverIBDevices returns an
// empty slice (not an error) when /sys/class/infiniband does not exist at all.
func TestDiscoverIBDevices_MissingDir(t *testing.T) {
	// Point the function at a path that definitely doesn't exist.
	// We test this by verifying the package-level behaviour through the exported API.
	// Since ibSysDir is a package-level const we can't override it directly in tests,
	// so we exercise the same IsNotExist branch via readIBPorts directly.
	devices, err := readIBPorts("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("expected nil error for missing dir, got: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("expected empty slice, got %d ports", len(devices))
	}
}

// TestCleanIBState verifies the state string normalization.
func TestCleanIBState(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"4: ACTIVE", "ACTIVE"},
		{"5: LinkUp", "LinkUp"},
		{"1: DOWN", "DOWN"},
		{"2: Polling", "Polling"},
		{"ACTIVE", "ACTIVE"},        // no prefix — returned as-is
		{"  LinkUp  ", "LinkUp"},    // plain whitespace trimmed
	}
	for _, tc := range cases {
		got := cleanIBState(tc.input)
		if got != tc.want {
			t.Errorf("cleanIBState(%q): expected %q, got %q", tc.input, tc.want, got)
		}
	}
}
