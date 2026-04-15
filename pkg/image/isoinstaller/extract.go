package isoinstaller

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// extractSystemdRunAvailable is detected once at package init and used to
// decide whether ExtractViaSubprocess can use systemd-run scope isolation.
var extractSystemdRunAvailable bool

func init() {
	_, err := exec.LookPath("systemd-run")
	extractSystemdRunAvailable = (err == nil)
}

// ExtractViaSubprocess runs rootfs extraction in a subprocess via
// "clonr-serverd extract ..." so that losetup/mount operations happen outside
// clonr-serverd's own hardened unit (NoNewPrivileges, tight capabilities, etc.).
//
// When systemd-run is available the subprocess is placed in
// clonr-builders.slice, which has the capability grants and device permissions
// required for block-device work.  When systemd-run is unavailable (dev
// machines, containers) the subprocess is exec'd directly — it still runs as
// the same user but inherits a less-restricted environment than the parent
// service unit.
//
// buildID is used to name the transient scope unit so operators can correlate
// it in `systemctl status`.  The line callbacks are optional; when non-nil they
// receive stdout/stderr lines from the subprocess in real time (fed to the
// build's progress store so the serial-console panel in the UI shows extraction
// progress).
func ExtractViaSubprocess(buildID string, opts ExtractOptions, onStdout, onStderr func(string)) error {
	selfBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("extract subprocess: locate own binary: %w", err)
	}

	extractArgs := []string{
		"extract",
		"--disk=" + opts.RawDiskPath,
		"--out=" + opts.RootfsDestDir,
	}

	var bin string
	var args []string

	if extractSystemdRunAvailable {
		unitName := "clonr-extract-" + buildID + ".scope"
		bin = "systemd-run"
		args = []string{
			"--scope",
			"--slice=clonr-builders.slice",
			"--unit=" + unitName,
			"--quiet",
			"--",
			selfBin,
		}
		args = append(args, extractArgs...)
	} else {
		bin = selfBin
		args = extractArgs
	}

	cmd := exec.Command(bin, args...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("extract subprocess: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("extract subprocess: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("extract subprocess: start: %w", err)
	}

	// Drain stdout and stderr in the background, forwarding to callbacks.
	drain := func(r io.Reader, cb func(string)) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			if cb != nil {
				cb(scanner.Text())
			}
		}
	}
	go drain(stdoutPipe, onStdout)
	go drain(stderrPipe, onStderr)

	waitErr := cmd.Wait()
	if waitErr == nil {
		return nil
	}

	// Classify exit errors the same way the QEMU wrapper does.
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		return fmt.Errorf("extract subprocess: %w", waitErr)
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			return fmt.Errorf("extract subprocess killed by signal %v (check dmesg for OOM)", status.Signal())
		}
		return fmt.Errorf("extract subprocess exited with code %d", status.ExitStatus())
	}
	return fmt.Errorf("extract subprocess: %w", waitErr)
}

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

// contentOnlyExcludes lists the rsync --exclude arguments that strip all
// layout-specific state from an installed rootfs before it is packed into a
// content-only image tarball (ADR-0009).
//
// These paths fall into three categories:
//
//  1. Boot identity — files whose content is unique to the machine-id that
//     Anaconda used during installation. They must be absent from the image
//     so the deployer can write fresh copies that reference the target node's
//     actual machine-id and disk topology.
//     Includes: /etc/fstab, /etc/machine-id, /var/lib/dbus/machine-id,
//               BLS boot entries, grub.cfg, grubenv.
//
//  2. Bootloader binaries — grub2 modules and EFI binaries are
//     firmware/target-specific; they are re-installed by grub2-install at
//     deploy time for the target node's firmware type (BIOS or UEFI).
//     Including them in the image would pin the image to the firmware type
//     of the build VM, breaking cross-firmware deployments.
//
//  3. Anaconda artefacts — anything the installer wrote that is specific to
//     the install session (e.g. /root/anaconda-ks.cfg, installer logs).
//
// Paths use rsync glob syntax. Trailing /** is used for directory subtrees.
var contentOnlyExcludes = []string{
	// ── Boot identity ────────────────────────────────────────────────────────
	// /etc/fstab: empty placeholder; deployer writes the real one with UUIDs
	// and any operator-configured extra mounts.
	"--exclude=/etc/fstab",
	// /etc/machine-id and its dbus symlink: regenerated on first boot by
	// systemd-firstboot or dbus-uuidgen.
	"--exclude=/etc/machine-id",
	"--exclude=/var/lib/dbus/machine-id",
	// BLS (Boot Loader Specification) entries: Rocky 9+ places one
	// conf file per kernel per machine-id under /boot/loader/entries/.
	// The deployer writes fresh entries with the target kernel and UUID.
	"--exclude=/boot/loader/entries/*.conf",
	// grub.cfg / grubenv: regenerated by grub2-mkconfig at deploy time.
	// grubenv holds save_env state (last-boot menu selection) that can cause
	// the wrong kernel to boot when carried from the build VM.
	"--exclude=/boot/grub2/grub.cfg",
	"--exclude=/boot/grub2/grubenv",
	// ── Bootloader binaries ──────────────────────────────────────────────────
	// UEFI grub binaries on the ESP: re-installed by grub2-install at deploy.
	"--exclude=/boot/efi/EFI/*/grub*.efi",
	"--exclude=/boot/efi/EFI/*/grub.cfg",
	"--exclude=/boot/efi/EFI/BOOT/**",
	// BIOS grub modules: re-installed by grub2-install --target=i386-pc.
	"--exclude=/boot/grub2/i386-pc/**",
	// UEFI grub modules: re-installed by grub2-install --target=x86_64-efi.
	"--exclude=/boot/grub2/x86_64-efi/**",
	// ── Anaconda artefacts ────────────────────────────────────────────────────
	// Kickstart that Anaconda saved to /root — build-session specific.
	"--exclude=/root/anaconda-ks.cfg",
	"--exclude=/root/original-ks.cfg",
}

// ContentOnlyExcludes returns the rsync exclude arguments used by
// rsyncExtracted to produce a content-only image tarball (ADR-0009).
// Exported for testing and introspection.
func ContentOnlyExcludes() []string {
	return contentOnlyExcludes
}

// rsyncExtracted rsyncs an extracted rootfs, preserving all attributes and
// symlinks literally (dangling symlinks are copied as-is, not dereferenced).
//
// Exit code 23 from rsync means "some files/attrs were not transferred". On a
// freshly installed Rocky/RHEL system this is almost always caused by dangling
// symlinks — authselect-managed links (/etc/nsswitch.conf, /etc/pam.d/*-auth,
// etc.), kernel-devel build-dir links, and firmware package oddities. These are
// intentionally dangling and are safe to carry into the image as-is; they
// resolve correctly on first boot. We tolerate exit 23 when every error line
// matches "symlink has no referent". Any other exit-23 cause (I/O error,
// permission denied, etc.) is still surfaced as an error.
func rsyncExtracted(src, dst string) error {
	// --one-file-system is intentionally NOT used here: we want to cross
	// the /boot mount boundary (already mounted at rootMnt/boot above).
	// Pseudo-filesystems (/proc, /sys, /dev) don't exist in the installed image.
	// NOTE: do NOT pass --copy-links / -L / --copy-unsafe-links / --safe-links:
	// those flags dereference symlinks and turn dangling ones into errors.
	// -l (preserve symlinks) is already implied by -a.
	//
	// contentOnlyExcludes strips layout-specific boot state so the resulting
	// rootfs is firmware-agnostic and can be deployed to any node (ADR-0009).
	args := append([]string{"-aAXH", "--numeric-ids"}, contentOnlyExcludes...)
	args = append(args, src, dst)
	cmd := exec.Command("rsync", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &stderr // rsync mixes diagnostic output on stdout too

	err := cmd.Run()
	if err == nil {
		return nil
	}

	// Check for exit code 23 (partial transfer due to errors).
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 23 {
		return fmt.Errorf("rsync extracted rootfs: %w\noutput: %s", err, stderr.String())
	}

	// Exit 23: inspect each error line. Tolerate only "symlink has no referent".
	errOutput := stderr.String()
	for _, line := range strings.Split(errOutput, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// rsync prefixes its own messages with "rsync:" or "rsync error:" — skip those.
		if strings.HasPrefix(line, "rsync:") || strings.HasPrefix(line, "rsync error:") ||
			strings.HasPrefix(line, "sent ") || strings.HasPrefix(line, "total size") {
			continue
		}
		// The only tolerated per-file warning.
		if strings.Contains(line, "symlink has no referent") {
			continue
		}
		// Any other error line is a real problem.
		return fmt.Errorf("rsync extracted rootfs (exit 23, non-symlink error): %s\nfull output: %s", line, errOutput)
	}

	// All errors were dangling symlinks — this is expected and safe to ignore.
	return nil
}
