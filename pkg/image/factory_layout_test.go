package image

import (
	"os"
	"path/filepath"
	"testing"
)

// newTestFactory returns a minimal Factory suitable for unit tests.
// It does not wire up a real DB, BuildProgress, or ImageDir — only the
// detectDiskLayout method is exercised.
func newTestFactory() *Factory {
	return &Factory{}
}

// TestDetectDiskLayout_BIOSFirmware verifies that firmware="bios" always emits
// a biosboot layout regardless of whether /boot/efi exists in the rootfs.
// This is the root cause of Bug 2: Rocky10 BIOS images have /boot/efi content
// from the installer even though they target legacy BIOS mode.
func TestDetectDiskLayout_BIOSFirmware(t *testing.T) {
	f := newTestFactory()

	// Create a rootfs with /boot/efi populated (mimics Rocky BIOS install).
	rootfs := t.TempDir()
	efiDir := filepath.Join(rootfs, "boot", "efi", "EFI")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatalf("create /boot/efi: %v", err)
	}
	if err := os.WriteFile(filepath.Join(efiDir, "shimx64.efi"), []byte("fake"), 0o644); err != nil {
		t.Fatalf("write shimx64.efi: %v", err)
	}

	layout := f.detectDiskLayout(rootfs, "bios")

	// Must use i386-pc bootloader — not x86_64-efi.
	if layout.Bootloader.Target != "i386-pc" {
		t.Errorf("firmware=bios: expected bootloader.target=i386-pc, got %q", layout.Bootloader.Target)
	}

	// Must have a biosboot partition, not an ESP.
	hasBiosboot := false
	hasESP := false
	for _, p := range layout.Partitions {
		for _, flag := range p.Flags {
			if flag == "bios_grub" || flag == "biosboot" {
				hasBiosboot = true
			}
		}
		if p.Filesystem == "biosboot" {
			hasBiosboot = true
		}
		if p.MountPoint == "/boot/efi" || p.Filesystem == "vfat" {
			hasESP = true
		}
	}
	if !hasBiosboot {
		t.Errorf("firmware=bios: expected biosboot partition, got none\npartitions: %+v", layout.Partitions)
	}
	if hasESP {
		t.Errorf("firmware=bios: unexpected ESP/vfat partition in BIOS layout")
	}
}

// TestDetectDiskLayout_UEFIFirmware verifies that firmware="uefi" always emits
// an ESP layout.
func TestDetectDiskLayout_UEFIFirmware(t *testing.T) {
	f := newTestFactory()
	rootfs := t.TempDir() // no /boot/efi content

	layout := f.detectDiskLayout(rootfs, "uefi")

	if layout.Bootloader.Target != "x86_64-efi" {
		t.Errorf("firmware=uefi: expected bootloader.target=x86_64-efi, got %q", layout.Bootloader.Target)
	}

	hasESP := false
	for _, p := range layout.Partitions {
		if p.MountPoint == "/boot/efi" {
			hasESP = true
		}
	}
	if !hasESP {
		t.Errorf("firmware=uefi: expected /boot/efi ESP partition, got none\npartitions: %+v", layout.Partitions)
	}
}

// TestDetectDiskLayout_Heuristic_UEFI verifies that the rootfs heuristic
// (empty firmware string) correctly identifies a UEFI install from /boot/efi.
func TestDetectDiskLayout_Heuristic_UEFI(t *testing.T) {
	f := newTestFactory()

	rootfs := t.TempDir()
	efiDir := filepath.Join(rootfs, "boot", "efi", "EFI")
	if err := os.MkdirAll(efiDir, 0o755); err != nil {
		t.Fatalf("create /boot/efi: %v", err)
	}
	_ = os.WriteFile(filepath.Join(efiDir, "shimx64.efi"), []byte("fake"), 0o644)

	layout := f.detectDiskLayout(rootfs, "")

	if layout.Bootloader.Target != "x86_64-efi" {
		t.Errorf("heuristic UEFI: expected x86_64-efi, got %q", layout.Bootloader.Target)
	}
}

// TestDetectDiskLayout_Heuristic_BIOS verifies that the rootfs heuristic
// falls back to BIOS when /boot/efi is absent or empty.
func TestDetectDiskLayout_Heuristic_BIOS(t *testing.T) {
	f := newTestFactory()
	rootfs := t.TempDir() // no /boot/efi

	layout := f.detectDiskLayout(rootfs, "")

	if layout.Bootloader.Target != "i386-pc" {
		t.Errorf("heuristic BIOS: expected i386-pc, got %q", layout.Bootloader.Target)
	}
}
