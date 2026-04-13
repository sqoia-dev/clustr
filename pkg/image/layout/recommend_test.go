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
