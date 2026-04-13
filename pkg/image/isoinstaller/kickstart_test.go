package isoinstaller

import (
	"strings"
	"testing"
)

// testTemplateData returns a minimal templateData suitable for kickstart generation.
func testTemplateData() templateData {
	return templateData{
		RootPasswordHash: "$6$rounds=4096$test$fakehash",
		DiskSizeGB:       20,
		TargetDisk:       "vda",
	}
}

// TestGenerateKickstart_Firmware_UEFI verifies that firmware=uefi produces an
// ESP (vfat /boot/efi) partition directive and no biosboot directive.
func TestGenerateKickstart_Firmware_UEFI(t *testing.T) {
	opts := BuildOptions{
		Firmware: "uefi",
	}
	cfg, err := generateKickstart(DistroRocky, testTemplateData(), opts, "")
	if err != nil {
		t.Fatalf("generateKickstart error: %v", err)
	}

	ks := cfg.KickstartContent

	if !strings.Contains(ks, "vfat") {
		t.Errorf("expected vfat ESP partition for uefi, got kickstart:\n%s", ks)
	}
	if !strings.Contains(ks, "/boot/efi") {
		t.Errorf("expected /boot/efi mount point for uefi, got kickstart:\n%s", ks)
	}
	if strings.Contains(ks, "biosboot") {
		t.Errorf("unexpected biosboot directive in uefi kickstart:\n%s", ks)
	}
	if !strings.Contains(ks, "Firmware: uefi") {
		t.Errorf("expected 'Firmware: uefi' header comment, got:\n%s", ks)
	}
}

// TestGenerateKickstart_Firmware_BIOS verifies that firmware=bios produces a
// biosboot partition directive and no vfat/ESP directive.
func TestGenerateKickstart_Firmware_BIOS(t *testing.T) {
	opts := BuildOptions{
		Firmware: "bios",
	}
	cfg, err := generateKickstart(DistroRocky, testTemplateData(), opts, "")
	if err != nil {
		t.Fatalf("generateKickstart error: %v", err)
	}

	ks := cfg.KickstartContent

	if !strings.Contains(ks, "biosboot") {
		t.Errorf("expected biosboot partition for bios firmware, got kickstart:\n%s", ks)
	}
	if strings.Contains(ks, "/boot/efi") {
		t.Errorf("unexpected /boot/efi directive in bios kickstart:\n%s", ks)
	}
	if strings.Contains(ks, "vfat") {
		t.Errorf("unexpected vfat (ESP) directive in bios kickstart:\n%s", ks)
	}
	if !strings.Contains(ks, "Firmware: bios") {
		t.Errorf("expected 'Firmware: bios' header comment, got:\n%s", ks)
	}
}

// TestGenerateKickstart_Firmware_Empty verifies that an empty firmware field
// defaults to uefi (backward-compatible behavior).
func TestGenerateKickstart_Firmware_Empty(t *testing.T) {
	opts := BuildOptions{
		Firmware: "",
	}
	cfg, err := generateKickstart(DistroRocky, testTemplateData(), opts, "")
	if err != nil {
		t.Fatalf("generateKickstart error: %v", err)
	}

	ks := cfg.KickstartContent

	// Empty firmware must default to uefi.
	if !strings.Contains(ks, "vfat") {
		t.Errorf("empty firmware should default to uefi (vfat ESP), got kickstart:\n%s", ks)
	}
	if strings.Contains(ks, "biosboot") {
		t.Errorf("unexpected biosboot in default (empty firmware) kickstart:\n%s", ks)
	}
}

// TestGenerateKickstart_Firmware_Invalid verifies that an invalid firmware value
// is silently normalized to uefi (safe default, not an error).
func TestGenerateKickstart_Firmware_Invalid(t *testing.T) {
	opts := BuildOptions{
		Firmware: "legacy", // not a recognized value
	}
	cfg, err := generateKickstart(DistroRocky, testTemplateData(), opts, "")
	if err != nil {
		t.Fatalf("generateKickstart error: %v", err)
	}

	ks := cfg.KickstartContent
	if strings.Contains(ks, "biosboot") {
		t.Errorf("invalid firmware value should default to uefi (no biosboot), got kickstart:\n%s", ks)
	}
	if !strings.Contains(ks, "vfat") {
		t.Errorf("invalid firmware value should default to uefi (vfat ESP), got kickstart:\n%s", ks)
	}
}

// TestGenerateKickstart_CustomKickstart verifies that a custom kickstart bypasses
// template rendering entirely (firmware field is ignored).
func TestGenerateKickstart_CustomKickstart(t *testing.T) {
	custom := "# my custom kickstart\nreboot\n"
	opts := BuildOptions{Firmware: "bios"}
	cfg, err := generateKickstart(DistroRocky, testTemplateData(), opts, custom)
	if err != nil {
		t.Fatalf("generateKickstart with custom error: %v", err)
	}
	if cfg.KickstartContent != custom {
		t.Errorf("expected custom kickstart to pass through unchanged, got:\n%s", cfg.KickstartContent)
	}
}
