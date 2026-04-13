package isoinstaller

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// BuildOptions configures a QEMU-based OS install run.
type BuildOptions struct {
	// ISOPath is the path to the downloaded installer ISO on the host filesystem.
	ISOPath string

	// Distro is the detected or admin-supplied distribution. Used to select
	// the automated-install config format and to tune the QEMU command line.
	Distro Distro

	// DiskSizeGB is the size in GiB of the blank target disk created for the
	// install. Default: 20.
	DiskSizeGB int

	// MemoryMB is the RAM allocated to the install VM in MiB. Default: 2048.
	MemoryMB int

	// CPUs is the number of virtual CPUs. Default: 2.
	CPUs int

	// Timeout is the maximum wall-clock time allowed for the full install.
	// Default: 30 minutes. Hard-kills the QEMU process on expiry.
	Timeout time.Duration

	// WorkDir is a directory where temporary files (blank disk, seed ISO,
	// serial log, QMP socket) are stored during the build. The caller is
	// responsible for cleaning it up after Build returns.
	WorkDir string

	// SerialLog is where VM serial console output is written in real time.
	// When nil, output is discarded. Pass a zerolog-compatible writer or any
	// io.Writer (e.g. os.Stdout for debugging, a file for production logging).
	SerialLog io.Writer

	// Logger is used for structured log output from the orchestration layer.
	Logger zerolog.Logger

	// CustomKickstart overrides the auto-generated kickstart/autoinstall config
	// with admin-supplied content. Empty = use the generated template.
	CustomKickstart string

	// RoleIDs is the list of HPC role preset IDs to include in the generated
	// kickstart/autoinstall config. Empty = no role packages beyond minimal base.
	// Example: []string{"compute", "gpu-compute"}
	RoleIDs []string

	// InstallUpdates, when true, adds a "dnf update -y" (or "apt upgrade -y")
	// call to the %post section so the resulting image is fully patched at capture
	// time. Adds roughly 5-10 minutes to the build.
	InstallUpdates bool

	// OnPhase, when set, is called each time the VM installer transitions to a
	// new named phase (e.g. "launching_vm", "installing"). Used by the progress
	// subsystem to update the build status panel in the UI.
	OnPhase func(phase string)

	// OnSerialLine, when set, is called for each line read from the QEMU serial
	// console log in near-real-time. Used to stream the VM console to the UI.
	OnSerialLine func(line string)

	// OnStderrLine, when set, is called for each line read from QEMU's own
	// stderr (process-level errors, not guest OS output).
	OnStderrLine func(line string)
}

// BuildResult is returned by a successful Build call.
type BuildResult struct {
	// RawDiskPath is the path to the raw disk image containing the installed OS.
	// The caller (typically Factory.buildISOAsync) extracts the root filesystem
	// from this disk, then discards it.
	RawDiskPath string

	// ElapsedTime is the wall-clock duration of the full install run.
	ElapsedTime time.Duration

	// SerialLogPath is the path to the captured serial console log.
	// Useful for debugging failed installs.
	SerialLogPath string
}

// defaults applied when callers leave fields at zero.
const (
	defaultDiskSizeGB = 20
	defaultMemoryMB   = 2048
	defaultCPUs       = 2
	defaultTimeout    = 30 * time.Minute
)

// Build runs an OS installer ISO inside a temporary QEMU VM, waits for the
// guest to halt (which the kickstart/autoinstall triggers at the end of
// install), and returns the path to the resulting raw disk image.
//
// It is safe to cancel via ctx: the QEMU process will be killed and all
// temporary files in opts.WorkDir will be left for the caller to clean up.
//
// Broad flow:
//
//	1. Apply defaults.
//	2. Generate the automated-install config for the distro.
//	3. Write the config to a seed ISO (genisoimage / xorriso).
//	4. Create a blank raw disk image (qemu-img create).
//	5. Launch QEMU with -no-reboot so it exits cleanly when the guest halts.
//	6. Monitor the QMP socket for guest shutdown events (and fall back to
//	   watching the process exit if QMP is unavailable).
//	7. Enforce the hard Timeout.
//	8. Return the raw disk path on success.
func Build(ctx context.Context, opts BuildOptions) (*BuildResult, error) {
	applyDefaults(&opts)

	log := opts.Logger

	// callPhase is a nil-safe phase callback helper.
	callPhase := func(phase string) {
		if opts.OnPhase != nil {
			opts.OnPhase(phase)
		}
	}

	// ── Generate automated-install config ─────────────────────────────────
	log.Info().
		Str("distro", string(opts.Distro)).
		Str("format", string(opts.Distro.Format())).
		Msg("isoinstaller: generating automated-install config")

	cfg, err := GenerateAutoInstallConfig(opts.Distro, opts, opts.CustomKickstart)
	if err != nil {
		return nil, fmt.Errorf("isoinstaller: generate install config: %w", err)
	}

	// ── Write seed ISO ─────────────────────────────────────────────────────
	callPhase("generating_config")
	seedISOPath := filepath.Join(opts.WorkDir, "seed.iso")
	if err := writeSeedISO(opts.WorkDir, seedISOPath, cfg); err != nil {
		return nil, fmt.Errorf("isoinstaller: write seed ISO: %w", err)
	}
	log.Info().Str("seed_iso", seedISOPath).Msg("isoinstaller: seed ISO written")

	// ── Create blank target disk ───────────────────────────────────────────
	callPhase("creating_disk")
	rawDiskPath := filepath.Join(opts.WorkDir, "disk.raw")
	diskSize := fmt.Sprintf("%dG", opts.DiskSizeGB)
	log.Info().Str("disk", rawDiskPath).Str("size", diskSize).Msg("isoinstaller: creating blank disk")
	if out, err := exec.CommandContext(ctx, "qemu-img", "create", "-f", "raw", rawDiskPath, diskSize).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("isoinstaller: qemu-img create: %w\noutput: %s", err, string(out))
	}

	// ── Serial log ─────────────────────────────────────────────────────────
	serialLogPath := filepath.Join(opts.WorkDir, "serial.log")
	serialWriter, err := openSerialLog(serialLogPath, opts.SerialLog)
	if err != nil {
		return nil, fmt.Errorf("isoinstaller: open serial log: %w", err)
	}
	defer serialWriter.close()

	// ── QMP socket path ────────────────────────────────────────────────────
	qmpSocketPath := filepath.Join(opts.WorkDir, "qmp.sock")

	// ── Build QEMU command ─────────────────────────────────────────────────
	callPhase("launching_vm")
	qemuArgs := buildQEMUArgs(opts, rawDiskPath, seedISOPath, serialLogPath, qmpSocketPath)
	log.Info().
		Strs("args", qemuArgs).
		Msg("isoinstaller: launching QEMU")

	// Apply install timeout.
	installCtx, installCancel := context.WithTimeout(ctx, opts.Timeout)
	defer installCancel()

	qemuBin, ok := FindQEMU()
	if !ok {
		return nil, fmt.Errorf("isoinstaller: qemu not found — install qemu-kvm (RHEL/Rocky) or qemu-system-x86_64 (Debian/Ubuntu)")
	}

	qemu := exec.CommandContext(installCtx, qemuBin, qemuArgs...)

	// Capture QEMU stderr via a pipe so we can stream it to the progress store.
	stderrPipe, pipeErr := qemu.StderrPipe()
	if pipeErr != nil {
		// Non-fatal: fall back to discarding stderr.
		stderrPipe = nil
	}

	start := time.Now()
	if err := qemu.Start(); err != nil {
		return nil, fmt.Errorf("isoinstaller: start QEMU: %w", err)
	}

	// ── Tail serial log in background ────────────────────────────────────
	callPhase("installing")
	serialTailCtx, serialTailCancel := context.WithCancel(installCtx)
	defer serialTailCancel()

	if opts.OnSerialLine != nil {
		go tailFile(serialTailCtx, serialLogPath, opts.OnSerialLine)
	}

	// ── Scan QEMU stderr in background ───────────────────────────────────
	if stderrPipe != nil && opts.OnStderrLine != nil {
		go scanPipeLines(stderrPipe, opts.OnStderrLine)
	}

	// ── Wait for install to complete ───────────────────────────────────────
	// We watch two signals in parallel:
	//   a) QMP socket: guest sends SHUTDOWN event when the installer halts.
	//   b) QEMU process exit: -no-reboot converts halt→exit.
	qmpDone := make(chan error, 1)
	go func() {
		qmpDone <- waitForQMPShutdown(installCtx, qmpSocketPath, log)
	}()

	procDone := make(chan error, 1)
	go func() {
		procDone <- qemu.Wait()
	}()

	var procExitErr error
	select {
	case <-installCtx.Done():
		// Timeout or parent cancellation.
		_ = qemu.Process.Kill()
		<-procDone
		serialTailCancel()
		elapsed := time.Since(start)
		if ctx.Err() != nil {
			return &BuildResult{SerialLogPath: serialLogPath},
				fmt.Errorf("isoinstaller: install cancelled after %v", elapsed.Round(time.Second))
		}
		return &BuildResult{SerialLogPath: serialLogPath},
			fmt.Errorf("isoinstaller: install timed out after %v (limit: %v) — check serial log at %s",
				elapsed.Round(time.Second), opts.Timeout, serialLogPath)

	case qmpErr := <-qmpDone:
		// QMP reported a clean shutdown; wait for the QEMU process to exit.
		if qmpErr != nil {
			log.Warn().Err(qmpErr).Msg("isoinstaller: QMP watcher exited with error (waiting for process exit)")
		}
		// Give the process up to 60s to exit cleanly after the QMP shutdown event.
		exitCtx, exitCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer exitCancel()
		select {
		case err := <-procDone:
			if err != nil {
				log.Debug().Err(err).Msg("isoinstaller: QEMU exited with non-zero (may be normal for -no-reboot)")
			}
			procExitErr = err
		case <-exitCtx.Done():
			_ = qemu.Process.Kill()
			<-procDone
			return &BuildResult{SerialLogPath: serialLogPath},
				fmt.Errorf("isoinstaller: QEMU did not exit within 60s after shutdown event")
		}

	case err := <-procDone:
		// Process exited on its own (triggered by -no-reboot on guest halt).
		procExitErr = err
		if err != nil {
			log.Debug().Err(err).Msg("isoinstaller: QEMU process exited with error")
		}
	}

	// Stop the serial tail goroutine now that QEMU has exited.
	serialTailCancel()

	elapsed := time.Since(start)
	log.Info().
		Dur("elapsed", elapsed.Round(time.Second)).
		Str("disk", rawDiskPath).
		Err(procExitErr).
		Msg("isoinstaller: install VM exited — verifying disk")

	// ── Fail-fast on startup failures ─────────────────────────────────────
	// If QEMU exited in under 60 seconds with a non-zero status, that's a
	// startup failure (wrong CPU flag, missing device, bad ISO, etc.), not a
	// successful install. Anaconda/subiquity installs always take at least
	// 5+ minutes. Proceeding to extract would produce a blank image or
	// confusing partition errors. Read the serial log tail so the admin can
	// see exactly why QEMU failed.
	if procExitErr != nil && elapsed < 60*time.Second {
		tail := readSerialLogTail(serialLogPath, 30)
		return &BuildResult{SerialLogPath: serialLogPath},
			fmt.Errorf("isoinstaller: QEMU exited after %v with status %v — install failed to start (check serial log at %s)\n%s",
				elapsed.Round(time.Second), procExitErr, serialLogPath, tail)
	}

	// ── Verify the disk image was written ─────────────────────────────────
	fi, err := os.Stat(rawDiskPath)
	if err != nil {
		return &BuildResult{SerialLogPath: serialLogPath},
			fmt.Errorf("isoinstaller: raw disk not found after install: %w", err)
	}
	minExpectedBytes := int64(500 * 1024 * 1024) // sanity: at least 500 MB written
	if fi.Size() < minExpectedBytes {
		return &BuildResult{SerialLogPath: serialLogPath},
			fmt.Errorf("isoinstaller: raw disk too small (%d bytes) — install likely failed; check serial log at %s",
				fi.Size(), serialLogPath)
	}

	return &BuildResult{
		RawDiskPath:   rawDiskPath,
		ElapsedTime:   elapsed,
		SerialLogPath: serialLogPath,
	}, nil
}

// tailFile reads lines from path in near-real-time (tail -F semantics),
// calling onLine for each line until ctx is cancelled.
func tailFile(ctx context.Context, path string, onLine func(string)) {
	// Wait for the file to appear.
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return
		}
	}

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			onLine(strings.TrimRight(line, "\r\n"))
		}
		if err == io.EOF {
			select {
			case <-time.After(200 * time.Millisecond):
				continue
			case <-ctx.Done():
				return
			}
		} else if err != nil {
			return
		}
	}
}

// scanPipeLines reads from r line by line, calling onLine for each.
// Returns when the reader is exhausted or closed.
func scanPipeLines(r io.Reader, onLine func(string)) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
}

// readSerialLogTail reads the last N lines of the serial log file and
// returns them formatted as a string for embedding in error messages.
// Returns "(serial log unavailable)" if the file can't be read.
func readSerialLogTail(path string, lines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(serial log unavailable: %v)", err)
	}
	if len(data) == 0 {
		return "(serial log empty — QEMU may have failed before writing any output)"
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return "--- last " + fmt.Sprintf("%d", len(all)) + " serial lines ---\n" + strings.Join(all, "\n")
}

// applyDefaults fills in zero-value BuildOptions fields with sensible defaults.
func applyDefaults(opts *BuildOptions) {
	if opts.DiskSizeGB == 0 {
		opts.DiskSizeGB = defaultDiskSizeGB
	}
	if opts.MemoryMB == 0 {
		opts.MemoryMB = defaultMemoryMB
	}
	if opts.CPUs == 0 {
		opts.CPUs = defaultCPUs
	}
	if opts.Timeout == 0 {
		opts.Timeout = defaultTimeout
	}
}

// buildQEMUArgs constructs the qemu-system-x86_64 argument list.
func buildQEMUArgs(opts BuildOptions, rawDiskPath, seedISOPath, serialLogPath, qmpSocketPath string) []string {
	args := []string{
		// Machine and acceleration.
		"-machine", "pc,accel=kvm:tcg", // prefer KVM, fall back to TCG automatically
		// -cpu host passes through all available host CPU features to the guest.
		// Previously included +vmx for nested virt, but when clonr-server runs
		// inside a VM (as in our Proxmox lab), the parent hypervisor doesn't
		// expose vmx to clonr-server's CPU, so QEMU exits immediately with
		// "CPU feature vmx not available". Bare -cpu host is correct for all
		// deployment topologies — the install VM doesn't need nested virt to
		// run Anaconda or cloud-init.
		"-cpu", "host",
	}

	// When KVM is not available, force TCG (software emulation).
	// The accel=kvm:tcg fallback above handles this automatically in recent QEMU,
	// but we emit a log line upstream so the admin knows it will be slow.
	if !HasKVM() {
		// Replace the first two args with TCG-only config.
		args = []string{"-machine", "pc,accel=tcg", "-cpu", "qemu64"}
	}

	args = append(args,
		"-smp", fmt.Sprintf("%d", opts.CPUs),
		"-m", fmt.Sprintf("%d", opts.MemoryMB),

		// Target disk (where the OS will be installed).
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,cache=none", rawDiskPath),

		// Installer ISO as first CD-ROM (boot device).
		"-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on,if=ide,index=0", opts.ISOPath),

		// Seed ISO (kickstart / cloud-init) as second CD-ROM.
		// Anaconda detects OEMDRV label automatically; Ubuntu detects CIDATA.
		"-drive", fmt.Sprintf("file=%s,media=cdrom,readonly=on,if=ide,index=1", seedISOPath),

		// Boot from CD first.
		"-boot", "order=d,once=d",

		// Networking: user-mode NAT so the installer can reach package mirrors.
		"-netdev", "user,id=net0",
		"-device", "virtio-net-pci,netdev=net0",

		// No display — serial console only.
		"-nographic",

		// Serial port → log file for progress visibility.
		"-serial", fmt.Sprintf("file:%s", serialLogPath),

		// QMP management socket — used to detect clean shutdown.
		"-monitor", fmt.Sprintf("unix:%s,server=on,wait=off", qmpSocketPath),

		// Exit on guest halt instead of rebooting (critical!).
		// The kickstart/autoinstall ends with reboot/halt; with -no-reboot
		// QEMU converts the reboot into a clean process exit.
		"-no-reboot",
	)

	return args
}

// writeSeedISO creates a seed ISO containing the automated-install config files.
// For RHEL-family (OEMDRV label) it creates a single ks.cfg file.
// For Ubuntu (CIDATA label) it creates user-data and meta-data files.
func writeSeedISO(workDir, seedISOPath string, cfg *AutoInstallConfig) error {
	// Write config files into a staging directory.
	stageDir := filepath.Join(workDir, "seed-stage")
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("create seed staging dir: %w", err)
	}

	switch cfg.Format {
	case FormatAutoInstall:
		// Ubuntu: CIDATA ISO requires user-data + meta-data files.
		if err := os.WriteFile(filepath.Join(stageDir, "user-data"), []byte(cfg.KickstartContent), 0o644); err != nil {
			return fmt.Errorf("write user-data: %w", err)
		}
		metaData := cfg.MetaDataContent
		if metaData == "" {
			metaData = "instance-id: clonr-build\nlocal-hostname: generic\n"
		}
		if err := os.WriteFile(filepath.Join(stageDir, "meta-data"), []byte(metaData), 0o644); err != nil {
			return fmt.Errorf("write meta-data: %w", err)
		}

	case FormatPreseed:
		if err := os.WriteFile(filepath.Join(stageDir, "preseed.cfg"), []byte(cfg.KickstartContent), 0o644); err != nil {
			return fmt.Errorf("write preseed.cfg: %w", err)
		}

	case FormatAutoYaST:
		if err := os.WriteFile(filepath.Join(stageDir, "autoinst.xml"), []byte(cfg.KickstartContent), 0o644); err != nil {
			return fmt.Errorf("write autoinst.xml: %w", err)
		}

	default:
		// Kickstart and answers files: ks.cfg on OEMDRV is auto-detected by Anaconda.
		if err := os.WriteFile(filepath.Join(stageDir, "ks.cfg"), []byte(cfg.KickstartContent), 0o644); err != nil {
			return fmt.Errorf("write ks.cfg: %w", err)
		}
	}

	return buildISO(stageDir, seedISOPath, cfg.ISOLabel)
}

// buildISO creates an ISO from srcDir using genisoimage or xorriso, whichever
// is available on the host.
func buildISO(srcDir, dstPath, label string) error {
	if path, err := exec.LookPath("genisoimage"); err == nil {
		out, err := exec.Command(path,
			"-o", dstPath,
			"-V", label,
			"-J", "-r",
			srcDir,
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("genisoimage: %w\noutput: %s", err, string(out))
		}
		return nil
	}

	if path, err := exec.LookPath("xorriso"); err == nil {
		out, err := exec.Command(path,
			"-as", "mkisofs",
			"-o", dstPath,
			"-V", label,
			"-J", "-r",
			srcDir,
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("xorriso: %w\noutput: %s", err, string(out))
		}
		return nil
	}

	return fmt.Errorf("neither genisoimage nor xorriso found; install one of them on the clonr-server host")
}

// ── QMP monitoring ────────────────────────────────────────────────────────────

// waitForQMPShutdown connects to the QEMU QMP socket and waits for a SHUTDOWN
// or POWERDOWN event, which indicates the guest halted cleanly.
//
// It retries the connection for up to 30s to allow QEMU time to start and bind
// the socket. Returns nil when a clean shutdown is detected, or an error if the
// context is cancelled or the socket is not available in time.
func waitForQMPShutdown(ctx context.Context, socketPath string, log zerolog.Logger) error {
	// Wait for the socket to appear (QEMU starts the socket listener early).
	deadline := time.Now().Add(30 * time.Second)
	var conn net.Conn
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("QMP socket not available after 30s")
		}
		var err error
		conn, err = net.DialTimeout("unix", socketPath, 2*time.Second)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	defer conn.Close()

	log.Debug().Msg("isoinstaller: QMP connected")

	// QMP greeting: {"QMP": {"version": ..., "capabilities": [...]}}
	// Then we must send {"execute": "qmp_capabilities"} to enter command mode.
	scanner := bufio.NewScanner(conn)

	// Read greeting.
	if !scanner.Scan() {
		return fmt.Errorf("QMP: no greeting received")
	}

	// Send capabilities handshake.
	if _, err := fmt.Fprintln(conn, `{"execute":"qmp_capabilities"}`); err != nil {
		return fmt.Errorf("QMP: send capabilities: %w", err)
	}

	// Read events until SHUTDOWN/POWERDOWN or context cancellation.
	type qmpEvent struct {
		Event string `json:"event"`
	}
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		var ev qmpEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // not JSON or not an event — skip
		}
		switch ev.Event {
		case "SHUTDOWN", "POWERDOWN", "RESET":
			log.Info().Str("qmp_event", ev.Event).Msg("isoinstaller: QMP shutdown event received")
			return nil
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("QMP: read error: %w", err)
	}
	// Scanner EOF means the QEMU process closed the socket — treat as shutdown.
	return nil
}

// ── Serial log wiring ─────────────────────────────────────────────────────────

// serialLogWriter wraps a log file and optionally mirrors output to an
// additional writer (e.g. a progress streamer).
type serialLogWriter struct {
	file   *os.File
	mirror io.Writer // may be nil
}

func (w *serialLogWriter) close() {
	if w.file != nil {
		_ = w.file.Close()
	}
}

// openSerialLog opens a log file at path and wires the optional mirror writer.
// QEMU writes the serial log directly to the file path (via -serial file:path),
// so this function only opens the file for the caller to read for progress
// parsing, and stores the mirror writer for future use if needed.
func openSerialLog(path string, mirror io.Writer) (*serialLogWriter, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("open serial log %s: %w", path, err)
	}
	return &serialLogWriter{file: f, mirror: mirror}, nil
}
