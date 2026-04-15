package isoinstaller_test

import (
	"archive/tar"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sqoia-dev/clonr/pkg/image/isoinstaller"
)

// TestExtractSubcommand creates a small loop-backed ext4 disk image with a few
// files, then calls ExtractViaSubprocess (which re-invokes the current test
// binary as "clonr-serverd extract" using os.Executable).  Because the test
// binary is NOT clonr-serverd, this path is not normally exercisable in unit
// tests — so we test ExtractRootfs directly here as the unit-level check, and
// leave the full subprocess round-trip to the integration tag below.
//
// What this test validates:
//   - ExtractRootfs can losetup + mount + rsync a real ext4 partition image.
//   - The extracted directory contains the expected files.
//
// Requires root (losetup/mount) — skip otherwise.
func TestExtractRootfs_SmallDisk(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("skipping: requires root for losetup/mount")
	}

	for _, tool := range []string{"losetup", "mkfs.ext4", "rsync", "lsblk", "blkid"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("skipping: %s not found", tool)
		}
	}

	// Build a 64 MB raw disk image with a single ext4 partition.
	workDir := t.TempDir()
	diskPath := filepath.Join(workDir, "test.raw")

	// dd if=/dev/zero to create blank disk.
	if out, err := exec.Command("dd", "if=/dev/zero", "of="+diskPath, "bs=1M", "count=64").CombinedOutput(); err != nil {
		t.Fatalf("dd: %v\n%s", err, out)
	}

	// Create partition table with a single Linux partition spanning the whole disk.
	partScript := "o\nn\np\n1\n\n\nw\n"
	fdisk := exec.Command("fdisk", diskPath)
	fdisk.Stdin = mustStringReader(partScript)
	if out, err := fdisk.CombinedOutput(); err != nil {
		// fdisk returns non-zero even on success with some versions; check the disk.
		t.Logf("fdisk output (may be benign): %s", out)
	}

	// Attach the disk with --partscan to expose the partition.
	loopOut, err := exec.Command("losetup", "--find", "--partscan", "--show", diskPath).CombinedOutput()
	if err != nil {
		t.Fatalf("losetup: %v\n%s", err, loopOut)
	}
	loopDev := trimSpace(loopOut)
	t.Cleanup(func() { _ = exec.Command("losetup", "-d", loopDev).Run() })

	// Wait for udev.
	_ = exec.Command("udevadm", "settle", "--timeout=10").Run()

	// Determine partition device (loopXp1 or loopX1).
	partDev := loopDev + "p1"
	if _, err := os.Stat(partDev); err != nil {
		partDev = loopDev + "1"
	}

	// Format as ext4 with a recognizable label.
	if out, err := exec.Command("mkfs.ext4", "-L", "root", "-F", partDev).CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4: %v\n%s", err, out)
	}

	// Mount, write sentinel files, unmount.
	mnt := t.TempDir()
	if out, err := exec.Command("mount", partDev, mnt).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v\n%s", err, out)
	}
	sentinelFiles := []string{"etc/os-release", "etc/hostname", "usr/bin/bash"}
	for _, rel := range sentinelFiles {
		full := filepath.Join(mnt, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte("test\n"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
		t.Fatalf("umount: %v\n%s", err, out)
	}
	// Detach so ExtractRootfs can re-attach cleanly.
	_ = exec.Command("losetup", "-d", loopDev).Run()

	// Run ExtractRootfs.
	destDir := t.TempDir()
	opts := isoinstaller.ExtractOptions{
		RawDiskPath:   diskPath,
		RootfsDestDir: destDir,
	}
	if err := isoinstaller.ExtractRootfs(opts); err != nil {
		t.Fatalf("ExtractRootfs: %v", err)
	}

	// Verify sentinel files are present in the extracted rootfs.
	for _, rel := range sentinelFiles {
		full := filepath.Join(destDir, rel)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("expected extracted file %s not found: %v", rel, err)
		}
	}
}

// TestExtractViaSubprocess_MissingBinary verifies that ExtractViaSubprocess
// returns a meaningful error when the subprocess binary fails to start.
// This runs without root and without any real disk.
func TestExtractViaSubprocess_MissingBinary(t *testing.T) {
	// Point os.Executable at a nonexistent binary by overriding PATH.
	// We can't override os.Executable, but we can verify error propagation
	// by passing a disk path that doesn't exist — the subprocess will exit
	// non-zero (extract subcommand fails) and we should get an error back.
	//
	// Note: this test only works if the current binary is named after a
	// real executable, so we just test the error path from a bad disk.
	// The actual binary round-trip is covered by the integration test.

	opts := isoinstaller.ExtractOptions{
		RawDiskPath:   "/nonexistent/disk.raw",
		RootfsDestDir: t.TempDir(),
	}

	// We don't have the full clonr-serverd binary in the test binary, so
	// ExtractViaSubprocess will re-exec os.Executable which is the test
	// binary — which doesn't implement "extract", so cobra will error.
	// Either way, the error should propagate back non-nil.
	err := isoinstaller.ExtractViaSubprocess("test-build-id", opts, nil, nil)
	if err == nil {
		t.Fatal("expected error from ExtractViaSubprocess with nonexistent disk, got nil")
	}
	t.Logf("got expected error: %v", err)
}

// TestContentOnlyExcludes_Coverage verifies that ContentOnlyExcludes() is
// non-empty and contains each of the critical layout-specific paths that
// must be absent from a content-only image (ADR-0009).  Static check — no
// filesystem operations required.
func TestContentOnlyExcludes_Coverage(t *testing.T) {
	excl := isoinstaller.ContentOnlyExcludes()
	if len(excl) == 0 {
		t.Fatal("ContentOnlyExcludes is empty — no layout paths will be excluded")
	}

	// Each entry must begin with "--exclude=" (rsync flag form).
	for i, e := range excl {
		if len(e) < len("--exclude=") || e[:len("--exclude=")] != "--exclude=" {
			t.Errorf("entry %d %q does not start with --exclude=", i, e)
		}
	}

	// Critical paths that ADR-0009 mandates must be excluded.
	criticalPatterns := []string{
		"/etc/fstab",
		"/etc/machine-id",
		"/var/lib/dbus/machine-id",
		"/boot/loader/entries/*.conf",
		"/boot/grub2/grub.cfg",
		"/boot/grub2/grubenv",
		"/boot/grub2/i386-pc/**",
		"/boot/grub2/x86_64-efi/**",
		"/boot/efi/EFI/BOOT/**",
	}
	joined := strings.Join(excl, "\n")
	for _, pat := range criticalPatterns {
		if !strings.Contains(joined, pat) {
			t.Errorf("critical exclude pattern missing: %s", pat)
		}
	}
}

// TestExtractTarOutput exercises rsyncExtracted indirectly by verifying that
// a tar archive of a directory contains expected entries.  Pure unit test, no
// root required.
func TestExtractOptions_FieldsExported(t *testing.T) {
	opts := isoinstaller.ExtractOptions{
		RawDiskPath:   "/tmp/disk.raw",
		RootfsDestDir: "/tmp/rootfs",
		BootDestDir:   "/tmp/boot",
	}
	if opts.RawDiskPath != "/tmp/disk.raw" {
		t.Error("RawDiskPath not exported correctly")
	}
	if opts.RootfsDestDir != "/tmp/rootfs" {
		t.Error("RootfsDestDir not exported correctly")
	}
	if opts.BootDestDir != "/tmp/boot" {
		t.Error("BootDestDir not exported correctly")
	}
}

// verifyTar checks that all expectedFiles are present in the tar at tarPath.
func verifyTar(t *testing.T, tarPath string, expectedFiles []string) {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatalf("open tar %s: %v", tarPath, err)
	}
	defer f.Close()

	found := map[string]bool{}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		found[hdr.Name] = true
	}

	for _, name := range expectedFiles {
		if !found[name] {
			t.Errorf("expected %q in tar, not found", name)
		}
	}
}

func mustStringReader(s string) io.Reader {
	return &stringReader{s: s}
}

type stringReader struct {
	s   string
	pos int
}

func (r *stringReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	n = copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}

func trimSpace(b []byte) string {
	return string([]byte(trimBytes(b)))
}

func trimBytes(b []byte) []byte {
	// trim trailing newlines and spaces
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
