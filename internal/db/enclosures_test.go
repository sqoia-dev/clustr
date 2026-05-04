package db_test

// enclosures_test.go — integration tests for Sprint-31 chassis enclosure persistence.
// Tests run against an in-process SQLite database (openTestDB) so they work both
// locally and in CI without any external dependencies.
//
// TestListEnclosuresByRack_NestedQuery is the regression test for the nested-cursor
// panic (Bug #246): ListEnclosuresByRack previously called ListSlotsByEnclosure
// inside the outer rows.Next() loop, causing SQLite to choke when MaxOpenConns=1.
// The fix materialises the enclosure rows first (pass 1) then fetches slots
// (pass 2) after the outer cursor is closed.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sqoia-dev/clustr/pkg/api"
)

// makeRack is a test helper that returns a minimal api.Rack.
func makeRack(id, name string) api.Rack {
	now := time.Now().UTC().Truncate(time.Second)
	return api.Rack{
		ID:        id,
		Name:      name,
		HeightU:   42,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// makeEnclosure returns a minimal api.Enclosure for the given rack at the given
// bottom U position. typeID must be a key in the canned catalog.
func makeEnclosure(id, rackID, typeID string, rackSlotU int) api.Enclosure {
	now := time.Now().UTC().Truncate(time.Second)
	return api.Enclosure{
		ID:        id,
		RackID:    rackID,
		RackSlotU: rackSlotU,
		// HeightU is derived from the catalog inside CreateEnclosure; leave 0 here
		// so the DB layer populates it from the catalog.
		TypeID:    typeID,
		Label:     "chassis-" + id[:8],
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// makeNodeForTest returns a minimal api.NodeConfig that satisfies FK constraints.
func makeNodeForTest(id, baseImageID, hostname, mac string) api.NodeConfig {
	now := time.Now().UTC().Truncate(time.Second)
	return api.NodeConfig{
		ID:         id,
		Hostname:   hostname,
		FQDN:       hostname + ".hpc.example.com",
		PrimaryMAC: mac,
		Interfaces: []api.InterfaceConfig{
			{MACAddress: mac, Name: "ens3", IPAddress: "10.0.0.1/24"},
		},
		BaseImageID: baseImageID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// TestListEnclosuresByRack_NestedQuery is the regression test for Bug #246.
//
// Before the fix, calling ListEnclosuresByRack on a rack with N >= 1 enclosures
// panicked because ListSlotsByEnclosure (which opens two more queries) was called
// while the outer rows cursor was still open — violating SQLite's single-connection
// serialisation.
//
// The test:
//  1. Creates a rack with 3 enclosures of different types.
//  2. Creates 2 nodes and places each in slot 1 of different enclosures.
//  3. Calls ListEnclosuresByRack and asserts the full result without panic.
//  4. Verifies slot occupancy is populated correctly (occupied + empty).
func TestListEnclosuresByRack_NestedQuery(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	// Set up a base image (required FK for node_configs).
	img := makeImage(uuid.New().String())
	if err := d.CreateBaseImage(ctx, img); err != nil {
		t.Fatalf("create base image: %v", err)
	}

	// Create a rack with 42U.
	rackID := uuid.New().String()
	if err := d.CreateRack(ctx, makeRack(rackID, "rack-01")); err != nil {
		t.Fatalf("create rack: %v", err)
	}

	// Create 3 enclosures in the rack.
	// twin-2u-2slot: 2U, 2 slots — place at U1 and U3 (non-overlapping).
	// halfwidth-1u-2slot: 1U, 2 slots — place at U5.
	encIDs := []string{uuid.New().String(), uuid.New().String(), uuid.New().String()}
	enclosures := []struct {
		typeID    string
		rackSlotU int
	}{
		{"twin-2u-2slot", 1},    // U1–U2
		{"twin-2u-2slot", 3},    // U3–U4
		{"halfwidth-1u-2slot", 5}, // U5
	}
	for i, spec := range enclosures {
		enc := makeEnclosure(encIDs[i], rackID, spec.typeID, spec.rackSlotU)
		if err := d.CreateEnclosure(ctx, enc); err != nil {
			t.Fatalf("create enclosure %d: %v", i, err)
		}
	}

	// Create 2 nodes and place each in slot 1 of the first two enclosures.
	node1ID := uuid.New().String()
	node2ID := uuid.New().String()
	for i, nid := range []string{node1ID, node2ID} {
		mac := "aa:bb:cc:dd:ee:" + string(rune('0'+i))
		hostname := "compute-" + string(rune('a'+i))
		node := makeNodeForTest(nid, img.ID, hostname, mac)
		if err := d.CreateNodeConfig(ctx, node); err != nil {
			t.Fatalf("create node %d: %v", i, err)
		}
		// Place node in slot 1 of enclosure i.
		if err := d.SetSlotOccupancy(ctx, encIDs[i], 1, nid); err != nil {
			t.Fatalf("set slot occupancy node %d: %v", i, err)
		}
	}

	// This is the call that panicked before the fix.
	// Run with -race to catch any concurrent-access issues.
	got, err := d.ListEnclosuresByRack(ctx, rackID)
	if err != nil {
		t.Fatalf("ListEnclosuresByRack: unexpected error: %v", err)
	}

	// Verify we got all 3 enclosures.
	if len(got) != 3 {
		t.Fatalf("expected 3 enclosures, got %d", len(got))
	}

	// Enclosures must be returned in rack_slot_u ASC order.
	expectedSlotU := []int{1, 3, 5}
	for i, enc := range got {
		if enc.RackSlotU != expectedSlotU[i] {
			t.Errorf("enclosure[%d]: rack_slot_u = %d, want %d", i, enc.RackSlotU, expectedSlotU[i])
		}
	}

	// Enclosures 0 and 1 each have 2 slots (twin-2u-2slot); slot 1 is occupied,
	// slot 2 is empty.
	for i := 0; i < 2; i++ {
		enc := got[i]
		if len(enc.Slots) != 2 {
			t.Fatalf("enclosure[%d]: expected 2 slots, got %d", i, len(enc.Slots))
		}
		if enc.Slots[0].SlotIndex != 1 {
			t.Errorf("enclosure[%d] slot[0]: SlotIndex = %d, want 1", i, enc.Slots[0].SlotIndex)
		}
		if enc.Slots[0].NodeID == "" {
			t.Errorf("enclosure[%d] slot[0]: expected occupied slot, got empty", i)
		}
		if enc.Slots[1].SlotIndex != 2 {
			t.Errorf("enclosure[%d] slot[1]: SlotIndex = %d, want 2", i, enc.Slots[1].SlotIndex)
		}
		if enc.Slots[1].NodeID != "" {
			t.Errorf("enclosure[%d] slot[1]: expected empty slot, got %s", i, enc.Slots[1].NodeID)
		}
	}

	// Enclosure 2 is a halfwidth-1u-2slot (2 slots), both empty.
	enc2 := got[2]
	if len(enc2.Slots) != 2 {
		t.Fatalf("enclosure[2]: expected 2 slots, got %d", len(enc2.Slots))
	}
	for _, sl := range enc2.Slots {
		if sl.NodeID != "" {
			t.Errorf("enclosure[2] slot %d: expected empty, got %s", sl.SlotIndex, sl.NodeID)
		}
	}
}

// TestGetEnclosure_WithSlots verifies GetEnclosure also populates slot occupancy
// correctly (it calls ListSlotsByEnclosure but not inside an open cursor, so it
// was not broken — this test locks in the behaviour).
func TestGetEnclosure_WithSlots(t *testing.T) {
	d := openTestDB(t)
	ctx := context.Background()

	img := makeImage(uuid.New().String())
	_ = d.CreateBaseImage(ctx, img)

	rackID := uuid.New().String()
	if err := d.CreateRack(ctx, makeRack(rackID, "rack-02")); err != nil {
		t.Fatalf("create rack: %v", err)
	}

	encID := uuid.New().String()
	if err := d.CreateEnclosure(ctx, makeEnclosure(encID, rackID, "blade-2u-4slot", 1)); err != nil {
		t.Fatalf("create enclosure: %v", err)
	}

	// Place a node in slot 3.
	nodeID := uuid.New().String()
	node := makeNodeForTest(nodeID, img.ID, "blade-compute-01", "cc:dd:ee:ff:00:01")
	if err := d.CreateNodeConfig(ctx, node); err != nil {
		t.Fatalf("create node: %v", err)
	}
	if err := d.SetSlotOccupancy(ctx, encID, 3, nodeID); err != nil {
		t.Fatalf("set slot occupancy: %v", err)
	}

	enc, err := d.GetEnclosure(ctx, encID)
	if err != nil {
		t.Fatalf("GetEnclosure: %v", err)
	}
	// blade-2u-4slot has 4 slots.
	if len(enc.Slots) != 4 {
		t.Fatalf("expected 4 slots, got %d", len(enc.Slots))
	}
	// Slot 3 should be occupied.
	if enc.Slots[2].NodeID != nodeID {
		t.Errorf("slot 3: expected %s, got %q", nodeID, enc.Slots[2].NodeID)
	}
	// Others should be empty.
	for _, sl := range enc.Slots {
		if sl.SlotIndex == 3 {
			continue
		}
		if sl.NodeID != "" {
			t.Errorf("slot %d: expected empty, got %s", sl.SlotIndex, sl.NodeID)
		}
	}
}
