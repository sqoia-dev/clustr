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

// TestGrubInstallTargets verifies grubInstallTargets topology detection.
//
// BIOS RAID rule: grub2-install must ALWAYS target raw member disks, never the
// md virtual device. GRUB's diskfilter driver is read-only ("diskfilter writes
// are not supported"), so grub2-install /dev/md0 fails regardless of topology.
// This applies to both RAID-on-whole-disk and md-on-partitions layouts.
func TestGrubInstallTargets(t *testing.T) {
	t.Run("no RAID — returns defaultDisk only", func(t *testing.T) {
		layout := api.DiskLayout{}
		got := grubInstallTargets("/dev/sda", layout)
		if len(got) != 1 || got[0] != "/dev/sda" {
			t.Errorf("got %v, want [/dev/sda]", got)
		}
	})

	t.Run("RAID-on-whole-disk: all partitions on md0 — returns raw member disks", func(t *testing.T) {
		// VM206 topology: all 4 partitions have Device="md0". grub2-install must
		// target the raw member disks (/dev/sda, /dev/sdb), not /dev/md0.
		// GRUB's diskfilter driver is read-only; targeting md0 fails with
		// "diskfilter writes are not supported".
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}},
			},
			Partitions: []api.PartitionSpec{
				{Device: "md0", Label: "biosboot", Flags: []string{"bios_grub"}},
				{Device: "md0", Label: "boot", MountPoint: "/boot"},
				{Device: "md0", Label: "swap", MountPoint: "swap"},
				{Device: "md0", Label: "root", MountPoint: "/"},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want raw member disks [/dev/sda /dev/sdb]", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("RAID-on-whole-disk: two md arrays — returns all raw member disks", func(t *testing.T) {
		// Two RAID arrays: all member disks must receive a grub2-install, not the
		// md virtual devices.
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}},
				{Name: "md1", Level: "raid1", Members: []string{"sdc", "sdd"}},
			},
			Partitions: []api.PartitionSpec{
				{Device: "md0", Label: "boot", MountPoint: "/boot"},
				{Device: "md1", Label: "root", MountPoint: "/"},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true, "/dev/sdc": true, "/dev/sdd": true}
		if len(got) != 4 {
			t.Fatalf("got %v, want all 4 raw member disks", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("md-on-partitions: no Device on partitions — returns raw member disks", func(t *testing.T) {
		// Partitions have no Device field (land on raw disks). md array is formed
		// from slices of sda and sdb. Each raw disk needs its own bootloader.
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda", "sdb"}},
			},
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Flags: []string{"bios_grub"}}, // sda1/sdb1 — no Device
				{Label: "boot", MountPoint: "/boot"},               // no Device
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want raw member disks [/dev/sda /dev/sdb]", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("md-on-partitions with absolute member paths", func(t *testing.T) {
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"/dev/sda", "/dev/sdb"}},
			},
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Flags: []string{"bios_grub"}},
				{Label: "boot", MountPoint: "/boot"},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want [/dev/sda /dev/sdb]", got)
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q in grub targets %v", d, got)
			}
		}
	})

	t.Run("md-on-partitions with partition-number suffixed members (VM206/VM207 topology)", func(t *testing.T) {
		// The layout recommender for BIOS+RAID1 generates RAIDSpec members as
		// partition device names (e.g. "sda2", "sdb2") because each md array is
		// assembled from specific partition slices. grubInstallTargets must strip
		// the trailing partition number to recover the raw parent disk so that
		// grub2-install targets /dev/sda and /dev/sdb (not /dev/sda2 and /dev/sdb2).
		// grub2-install on a partition device fails with:
		//   "Attempting to install GRUB to a partition. This is a BAD idea."
		//   "embedding is not possible, but this is required for RAID and LVM install."
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"sda2", "sdb2"}}, // /boot slices
				{Name: "md1", Level: "raid1", Members: []string{"sda3", "sdb3"}}, // swap slices
				{Name: "md2", Level: "raid1", Members: []string{"sda4", "sdb4"}}, // / slices
			},
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Flags: []string{"bios_grub"}}, // sda1/sdb1 — no Device
				{Label: "boot", MountPoint: "/boot", Device: "md0"},
				{Label: "swap", MountPoint: "swap", Device: "md1"},
				{Label: "root", MountPoint: "/", Device: "md2"},
			},
		}
		got := grubInstallTargets("/dev/sda", layout)
		// Must return raw disks /dev/sda and /dev/sdb — NOT the partition devices.
		want := map[string]bool{"/dev/sda": true, "/dev/sdb": true}
		if len(got) != 2 {
			t.Fatalf("got %v (len=%d), want raw member disks [/dev/sda /dev/sdb]", got, len(got))
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q in grub targets %v — expected raw disk, not partition", d, got)
			}
		}
	})

	t.Run("md-on-partitions with NVMe partition members", func(t *testing.T) {
		// NVMe partition devices use the 'p' separator (nvme0n1p2). Ensure
		// rawDiskFromDevice correctly strips it to return nvme0n1.
		layout := api.DiskLayout{
			RAIDArrays: []api.RAIDSpec{
				{Name: "md0", Level: "raid1", Members: []string{"nvme0n1p2", "nvme1n1p2"}},
			},
			Partitions: []api.PartitionSpec{
				{Label: "biosboot", Flags: []string{"bios_grub"}},
				{Label: "root", MountPoint: "/", Device: "md0"},
			},
		}
		got := grubInstallTargets("/dev/nvme0n1", layout)
		want := map[string]bool{"/dev/nvme0n1": true, "/dev/nvme1n1": true}
		if len(got) != 2 {
			t.Fatalf("got %v (len=%d), want [/dev/nvme0n1 /dev/nvme1n1]", got, len(got))
		}
		for _, d := range got {
			if !want[d] {
				t.Errorf("unexpected target %q — expected raw NVMe disk without partition suffix", d)
			}
		}
	})

	t.Run("RAID with smallest-N selector only — falls back to defaultDisk", func(t *testing.T) {
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
}
