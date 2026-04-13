package layout

import (
	"fmt"
	"strings"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/hardware"
)

// ValidationResult holds the outcome of validating a DiskLayout against a target disk.
type ValidationResult struct {
	Valid    bool
	Errors   []string
	Warnings []string
}

// Validate checks a DiskLayout for logical correctness and fitness for targetDisk.
// targetDisk may be a zero-value Disk when only logical checks are needed
// (e.g. when no disk has been discovered yet).
//
// Errors prevent the layout from being used; Warnings are advisory.
func Validate(layout api.DiskLayout, targetDisk hardware.Disk) ValidationResult {
	var errs, warns []string

	// ── Partition-level checks ────────────────────────────────────────────────

	if len(layout.Partitions) == 0 {
		errs = append(errs, "layout must define at least one partition")
		return ValidationResult{Valid: false, Errors: errs}
	}

	var (
		hasRoot       bool
		hasESP        bool
		hasBIOSGrub   bool
		hasBoot       bool
		hasSwap       bool
		fixedTotal    int64
		fillCount     int
	)

	// allowedFilesystems is the set of filesystem types supported by the deploy
	// initramfs (mkfs.xfs, mkfs.ext4, mkfs.vfat, mkswap are the only tools
	// present). "biosboot" and "" (none/raw) are also valid for special partitions.
	allowedFilesystems := map[string]bool{
		"xfs":      true,
		"ext4":     true,
		"vfat":     true,
		"fat32":    true, // alias for vfat
		"fat":      true, // alias for vfat
		"swap":     true,
		"biosboot": true,
		"bios_grub": true,
		"":         true, // none / raw / LVM PV — no mkfs run
	}

	for i, p := range layout.Partitions {
		idx := i + 1

		if p.SizeBytes < 0 {
			errs = append(errs, fmt.Sprintf("partition %d (%s): size_bytes must be >= 0 (0 = fill remaining)", idx, p.Label))
		}
		if p.SizeBytes == 0 {
			fillCount++
			if fillCount > 1 {
				errs = append(errs, fmt.Sprintf("partition %d (%s): only one partition may use size_bytes=0 (fill remaining)", idx, p.Label))
			}
		} else {
			fixedTotal += p.SizeBytes
		}

		// Filesystem type validation.
		fs := strings.ToLower(p.Filesystem)
		if !allowedFilesystems[fs] {
			errs = append(errs, fmt.Sprintf(
				"partition %d (%s): unsupported filesystem %q — must be one of: xfs, ext4, vfat, swap, biosboot, or empty (none/raw); btrfs is not available in the deploy initramfs",
				idx, p.Label, p.Filesystem))
		}

		isESPMount := strings.ToLower(p.MountPoint) == "/boot/efi"
		isESPFlag := false
		for _, flag := range p.Flags {
			if flag == "esp" || flag == "boot" {
				isESPFlag = true
			}
		}
		isESPFS := fs == "vfat" || fs == "fat32" || fs == "fat"

		// ESP must be vfat — UEFI firmware can only read FAT filesystems.
		if (isESPMount || isESPFlag) && !isESPFS && fs != "" {
			errs = append(errs, fmt.Sprintf(
				"partition %d (%s): ESP (EFI System Partition) must use filesystem vfat — UEFI firmware cannot read %q",
				idx, p.Label, p.Filesystem))
		}

		// Swap mountpoint must use swap filesystem.
		if strings.ToLower(p.MountPoint) == "swap" && fs != "swap" && fs != "" {
			errs = append(errs, fmt.Sprintf(
				"partition %d (%s): swap partition must use filesystem swap, not %q",
				idx, p.Label, p.Filesystem))
		}
		// swap filesystem must be on a swap-designated partition.
		if fs == "swap" && strings.ToLower(p.MountPoint) != "swap" && p.MountPoint != "" {
			errs = append(errs, fmt.Sprintf(
				"partition %d (%s): filesystem swap is only valid for swap partitions (mountpoint must be 'swap' or empty), got %q",
				idx, p.Label, p.MountPoint))
		}

		switch strings.ToLower(p.MountPoint) {
		case "/":
			hasRoot = true
		case "/boot":
			hasBoot = true
		case "/boot/efi":
			hasESP = true
		case "swap":
			hasSwap = true
		}

		for _, flag := range p.Flags {
			switch flag {
			case "esp", "boot":
				hasESP = true
			case "bios_grub", "biosboot":
				hasBIOSGrub = true
			}
		}

		if isESPFS {
			hasESP = true
		}
		if fs == "biosboot" || fs == "bios_grub" {
			hasBIOSGrub = true
		}
	}

	// Root partition required.
	if !hasRoot {
		errs = append(errs, "layout must include a partition with mountpoint '/' (root)")
	}

	// Boot mode consistency checks.
	isUEFI := isUEFIBootloader(layout.Bootloader)
	if isUEFI {
		if !hasESP {
			errs = append(errs, "UEFI bootloader (x86_64-efi) requires an ESP partition (vfat filesystem, 'esp' flag, or /boot/efi mountpoint)")
		}
	} else {
		// BIOS/GPT requires a biosboot partition for GRUB2.
		if !hasBIOSGrub && layout.Bootloader.Type == "grub2" && layout.Bootloader.Target == "i386-pc" {
			warns = append(warns, "BIOS/GPT layout with grub2 (i386-pc) should include a biosboot partition (1 MiB, bios_grub flag) — without it grub2-install will fail")
		}
	}

	// Advisory warnings.
	if !hasBoot {
		warns = append(warns, "no separate /boot partition — the kernel and initramfs will be placed under /, which is fine but means bootloader upgrades operate on the root filesystem")
	}
	_ = hasSwap // swap is always optional

	// ── Disk size checks ──────────────────────────────────────────────────────

	if targetDisk.Size > 0 && fixedTotal > 0 {
		diskBytes := int64(targetDisk.Size)
		if fixedTotal > diskBytes {
			errs = append(errs, fmt.Sprintf(
				"fixed partition sizes sum to %s but target disk %s is only %s — layout does not fit",
				fmtGB(fixedTotal), targetDisk.Name, fmtGB(diskBytes)))
		} else if fillCount == 0 && fixedTotal < diskBytes {
			// All partitions are fixed-size and together they don't fill the disk — warn.
			warns = append(warns, fmt.Sprintf(
				"%s of %s disk (%s) is unallocated — consider adding a fill-remaining partition",
				fmtGB(diskBytes-fixedTotal), fmtGB(diskBytes), targetDisk.Name))
		}
	}

	// ── Swap size advisory ────────────────────────────────────────────────────

	ramBytes := int64(0) // targetDisk carries no RAM info — this is checked separately

	for _, p := range layout.Partitions {
		if strings.EqualFold(p.MountPoint, "swap") && p.SizeBytes > 0 {
			if p.SizeBytes > 32*gb {
				warns = append(warns, fmt.Sprintf(
					"swap partition is %s — this is unusually large (> 32 GB); verify this is intentional",
					fmtGB(p.SizeBytes)))
			}
			_ = ramBytes // skip 2×RAM check without RAM data
		}
	}

	return ValidationResult{
		Valid:    len(errs) == 0,
		Errors:   errs,
		Warnings: warns,
	}
}

// ValidateWithRAM performs the full validation including RAM-aware swap checks.
// ramKB is MemoryInfo.TotalKB from hardware discovery (0 = unknown, skip check).
func ValidateWithRAM(layout api.DiskLayout, targetDisk hardware.Disk, ramKB uint64) ValidationResult {
	result := Validate(layout, targetDisk)

	if ramKB == 0 {
		return result
	}

	ramBytes := int64(ramKB) * 1024
	for _, p := range layout.Partitions {
		if strings.EqualFold(p.MountPoint, "swap") && p.SizeBytes > 0 {
			if p.SizeBytes > 2*ramBytes {
				result.Warnings = append(result.Warnings, fmt.Sprintf(
					"swap partition (%s) is more than 2× RAM (%s) — this is unusual and may indicate a misconfiguration",
					fmtGB(p.SizeBytes), fmtGB(ramBytes)))
			}
		}
	}

	return result
}

func isUEFIBootloader(bl api.Bootloader) bool {
	return strings.Contains(strings.ToLower(bl.Target), "efi") ||
		strings.Contains(strings.ToLower(bl.Type), "efi")
}
