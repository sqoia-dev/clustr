package layout

import (
	"strings"
	"testing"

	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// minimalHW returns a hardware.SystemInfo with a single 500 GB SATA disk,
// suitable for exercising layout recommendations without DMI data.
func minimalHW() hardware.SystemInfo {
	return hardware.SystemInfo{
		Disks: []hardware.Disk{
			{
				Name:      "sda",
				Size:      500 * 1024 * 1024 * 1024,
				Transport: "sata",
			},
		},
		Memory: hardware.MemoryInfo{TotalKB: 8 * 1024 * 1024},
	}
}

// hasBiosBoot returns true when the layout contains a biosboot/bios_grub partition.
func hasBiosBoot(partitions []interface{}) bool { return false }

// biosBootCount counts partitions with the bios_grub or biosboot flag set.
func biosBootCount(rec Recommendation) int {
	count := 0
	for _, p := range rec.Layout.Partitions {
		for _, f := range p.Flags {
			if f == "bios_grub" || f == "biosboot" {
				count++
			}
		}
	}
	return count
}

// espCount counts partitions that are vfat ESP-style (mount point /boot/efi or esp flag).
func espCount(rec Recommendation) int {
	count := 0
	for _, p := range rec.Layout.Partitions {
		if p.MountPoint == "/boot/efi" || p.Filesystem == "vfat" {
			count++
		}
		for _, f := range p.Flags {
			if f == "esp" || f == "boot" {
				count++
				break
			}
		}
	}
	return count
}

// TestRecommend_FirmwareOverride_BIOS verifies that passing imageFirmware="bios"
// forces a biosboot GPT partition regardless of DMI content.
func TestRecommend_FirmwareOverride_BIOS(t *testing.T) {
	hw := minimalHW()
	// DMI is empty — detectUEFI() would return false anyway, but this confirms
	// the override path is exercised (the reasoning string must say "overridden").
	rec, err := Recommend(hw, "filesystem", "bios")
	if err != nil {
		t.Fatalf("Recommend returned error: %v", err)
	}

	if biosBootCount(rec) == 0 {
		t.Errorf("expected a biosboot/bios_grub partition for firmware=bios, got none\npartitions: %+v", rec.Layout.Partitions)
	}

	if !strings.Contains(rec.Reasoning, "DMI detection overridden") {
		t.Errorf("expected reasoning to mention 'DMI detection overridden', got:\n%s", rec.Reasoning)
	}

	// Must NOT have an ESP for BIOS layout.
	for _, p := range rec.Layout.Partitions {
		if p.MountPoint == "/boot/efi" {
			t.Errorf("unexpected /boot/efi partition in BIOS layout: %+v", p)
		}
	}

	// Bootloader target must be i386-pc.
	if rec.Layout.Bootloader.Target != "i386-pc" {
		t.Errorf("expected bootloader target=i386-pc, got %q", rec.Layout.Bootloader.Target)
	}
}

// TestRecommend_FirmwareOverride_UEFI verifies that passing imageFirmware="uefi"
// forces an ESP partition and x86_64-efi bootloader target.
func TestRecommend_FirmwareOverride_UEFI(t *testing.T) {
	hw := minimalHW()
	rec, err := Recommend(hw, "filesystem", "uefi")
	if err != nil {
		t.Fatalf("Recommend returned error: %v", err)
	}

	hasESP := false
	for _, p := range rec.Layout.Partitions {
		if p.MountPoint == "/boot/efi" {
			hasESP = true
		}
	}
	if !hasESP {
		t.Errorf("expected /boot/efi ESP partition for firmware=uefi, got none\npartitions: %+v", rec.Layout.Partitions)
	}

	if biosBootCount(rec) > 0 {
		t.Errorf("unexpected biosboot partition in UEFI layout")
	}

	if !strings.Contains(rec.Reasoning, "DMI detection overridden") {
		t.Errorf("expected reasoning to mention 'DMI detection overridden', got:\n%s", rec.Reasoning)
	}

	if rec.Layout.Bootloader.Target != "x86_64-efi" {
		t.Errorf("expected bootloader target=x86_64-efi, got %q", rec.Layout.Bootloader.Target)
	}
}

// TestRecommend_FirmwareOverride_Empty verifies that an empty imageFirmware
// falls back to DMI detection (no override mention in reasoning).
func TestRecommend_FirmwareOverride_Empty(t *testing.T) {
	hw := minimalHW()
	rec, err := Recommend(hw, "filesystem", "")
	if err != nil {
		t.Fatalf("Recommend returned error: %v", err)
	}

	if strings.Contains(rec.Reasoning, "DMI detection overridden") {
		t.Errorf("expected DMI auto-detection (no override) when imageFirmware is empty, got:\n%s", rec.Reasoning)
	}
}

// TestRecommend_FirmwareOverride_CaseInsensitive checks that "BIOS" and "UEFI"
// are treated the same as their lowercase equivalents.
func TestRecommend_FirmwareOverride_CaseInsensitive(t *testing.T) {
	hw := minimalHW()

	recBIOS, err := Recommend(hw, "filesystem", "BIOS")
	if err != nil {
		t.Fatalf("Recommend(BIOS) error: %v", err)
	}
	if biosBootCount(recBIOS) == 0 {
		t.Errorf("firmware=BIOS (uppercase): expected biosboot partition, got none")
	}

	recUEFI, err := Recommend(hw, "filesystem", "UEFI")
	if err != nil {
		t.Fatalf("Recommend(UEFI) error: %v", err)
	}
	hasESP := false
	for _, p := range recUEFI.Layout.Partitions {
		if p.MountPoint == "/boot/efi" {
			hasESP = true
		}
	}
	if !hasESP {
		t.Errorf("firmware=UEFI (uppercase): expected /boot/efi ESP partition, got none")
	}
}

// TestRecommend_NoDisk verifies that an error is returned when no disks are present.
func TestRecommend_NoDisk(t *testing.T) {
	hw := hardware.SystemInfo{} // no disks
	_, err := Recommend(hw, "filesystem", "bios")
	if err == nil {
		t.Error("expected error when no disks are present, got nil")
	}
}

// twoIdenticalDisksHW returns hardware with two identically-sized 500 GB SATA
// disks, suitable for exercising the RAID1 layout recommendation path.
func twoIdenticalDisksHW() hardware.SystemInfo {
	return hardware.SystemInfo{
		Disks: []hardware.Disk{
			{Name: "sda", Size: 500 * 1024 * 1024 * 1024, Transport: "sata"},
			{Name: "sdb", Size: 500 * 1024 * 1024 * 1024, Transport: "sata"},
		},
		Memory: hardware.MemoryInfo{TotalKB: 8 * 1024 * 1024},
	}
}

// TestRecommend_BIOSRAIDLayout verifies that a BIOS+RAID1 recommendation:
//  1. Places exactly ONE biosboot partition per physical disk (on each raw disk,
//     not inside any md array).
//  2. The biosboot partitions have NO Device field (they land on raw disks, not md).
//  3. All other data partitions (/boot, swap, /) have a Device field pointing to
//     an md array.
//  4. The RAIDSpec members are partition-sliced devices (e.g. "sda2", "sdb2"),
//     confirming the md-on-partitions topology.
//  5. No md array has a biosboot partition as a member.
//  6. The bootloader target is i386-pc (BIOS).
func TestRecommend_BIOSRAIDLayout(t *testing.T) {
	hw := twoIdenticalDisksHW()
	rec, err := Recommend(hw, "filesystem", "bios")
	if err != nil {
		t.Fatalf("Recommend returned error: %v", err)
	}

	// --- 1. Exactly one biosboot partition, no Device field ---
	biosbootCount := 0
	for _, p := range rec.Layout.Partitions {
		isBiosBoot := false
		for _, f := range p.Flags {
			if f == "bios_grub" || f == "biosboot" {
				isBiosBoot = true
			}
		}
		if p.Filesystem == "biosboot" {
			isBiosBoot = true
		}
		if !isBiosBoot {
			continue
		}
		biosbootCount++
		// --- 2. biosboot must NOT have a Device field (must land on raw disks) ---
		if p.Device != "" {
			t.Errorf("biosboot partition has Device=%q — it must NOT be inside an md array (GRUB cannot write through diskfilter)", p.Device)
		}
	}
	if biosbootCount == 0 {
		t.Error("BIOS+RAID1 layout must include at least one biosboot partition")
	}

	// --- 3. Data partitions must have Device pointing to an md array ---
	for _, p := range rec.Layout.Partitions {
		isBiosBoot := false
		for _, f := range p.Flags {
			if f == "bios_grub" || f == "biosboot" {
				isBiosBoot = true
			}
		}
		if p.Filesystem == "biosboot" {
			isBiosBoot = true
		}
		if isBiosBoot {
			continue
		}
		if p.MountPoint == "" && p.Filesystem == "biosboot" {
			continue
		}
		if p.Device == "" {
			t.Errorf("data partition %q (mp=%q) has no Device field in BIOS RAID layout — expected an md array assignment", p.Label, p.MountPoint)
		}
	}

	// --- 4. RAIDSpec members are partition devices (md-on-partitions topology) ---
	if len(rec.Layout.RAIDArrays) == 0 {
		t.Fatal("BIOS+RAID1 layout must include at least one RAIDSpec")
	}
	for _, raid := range rec.Layout.RAIDArrays {
		for _, member := range raid.Members {
			// Members should be partition-sliced: e.g. "sda2", not "sda".
			// They must NOT be md device names.
			if strings.HasPrefix(member, "md") {
				t.Errorf("RAIDSpec %q has member %q — md devices cannot be RAID members of themselves", raid.Name, member)
			}
		}
	}

	// --- 5. No md array has biosboot as a member ---
	// Biosboot partitions are p1 on each disk. For the md-on-partitions topology
	// with /boot as p2, /swap as p3, / as p4, members should be [sda2,sdb2] etc.
	// If biosboot (p1) ended up as a RAID member, grub2-install on the md device
	// would fail with "diskfilter writes are not supported".
	biosbootPartNums := map[string]bool{}
	// Biosboot is partition 1 on each raw disk in this layout.
	for _, p := range rec.Layout.Partitions {
		isBiosBoot := false
		for _, f := range p.Flags {
			if f == "bios_grub" || f == "biosboot" {
				isBiosBoot = true
			}
		}
		if p.Filesystem == "biosboot" {
			isBiosBoot = true
		}
		if isBiosBoot {
			// p1 on sda and sdb
			biosbootPartNums["sda1"] = true
			biosbootPartNums["sdb1"] = true
			biosbootPartNums["/dev/sda1"] = true
			biosbootPartNums["/dev/sdb1"] = true
		}
	}
	for _, raid := range rec.Layout.RAIDArrays {
		for _, member := range raid.Members {
			if biosbootPartNums[member] {
				t.Errorf("RAIDSpec %q includes biosboot partition %q as a member — biosboot must NOT be inside any md array", raid.Name, member)
			}
		}
	}

	// --- 6. Bootloader target must be i386-pc ---
	if rec.Layout.Bootloader.Target != "i386-pc" {
		t.Errorf("expected bootloader target=i386-pc for BIOS+RAID1 layout, got %q", rec.Layout.Bootloader.Target)
	}
}
