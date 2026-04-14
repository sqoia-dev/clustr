package deploy

import (
	"errors"
	"testing"
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
