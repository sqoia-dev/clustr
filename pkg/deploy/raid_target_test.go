package deploy

import (
	"testing"

	"github.com/sqoia-dev/clonr/pkg/api"
)

// TestResolvePartitionDisk verifies that resolvePartitionDisk honours the
// PartitionSpec.Device field and falls back to defaultDisk when Device is empty.
func TestResolvePartitionDisk(t *testing.T) {
	tests := []struct {
		name        string
		defaultDisk string
		spec        api.PartitionSpec
		want        string
	}{
		{
			name:        "no Device field — use defaultDisk",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{},
			want:        "/dev/sda",
		},
		{
			name:        "Device = md0 (bare name) — resolves to /dev/md0",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{Device: "md0"},
			want:        "/dev/md0",
		},
		{
			name:        "Device = /dev/md1 (already absolute) — returned as-is",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{Device: "/dev/md1"},
			want:        "/dev/md1",
		},
		{
			name:        "Device = sdb (non-RAID secondary disk) — resolves to /dev/sdb",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{Device: "sdb"},
			want:        "/dev/sdb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolvePartitionDisk(tc.defaultDisk, tc.spec)
			if got != tc.want {
				t.Errorf("resolvePartitionDisk(%q, Device=%q) = %q, want %q",
					tc.defaultDisk, tc.spec.Device, got, tc.want)
			}
		})
	}
}

// TestResolveFormatTarget verifies that mkfs targets are correct for both
// single-disk and RAID-on-whole-disk layouts.
func TestResolveFormatTarget(t *testing.T) {
	tests := []struct {
		name        string
		defaultDisk string
		spec        api.PartitionSpec
		partNum     int
		want        string
	}{
		{
			name:        "single-disk layout, partition 1",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{},
			partNum:     1,
			want:        "/dev/sda1",
		},
		{
			name:        "single-disk layout, partition 3",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{},
			partNum:     3,
			want:        "/dev/sda3",
		},
		{
			name:        "RAID1 layout — Device=md0, partition 1 → md0p1",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{Device: "md0"},
			partNum:     1,
			want:        "/dev/md0p1",
		},
		{
			name:        "RAID1 layout — Device=md0, partition 2 → md0p2",
			defaultDisk: "/dev/sda",
			spec:        api.PartitionSpec{Device: "md0"},
			partNum:     2,
			want:        "/dev/md0p2",
		},
		{
			name:        "nvme single-disk, partition 2 → nvme0n1p2",
			defaultDisk: "/dev/nvme0n1",
			spec:        api.PartitionSpec{},
			partNum:     2,
			want:        "/dev/nvme0n1p2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveFormatTarget(tc.defaultDisk, tc.spec, tc.partNum)
			if got != tc.want {
				t.Errorf("resolveFormatTarget(%q, Device=%q, num=%d) = %q, want %q",
					tc.defaultDisk, tc.spec.Device, tc.partNum, got, tc.want)
			}
		})
	}
}

// TestPartitionDevices verifies that partitionDevices produces the correct
// device path slice for both single-disk and RAID-on-whole-disk layouts.
func TestPartitionDevices(t *testing.T) {
	t.Run("single-disk BIOS layout", func(t *testing.T) {
		layout := api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Filesystem: "biosboot"}, // sda1
				{Label: "boot", MountPoint: "/boot"},        // sda2
				{Label: "root", MountPoint: "/"},            // sda3
			},
		}
		got := partitionDevices("/dev/sda", layout)
		want := []string{"/dev/sda1", "/dev/sda2", "/dev/sda3"}
		if len(got) != len(want) {
			t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("partDevs[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("RAID1 whole-disk layout — all partitions on md0", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}}},
			Partitions: []api.PartitionSpec{
				{Device: "md0", Label: "biosboot", Filesystem: "biosboot"}, // md0p1
				{Device: "md0", Label: "boot", MountPoint: "/boot"},        // md0p2
				{Device: "md0", Label: "root", MountPoint: "/"},            // md0p3
			},
		}
		got := partitionDevices("/dev/sda", layout)
		want := []string{"/dev/md0p1", "/dev/md0p2", "/dev/md0p3"}
		if len(got) != len(want) {
			t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("partDevs[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("mixed layout — biosboot on sda, rest on md0", func(t *testing.T) {
		// Edge case: first partition is raw biosboot on sda (some exotic configs),
		// remaining partitions on md0.
		layout := api.DiskLayout{
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Filesystem: "biosboot"},            // sda1 (no Device)
				{Device: "md0", Label: "boot", MountPoint: "/boot"},   // md0p1
				{Device: "md0", Label: "root", MountPoint: "/"},       // md0p2
			},
		}
		got := partitionDevices("/dev/sda", layout)
		want := []string{"/dev/sda1", "/dev/md0p1", "/dev/md0p2"}
		if len(got) != len(want) {
			t.Fatalf("len(got)=%d, want %d; got=%v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("partDevs[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

// TestGrubInstallTargets verifies that grubInstallTargets expands RAID member
// lists correctly and falls back to defaultDisk for non-RAID layouts.
func TestGrubInstallTargets(t *testing.T) {
	t.Run("no RAID — returns defaultDisk only", func(t *testing.T) {
		layout := api.DiskLayout{}
		got := grubInstallTargets("/dev/sda", layout)
		if len(got) != 1 || got[0] != "/dev/sda" {
			t.Errorf("got %v, want [/dev/sda]", got)
		}
	})

	t.Run("RAID1 with two named members — returns both disks", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want 2 entries", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected disk %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("RAID1 with absolute member paths", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"/dev/sda", "/dev/sdb"}},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want 2 entries", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected disk %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("RAID with smallest-N selector — falls back to defaultDisk", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"smallest-2"}},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		if len(got) != 1 || got[0] != "/dev/sda" {
			t.Errorf("got %v, want [/dev/sda] for selector-only members", got)
		}
	})

	t.Run("de-duplication across multiple arrays", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}},
				{Name: "md1", Level: "raid1", Members: []string{"sda", "sdb"}}, // same disks, different array
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		if len(got) != 2 {
			t.Errorf("got %v, want exactly 2 unique disks (de-duped)", got)
		}
	})
}
