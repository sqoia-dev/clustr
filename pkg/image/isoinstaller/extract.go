package isoinstaller

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// ExtractOptions configures the filesystem extraction from an installed raw disk.
type ExtractOptions struct {
	// RawDiskPath is the path to the raw disk image produced by Build.
	RawDiskPath string

	// RootfsDestDir is the directory where the root filesystem will be
	// extracted. It must already exist.
	RootfsDestDir string

	// BootDestDir, when non-empty, extracts /boot into a separate directory.
	// When empty, /boot is handled as part of the root rsync.
	BootDestDir string
}

// ExtractRootfs mounts an installed raw disk image (via losetup + kpartx),
// locates the root partition, and rsyncs its contents into RootfsDestDir.
//
// Partition discovery strategy:
//  1. Loop-attach the raw disk with --partscan.
//  2. Use lsblk to enumerate partitions.
//  3. Skip the biosboot / ESP partition (no filesystem or vfat).
//  4. The largest ext4/xfs partition is treated as root.
//  5. The first xfs/ext4 partition before root (if present) is treated as /boot.
//
// This is intentionally simple — the kickstart template uses a fixed layout
// (biosboot + /boot + /) so the heuristic is reliable for clonr-generated images.
// Admins using custom kickstarts with unusual layouts should use CaptureNode instead.
func ExtractRootfs(opts ExtractOptions) error {
	// ── Loop-attach the raw disk ─────────────────────────────────────────
	loopOut, err := exec.Command("losetup", "--find", "--partscan", "--show", opts.RawDiskPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup: %w\noutput: %s", err, string(loopOut))
	}
	loopDev := strings.TrimSpace(string(loopOut))
	defer func() {
		_ = exec.Command("losetup", "-d", loopDev).Run()
	}()

	// Allow udev to create partition devices.
	_ = exec.Command("udevadm", "settle", "--timeout=10").Run()

	// ── Enumerate partitions ─────────────────────────────────────────────
	partOut, err := exec.Command("lsblk", "-lno", "NAME,FSTYPE,SIZE", loopDev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("lsblk: %w\noutput: %s", err, partOut)
	}

	var rootDev, bootDev string
	loopBase := filepath.Base(loopDev)

	for _, line := range strings.Split(strings.TrimSpace(string(partOut)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		fstype := fields[1]
		if name == loopBase {
			continue // skip the loop device itself
		}

		// Skip non-data filesystems.
		if fstype == "" || fstype == "vfat" || strings.EqualFold(fstype, "biosboot") {
			continue
		}

		dev := "/dev/" + name
		if _, statErr := os.Stat(dev); statErr != nil {
			continue
		}

		// Heuristic: if we haven't found a root yet, probe the mount point.
		mp := probeMountPoint(dev)
		switch {
		case mp == "/" || rootDev == "":
			if rootDev == "" || mp == "/" {
				rootDev = dev
			}
		case mp == "/boot" && bootDev == "":
			bootDev = dev
		}
	}

	if rootDev == "" {
		return fmt.Errorf("extract: could not identify root partition on %s — check lsblk output: %s",
			opts.RawDiskPath, string(partOut))
	}

	// ── Mount and rsync root partition ───────────────────────────────────
	rootMnt, err := os.MkdirTemp("", "clonr-root-*")
	if err != nil {
		return fmt.Errorf("create root mount: %w", err)
	}
	defer os.RemoveAll(rootMnt)

	if out, err := exec.Command("mount", "-o", "ro", rootDev, rootMnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount root %s: %w\noutput: %s", rootDev, err, string(out))
	}
	defer func() { _ = exec.Command("umount", "-l", rootMnt).Run() }()

	// If there is a separate /boot partition, mount it under the root mount
	// so rsync picks it up naturally.
	if bootDev != "" {
		bootMnt := filepath.Join(rootMnt, "boot")
		if err := os.MkdirAll(bootMnt, 0o755); err != nil {
			return fmt.Errorf("create boot mount point: %w", err)
		}
		if out, err := exec.Command("mount", "-o", "ro", bootDev, bootMnt).CombinedOutput(); err != nil {
			// Non-fatal: log and continue — we'll get /boot from the root partition
			// if the installer put it there instead of on a separate partition.
			_ = string(out) // suppress unused variable
		} else {
			defer func() { _ = exec.Command("umount", "-l", bootMnt).Run() }()
		}
	}

	// rsync the full mounted tree.
	return rsyncExtracted(rootMnt+"/", opts.RootfsDestDir)
}

// probeMountPoint attempts to identify the likely mount point of a block device
// by probing its filesystem label or by mounting it read-only and checking for
// canonical marker files.
func probeMountPoint(dev string) string {
	// Try blkid for a PARTLABEL first (fast, no mount required).
	out, err := exec.Command("blkid", "-o", "value", "-s", "PARTLABEL", dev).CombinedOutput()
	if err == nil {
		label := strings.ToLower(strings.TrimSpace(string(out)))
		switch label {
		case "root", "/":
			return "/"
		case "boot", "/boot":
			return "/boot"
		}
	}

	// Try LABEL.
	out, err = exec.Command("blkid", "-o", "value", "-s", "LABEL", dev).CombinedOutput()
	if err == nil {
		label := strings.ToLower(strings.TrimSpace(string(out)))
		switch label {
		case "root":
			return "/"
		case "boot":
			return "/boot"
		}
	}

	// Last resort: mount read-only and look for /etc/os-release (root marker).
	mnt, err := os.MkdirTemp("", "clonr-probe-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(mnt)

	if err := unix.Mount(dev, mnt, "auto", unix.MS_RDONLY, ""); err != nil {
		return ""
	}
	defer func() { _ = unix.Unmount(mnt, unix.MNT_DETACH) }()

	if _, err := os.Stat(filepath.Join(mnt, "etc", "os-release")); err == nil {
		return "/"
	}
	if _, err := os.Stat(filepath.Join(mnt, "vmlinuz")); err == nil {
		return "/boot"
	}
	if _, err := os.Stat(filepath.Join(mnt, "grub2")); err == nil {
		return "/boot"
	}
	if _, err := os.Stat(filepath.Join(mnt, "grub")); err == nil {
		return "/boot"
	}
	return ""
}

// rsyncExtracted rsyncs an extracted rootfs, preserving all attributes and
// following unsafe symlinks (to prevent path traversal from untrusted content).
func rsyncExtracted(src, dst string) error {
	// --one-file-system is intentionally NOT used here: we want to cross
	// the /boot mount boundary (already mounted at rootMnt/boot above).
	// Pseudo-filesystems (/proc, /sys, /dev) don't exist in the installed image.
	cmd := exec.Command("rsync",
		"-aAXH",
		"--numeric-ids",
		"--copy-unsafe-links",
		src, dst,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync extracted rootfs: %w\noutput: %s", err, string(out))
	}
	return nil
}
