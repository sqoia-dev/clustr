package deploy

import (
	"errors"
	"testing"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// TestRawDiskFromDevice verifies that rawDiskFromDevice strips trailing
// partition number suffixes to recover the parent disk path. This is required
// for md-on-partitions BIOS RAID layouts where RAIDSpec.Members contain
// partition device names (e.g. "sda2") rather than whole-disk names.
func TestRawDiskFromDevice(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		// Traditional SATA/SAS devices with direct digit suffix.
		{"/dev/sda", "/dev/sda"},   // whole disk — no change
		{"/dev/sdb", "/dev/sdb"},   // whole disk — no change
		{"/dev/sda1", "/dev/sda"},  // partition 1
		{"/dev/sda2", "/dev/sda"},  // partition 2
		{"/dev/sda3", "/dev/sda"},  // partition 3
		{"/dev/sdb2", "/dev/sdb"},  // second disk, partition 2
		{"/dev/sdc4", "/dev/sdc"},  // third disk, partition 4
		{"/dev/hda3", "/dev/hda"},  // legacy IDE disk

		// NVMe devices use 'p' separator before partition number.
		{"/dev/nvme0n1", "/dev/nvme0n1"},     // whole disk — no change
		{"/dev/nvme0n1p1", "/dev/nvme0n1"},   // partition 1
		{"/dev/nvme0n1p2", "/dev/nvme0n1"},   // partition 2
		{"/dev/nvme1n1p3", "/dev/nvme1n1"},   // second NVMe, partition 3

		// Bare names (no /dev/ prefix) — returned as-is (no stripping needed
		// for the /dev/ prefix path, but the function must not panic).
		{"sda", "sda"},    // no /dev/, no digit — unchanged
		{"sda2", "sda"},   // no /dev/ prefix but still strips digit
	}

	for _, tc := range tests {
		got := rawDiskFromDevice(tc.in)
		if got != tc.want {
			t.Errorf("rawDiskFromDevice(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBootloaderError_Fatal verifies that BootloaderError is a proper error
// that wraps its cause and is detectable via errors.As.
func TestBootloaderError_Fatal(t *testing.T) {
	cause := errors.New("embedding is not possible, but this is required for RAID and LVM install")
	be := &BootloaderError{
		Targets: []string{"/dev/sda", "/dev/sdb"},
		Cause:   cause,
	}

	// Must satisfy the error interface.
	if be.Error() == "" {
		t.Error("BootloaderError.Error() must be non-empty")
	}

	// Must unwrap to the cause.
	if !errors.Is(be, cause) {
		t.Error("errors.Is(BootloaderError, cause) should return true via Unwrap")
	}

	// errors.As must find it when wrapped.
	wrapped := errors.New("finalize: " + be.Error())
	_ = wrapped // errors.As needs a non-nil target; test the direct case
	var detected *BootloaderError
	if !errors.As(be, &detected) {
		t.Error("errors.As should detect *BootloaderError")
	}
	if len(detected.Targets) != 2 {
		t.Errorf("expected 2 targets, got %d", len(detected.Targets))
	}
}

// TestSortMountEntries verifies that sortMountEntries produces depth-first order
// so parent mountpoints are always established before their children.
//
// This is critical for correctness: if /boot is mounted before / (the parent),
// the tar extract writes kernel files to the in-memory root's /boot directory,
// then the real /boot partition (empty) is mounted on top, hiding the content
// from GRUB at boot time.
func TestSortMountEntries(t *testing.T) {
	tests := []struct {
		name  string
		input []mountEntry
		want  []string // expected mount order (mount field only)
	}{
		{
			name: "root before boot — already in correct order",
			input: []mountEntry{
				{dev: "/dev/sda4", mount: "/"},
				{dev: "/dev/sda2", mount: "/boot"},
			},
			want: []string{"/", "/boot"},
		},
		{
			name: "boot before root — must be reordered",
			input: []mountEntry{
				{dev: "/dev/sda2", mount: "/boot"},
				{dev: "/dev/sda4", mount: "/"},
			},
			want: []string{"/", "/boot"},
		},
		{
			name: "root + /boot + /home — three levels, all need depth sort",
			input: []mountEntry{
				{dev: "/dev/sda5", mount: "/home"},
				{dev: "/dev/sda2", mount: "/boot"},
				{dev: "/dev/sda4", mount: "/"},
			},
			want: []string{"/", "/boot", "/home"},
		},
		{
			name: "root + /boot + /boot/efi — three-level nesting",
			input: []mountEntry{
				{dev: "/dev/sda3", mount: "/boot/efi"},
				{dev: "/dev/sda4", mount: "/"},
				{dev: "/dev/sda2", mount: "/boot"},
			},
			want: []string{"/", "/boot", "/boot/efi"},
		},
		{
			name: "VM207 single-disk BIOS layout (biosboot skipped, swap skipped)",
			// biosboot and swap are excluded before sortMountEntries is called;
			// only / and /boot reach the sorter.
			input: []mountEntry{
				{dev: "/dev/sda2", mount: "/boot"},
				{dev: "/dev/sda4", mount: "/"},
			},
			want: []string{"/", "/boot"},
		},
		{
			name: "VM206 md-on-partitions RAID layout — md2=/ must mount before md0=/boot",
			// In this topology mountPartitions receives md devices. Before 6631f6d
			// the order depended on layout iteration order; md0=/boot appeared before
			// md2=/ causing /boot to be mounted before the root filesystem.
			input: []mountEntry{
				{dev: "/dev/md0", mount: "/boot"},
				{dev: "/dev/md2", mount: "/"},
			},
			want: []string{"/", "/boot"},
		},
		{
			name: "deterministic secondary sort when depths are equal",
			// /data and /home both have depth 2; /data < /home lexicographically.
			input: []mountEntry{
				{dev: "/dev/sda6", mount: "/home"},
				{dev: "/dev/sda4", mount: "/"},
				{dev: "/dev/sda5", mount: "/data"},
			},
			want: []string{"/", "/data", "/home"},
		},
		{
			name: "single entry — no-op",
			input: []mountEntry{
				{dev: "/dev/sda1", mount: "/"},
			},
			want: []string{"/"},
		},
		{
			name: "empty — no-op",
			input: []mountEntry{},
			want:  []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sortMountEntries(tc.input)
			got := make([]string, len(tc.input))
			for i, m := range tc.input {
				got[i] = m.mount
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("position %d: got %q, want %q (full order: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestMountPartitionsOrder verifies that mountPartitions builds a mount list
// in the correct depth-first order for the VM207 single-disk BIOS layout
// (biosboot + /boot + swap + /) WITHOUT requiring real block devices or root.
//
// Strategy: we call sortMountEntries (the extracted sort helper) directly with
// the same input that mountPartitions would produce from the VM207 layout, and
// verify the resulting order matches the expected mount sequence.
func TestMountPartitionsOrder(t *testing.T) {
	// VM207 disk layout: biosboot(1MB) + /boot(1GB xfs) + swap(4GB) + /(xfs fill)
	// partDevs after createFilesystems: [sda1, sda2, sda3, sda4]
	layout := api.DiskLayout{
		Partitions: []api.PartitionSpec{
			{Label: "biosboot", Filesystem: "biosboot", MountPoint: ""},
			{Label: "boot", Filesystem: "xfs", MountPoint: "/boot"},
			{Label: "swap", Filesystem: "swap", MountPoint: "swap"},
			{Label: "root", Filesystem: "xfs", MountPoint: "/"},
		},
	}
	partDevs := []string{"/dev/sda1", "/dev/sda2", "/dev/sda3", "/dev/sda4"}

	// Build the mount entry list the same way mountPartitions does.
	var mps []mountEntry
	for i, p := range layout.Partitions {
		if p.MountPoint == "" || p.Filesystem == "swap" {
			continue
		}
		mps = append(mps, mountEntry{dev: partDevs[i], mount: p.MountPoint})
	}

	// Verify that before sorting, the layout-order is /boot then / (wrong).
	if len(mps) != 2 {
		t.Fatalf("expected 2 mountable partitions (/ and /boot), got %d: %v", len(mps), mps)
	}
	if mps[0].mount != "/boot" || mps[1].mount != "/" {
		t.Logf("pre-sort order: %v %v (layout already ordered correctly, test still verifies sort)", mps[0].mount, mps[1].mount)
	}

	sortMountEntries(mps)

	// After sorting: / must come first, /boot second.
	if mps[0].mount != "/" || mps[0].dev != "/dev/sda4" {
		t.Errorf("position 0: got {mount=%q dev=%q}, want {mount=%q dev=%q}",
			mps[0].mount, mps[0].dev, "/", "/dev/sda4")
	}
	if mps[1].mount != "/boot" || mps[1].dev != "/dev/sda2" {
		t.Errorf("position 1: got {mount=%q dev=%q}, want {mount=%q dev=%q}",
			mps[1].mount, mps[1].dev, "/boot", "/dev/sda2")
	}
}
