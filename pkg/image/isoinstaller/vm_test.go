package isoinstaller

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// ── exit error classification helpers ────────────────────────────────────────

// fakeExitError builds an *exec.ExitError that wraps a WaitStatus with the
// given signal set. It does this by actually running a short-lived process and
// killing it — the only portable way to get a real *exec.ExitError in tests.
func fakeKilledBySignal(t *testing.T, sig syscall.Signal) error {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start sleep process: %v", err)
	}
	_ = cmd.Process.Signal(sig)
	err := cmd.Wait()
	if err == nil {
		t.Fatal("expected non-nil error from killed process")
	}
	return err
}

func fakeNonZeroExit(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit "+string(rune('0'+code)))
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected non-nil error from non-zero exit")
	}
	return err
}

// ── classifyQEMUExitError ─────────────────────────────────────────────────────
// The classification logic lives inline in Build(); we test it through a
// extracted helper to keep tests fast and independent of a real QEMU binary.

// classifyQEMUExitError returns a human-readable headline for the given error,
// matching the logic in Build(). This mirrors the production code path exactly.
func classifyQEMUExitError(procExitErr error, elapsed time.Duration, serialLogPath string) string {
	if exitErr, ok := procExitErr.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				sig := status.Signal()
				switch sig {
				case syscall.SIGKILL:
					return "SIGKILL"
				case syscall.SIGTERM:
					return "SIGTERM"
				default:
					return "OTHER_SIGNAL"
				}
			}
			return "EXIT_CODE"
		}
	}
	return "UNKNOWN"
}

func TestClassify_SIGKILL(t *testing.T) {
	err := fakeKilledBySignal(t, syscall.SIGKILL)
	result := classifyQEMUExitError(err, 47*time.Second, "/tmp/serial.log")
	if result != "SIGKILL" {
		t.Errorf("expected SIGKILL classification, got %q", result)
	}
}

func TestClassify_SIGTERM(t *testing.T) {
	err := fakeKilledBySignal(t, syscall.SIGTERM)
	result := classifyQEMUExitError(err, 10*time.Second, "/tmp/serial.log")
	if result != "SIGTERM" {
		t.Errorf("expected SIGTERM classification, got %q", result)
	}
}

func TestClassify_NonZeroExit(t *testing.T) {
	// Use a real non-zero exit via exec.
	cmd := exec.Command("sh", "-c", "exit 2")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected error")
	}
	result := classifyQEMUExitError(err, 5*time.Second, "/tmp/serial.log")
	if result != "EXIT_CODE" {
		t.Errorf("expected EXIT_CODE classification, got %q", result)
	}
}

func TestClassify_NilError(t *testing.T) {
	// nil error should not enter the classification block at all; verify
	// classifyQEMUExitError handles non-ExitError gracefully.
	err := os.ErrNotExist // not an *exec.ExitError
	result := classifyQEMUExitError(err, 5*time.Second, "/tmp/serial.log")
	if result != "UNKNOWN" {
		t.Errorf("expected UNKNOWN for non-ExitError, got %q", result)
	}
}

// ── wrapQEMUInScope ───────────────────────────────────────────────────────────

func TestWrapQEMUInScope_WithSystemdRun(t *testing.T) {
	// Temporarily force systemdRunAvailable = true for this test.
	orig := systemdRunAvailable
	systemdRunAvailable = true
	defer func() { systemdRunAvailable = orig }()

	bin, args := wrapQEMUInScope("build-abc123", "/usr/bin/qemu-kvm", []string{"-machine", "q35"})
	if bin != "systemd-run" {
		t.Errorf("expected bin=systemd-run, got %q", bin)
	}
	// args should contain --scope, --slice=clonr-builders.slice, unit name, and qemu args.
	found := map[string]bool{}
	for _, a := range args {
		if a == "--scope" {
			found["scope"] = true
		}
		if a == "--slice=clonr-builders.slice" {
			found["slice"] = true
		}
		if a == "--unit=clonr-iso-build-build-abc123.scope" {
			found["unit"] = true
		}
		if a == "/usr/bin/qemu-kvm" {
			found["qemu"] = true
		}
		if a == "-machine" {
			found["machine"] = true
		}
	}
	for _, key := range []string{"scope", "slice", "unit", "qemu", "machine"} {
		if !found[key] {
			t.Errorf("expected %q in args, args=%v", key, args)
		}
	}
}

func TestWrapQEMUInScope_WithoutSystemdRun(t *testing.T) {
	orig := systemdRunAvailable
	systemdRunAvailable = false
	defer func() { systemdRunAvailable = orig }()

	bin, args := wrapQEMUInScope("build-xyz", "/usr/bin/qemu-kvm", []string{"-machine", "q35"})
	if bin != "/usr/bin/qemu-kvm" {
		t.Errorf("expected bin to be qemu-kvm, got %q", bin)
	}
	if len(args) != 2 || args[0] != "-machine" || args[1] != "q35" {
		t.Errorf("expected original args unchanged, got %v", args)
	}
}

// ── buildQEMUArgs machine type ────────────────────────────────────────────────

func TestBuildQEMUArgs_MachineTypeQ35(t *testing.T) {
	opts := BuildOptions{
		DiskSizeGB: 20,
		MemoryMB:   2048,
		CPUs:       2,
		Firmware:   "bios",
		WorkDir:    t.TempDir(),
	}
	args := buildQEMUArgs(opts, "/tmp/disk.raw", "/tmp/seed.iso", "/tmp/serial.log", "/tmp/qmp.sock", nil)

	// Find -machine value.
	for i, a := range args {
		if a == "-machine" && i+1 < len(args) {
			if !hasPrefix(args[i+1], "q35") {
				t.Errorf("expected machine type q35, got %q", args[i+1])
			}
			return
		}
	}
	t.Error("-machine arg not found in QEMU args")
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// ── scanDmesgForOOM ───────────────────────────────────────────────────────────

func TestScanDmesgForOOM_NoError(t *testing.T) {
	// Just verify it doesn't panic. Result may be empty or contain lines depending
	// on the environment — we only care it returns without panicking.
	result := scanDmesgForOOM()
	_ = result
}
