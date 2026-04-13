package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/chroot"
	"github.com/sqoia-dev/clonr/pkg/ipmi"
)

// ifaceNameRe validates that a network interface name contains only safe
// characters before it is used to construct a filename.
var ifaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)

// applyNodeConfig writes all node-specific identity into the deployed filesystem
// rooted at mountRoot. This function is called by both FilesystemDeployer and
// BlockDeployer after the image is on disk.
//
// Order of operations:
//  1. Hostname
//  2. Network config (NetworkManager keyfiles)
//  3. SSH authorized_keys for root
//  4. /etc/fstab UUID update
//  5. Kernel args (GRUB)
//  6. BMC / IPMI network and credentials (if cfg.BMC is set)
//  7. InfiniBand / IPoIB config (if cfg.IBConfig is set)
func applyNodeConfig(ctx context.Context, cfg api.NodeConfig, mountRoot string) error {
	log := logger()

	log.Info().Str("hostname", cfg.Hostname).Msg("finalize: writing /etc/hostname")
	if err := writeHostname(mountRoot, cfg.Hostname, cfg.FQDN); err != nil {
		return fmt.Errorf("finalize: hostname: %w", err)
	}
	log.Info().Str("hostname", cfg.Hostname).Msg("finalize: wrote /etc/hostname")

	log.Info().Int("interfaces", len(cfg.Interfaces)).Msg("finalize: writing NetworkManager connection profiles")
	if err := writeNetworkConfig(mountRoot, cfg.Interfaces); err != nil {
		return fmt.Errorf("finalize: network: %w", err)
	}
	for _, iface := range cfg.Interfaces {
		log.Info().Str("interface", iface.Name).Str("ip", iface.IPAddress).
			Msgf("finalize: wrote /etc/NetworkManager/system-connections/%s.nmconnection", iface.Name)
	}

	if len(cfg.SSHKeys) > 0 {
		log.Info().Int("keys", len(cfg.SSHKeys)).Msg("finalize: writing /root/.ssh/authorized_keys")
		if err := writeSSHKeys(mountRoot, cfg.SSHKeys); err != nil {
			return fmt.Errorf("finalize: ssh keys: %w", err)
		}
		log.Info().Int("keys", len(cfg.SSHKeys)).Msg("finalize: wrote /root/.ssh/authorized_keys")
	}

	if cfg.KernelArgs != "" {
		log.Info().Str("args", cfg.KernelArgs).Msg("finalize: applying kernel args to /etc/default/grub")
		if err := applyKernelArgs(ctx, mountRoot, cfg.KernelArgs); err != nil {
			// Non-fatal: the GRUB config file edit may have succeeded even if
			// grub2-mkconfig failed in the chroot. Log prominently so operators
			// know to verify the bootloader configuration manually.
			log.Warn().Err(err).Msg("WARNING: finalize: kernel args update failed (non-fatal) — manual intervention may be required")
		} else {
			log.Info().Str("args", cfg.KernelArgs).Msg("finalize: kernel args applied, ran grub2-mkconfig")
		}
	}

	// BMC / IPMI — configure local BMC network and credentials.
	// This operates on the physical BMC directly (not the chroot), so it is
	// done here rather than inside the deployed filesystem.
	if cfg.BMC != nil {
		log.Info().Str("bmc_ip", cfg.BMC.IPAddress).Msg("finalize: configuring BMC via ipmitool")
		if err := applyBMCConfig(ctx, cfg.BMC); err != nil {
			// Non-fatal: BMC configuration failure should not abort a deployment.
			// The operator can manually configure the BMC afterward.
			log.Warn().Err(err).Msg("WARNING: finalize: BMC configuration failed (non-fatal)")
		} else {
			log.Info().Str("bmc_ip", cfg.BMC.IPAddress).Msg("finalize: BMC configured")
		}
	}

	// InfiniBand / IPoIB — write udev rules and NetworkManager profiles into
	// the deployed filesystem so IB interfaces come up correctly on first boot.
	if len(cfg.IBConfig) > 0 {
		log.Info().Int("devices", len(cfg.IBConfig)).Msg("finalize: writing InfiniBand/IPoIB configuration")
		if err := writeIBConfig(mountRoot, cfg.IBConfig); err != nil {
			return fmt.Errorf("finalize: ib config: %w", err)
		}
		log.Info().Int("devices", len(cfg.IBConfig)).Msg("finalize: InfiniBand/IPoIB configuration written")
	}

	return nil
}

// applyBMCConfig configures the local BMC via ipmitool.
// This targets the physical BMC of the node being finalized, not the chroot.
func applyBMCConfig(ctx context.Context, bmc *api.BMCNodeConfig) error {
	c := &ipmi.Client{} // local BMC — no host flags

	bmcCfg := ipmi.BMCConfig{
		Channel:   1,
		IPAddress: bmc.IPAddress,
		Netmask:   bmc.Netmask,
		Gateway:   bmc.Gateway,
		IPSource:  "static",
	}
	if err := c.SetBMCNetwork(ctx, bmcCfg); err != nil {
		return fmt.Errorf("set bmc network: %w", err)
	}

	if bmc.Username != "" && bmc.Password != "" {
		// Use user slot 2 — slot 1 is typically the reserved anonymous user.
		if err := c.SetBMCUser(ctx, 2, bmc.Username, bmc.Password); err != nil {
			return fmt.Errorf("set bmc user: %w", err)
		}
	}
	return nil
}

// writeIBConfig writes IPoIB NetworkManager connection profiles and udev rules
// for each InfiniBand device into the deployed filesystem at mountRoot.
func writeIBConfig(mountRoot string, ibCfgs []api.IBInterfaceConfig) error {
	nmDir := filepath.Join(mountRoot, "etc", "NetworkManager", "system-connections")
	if err := os.MkdirAll(nmDir, 0o700); err != nil {
		return fmt.Errorf("mkdir NM connections: %w", err)
	}

	for _, ib := range ibCfgs {
		if err := writeIPoIBProfile(nmDir, ib); err != nil {
			return fmt.Errorf("ib device %s: %w", ib.DeviceName, err)
		}
	}
	return nil
}

// writeIPoIBProfile writes a NetworkManager keyfile for an IPoIB interface.
// The interface name is derived from the IB device name (e.g. mlx5_0 → ib0).
func writeIPoIBProfile(nmDir string, ib api.IBInterfaceConfig) error {
	// IPoIB interface naming: mlx5_0→ib0, mlx5_1→ib1, hfi1_0→ib0, etc.
	// We use the device name directly as the NM connection id and interface-name.
	ifaceName := ibDeviceToIPoIBName(ib.DeviceName)

	// Validate the derived interface name before using it as a filename component.
	if !ifaceNameRe.MatchString(ifaceName) {
		return fmt.Errorf("derived IB interface name %q contains invalid characters", ifaceName)
	}

	mtu := ib.MTU
	if mtu == 0 {
		if strings.EqualFold(ib.IPoIBMode, "connected") {
			mtu = 65520
		} else {
			mtu = 2044 // datagram mode default
		}
	}

	mode := strings.ToLower(ib.IPoIBMode)
	if mode == "" {
		mode = "datagram"
	}

	var sb strings.Builder
	sb.WriteString("[connection]\n")
	fmt.Fprintf(&sb, "id=%s\n", ifaceName)
	sb.WriteString("type=infiniband\n")
	fmt.Fprintf(&sb, "interface-name=%s\n", ifaceName)
	sb.WriteString("\n")

	sb.WriteString("[infiniband]\n")
	fmt.Fprintf(&sb, "transport-mode=%s\n", mode)
	fmt.Fprintf(&sb, "mtu=%d\n", mtu)
	if len(ib.PKeys) > 0 {
		// Write the first partition key; additional pkeys require separate profiles.
		fmt.Fprintf(&sb, "p-key=%s\n", ib.PKeys[0])
	}
	sb.WriteString("\n")

	sb.WriteString("[ipv4]\n")
	if ib.IPAddress != "" {
		ip, prefix := parseIPCIDR(ib.IPAddress)
		sb.WriteString("method=manual\n")
		fmt.Fprintf(&sb, "address1=%s/%s\n", ip, prefix)
	} else {
		sb.WriteString("method=disabled\n")
	}
	sb.WriteString("\n")

	sb.WriteString("[ipv6]\n")
	sb.WriteString("method=ignore\n")

	filename := filepath.Join(nmDir, ifaceName+".nmconnection")
	return os.WriteFile(filename, []byte(sb.String()), 0o600)
}

// ibDeviceToIPoIBName maps an IB device name to its IPoIB interface name.
// e.g. "mlx5_0" → "ib0", "mlx5_1" → "ib1", "hfi1_0" → "ib0"
func ibDeviceToIPoIBName(devName string) string {
	// Extract trailing digit after the last underscore for the IB port index.
	idx := strings.LastIndex(devName, "_")
	if idx >= 0 && idx < len(devName)-1 {
		suffix := devName[idx+1:]
		// Only use the numeric suffix as the ib interface index.
		allDigits := true
		for _, c := range suffix {
			if c < '0' || c > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return "ib" + suffix
		}
	}
	return "ib0"
}

// writeHostname writes /etc/hostname and updates /etc/hosts.
func writeHostname(mountRoot, hostname, fqdn string) error {
	if hostname == "" {
		return nil
	}

	hostnameFile := filepath.Join(mountRoot, "etc", "hostname")
	if err := os.MkdirAll(filepath.Dir(hostnameFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(hostnameFile, []byte(hostname+"\n"), 0o644); err != nil {
		return fmt.Errorf("write /etc/hostname: %w", err)
	}

	// Update /etc/hosts: add or replace the 127.0.1.1 line.
	hostsFile := filepath.Join(mountRoot, "etc", "hosts")
	existing, err := os.ReadFile(hostsFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read /etc/hosts: %w", err)
	}

	// Build the new hosts content.
	var lines []string
	for _, line := range strings.Split(string(existing), "\n") {
		// Remove any existing 127.0.1.1 lines.
		if strings.HasPrefix(strings.TrimSpace(line), "127.0.1.1") {
			continue
		}
		lines = append(lines, line)
	}

	entry := "127.0.1.1\t" + hostname
	if fqdn != "" && fqdn != hostname {
		entry = "127.0.1.1\t" + fqdn + "\t" + hostname
	}
	lines = append(lines, entry)

	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return os.WriteFile(hostsFile, []byte(content), 0o644)
}

// writeNetworkConfig writes NetworkManager keyfile connections for each interface.
// This is the standard config format on Rocky Linux 8/9, RHEL 8/9, and derivatives.
func writeNetworkConfig(mountRoot string, interfaces []api.InterfaceConfig) error {
	if len(interfaces) == 0 {
		return nil
	}

	nmDir := filepath.Join(mountRoot, "etc", "NetworkManager", "system-connections")
	if err := os.MkdirAll(nmDir, 0o700); err != nil {
		return fmt.Errorf("mkdir NM connections: %w", err)
	}

	for _, iface := range interfaces {
		if err := writeNMKeyfile(nmDir, iface); err != nil {
			return fmt.Errorf("interface %s: %w", iface.Name, err)
		}
	}
	return nil
}

// writeNMKeyfile writes a single NetworkManager keyfile for an interface.
func writeNMKeyfile(nmDir string, iface api.InterfaceConfig) error {
	name := iface.Name
	if name == "" {
		name = iface.MACAddress
	}

	// Validate the interface name before using it as a filename component.
	if !ifaceNameRe.MatchString(name) {
		return fmt.Errorf("interface name %q contains invalid characters (must match ^[a-zA-Z0-9._:-]+$)", name)
	}

	// Parse IP and prefix from CIDR.
	ip, prefix := parseIPCIDR(iface.IPAddress)

	var sb strings.Builder
	sb.WriteString("[connection]\n")
	fmt.Fprintf(&sb, "id=%s\n", name)
	fmt.Fprintf(&sb, "type=ethernet\n")
	fmt.Fprintf(&sb, "interface-name=%s\n", name)
	sb.WriteString("\n")

	sb.WriteString("[ethernet]\n")
	if iface.MACAddress != "" {
		fmt.Fprintf(&sb, "mac-address=%s\n", iface.MACAddress)
	}
	if iface.MTU > 0 {
		fmt.Fprintf(&sb, "mtu=%d\n", iface.MTU)
	}
	sb.WriteString("\n")

	sb.WriteString("[ipv4]\n")
	if ip != "" {
		sb.WriteString("method=manual\n")
		fmt.Fprintf(&sb, "address1=%s/%s", ip, prefix)
		if iface.Gateway != "" {
			fmt.Fprintf(&sb, ",%s", iface.Gateway)
		}
		sb.WriteString("\n")
		if len(iface.DNS) > 0 {
			fmt.Fprintf(&sb, "dns=%s;\n", strings.Join(iface.DNS, ";"))
		}
	} else {
		sb.WriteString("method=auto\n")
	}
	sb.WriteString("\n")

	sb.WriteString("[ipv6]\n")
	sb.WriteString("method=ignore\n")

	filename := filepath.Join(nmDir, name+".nmconnection")
	// Mode 0600 is required by NM — it refuses to load world-readable keyfiles.
	return os.WriteFile(filename, []byte(sb.String()), 0o600)
}

// parseIPCIDR splits "192.168.1.10/24" into ("192.168.1.10", "24").
// Returns ("", "") if the input is empty or malformed.
func parseIPCIDR(cidr string) (ip, prefix string) {
	parts := strings.SplitN(cidr, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "24" // default /24 if no prefix given
	}
	return "", ""
}

// writeSSHKeys writes authorized_keys for root.
func writeSSHKeys(mountRoot string, keys []string) error {
	sshDir := filepath.Join(mountRoot, "root", ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("mkdir .ssh: %w", err)
	}

	authKeysPath := filepath.Join(sshDir, "authorized_keys")
	content := strings.Join(keys, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	return os.WriteFile(authKeysPath, []byte(content), 0o600)
}

// applyKernelArgs appends extra kernel args to the GRUB default config.
// It looks for GRUB_CMDLINE_LINUX in /etc/default/grub and appends args.
func applyKernelArgs(ctx context.Context, mountRoot, kernelArgs string) error {
	grubDefault := filepath.Join(mountRoot, "etc", "default", "grub")
	raw, err := os.ReadFile(grubDefault)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no GRUB config — might be a systemd-boot system
		}
		return fmt.Errorf("read grub config: %w", err)
	}

	content := string(raw)
	updated := false
	var newLines []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "GRUB_CMDLINE_LINUX=") {
			// Strip trailing quote, append args, re-add quote.
			trimmed := strings.TrimSuffix(strings.TrimSpace(line), `"`)
			line = trimmed + " " + kernelArgs + `"`
			updated = true
		}
		newLines = append(newLines, line)
	}

	if !updated {
		// Key not present — append it.
		newLines = append(newLines, fmt.Sprintf(`GRUB_CMDLINE_LINUX="%s"`, kernelArgs))
	}

	newContent := strings.Join(newLines, "\n")
	if err := os.WriteFile(grubDefault, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write grub config: %w", err)
	}

	// Attempt to regenerate grub.cfg inside the chroot using chroot + grub2-mkconfig.
	// grub2-mkconfig may not be available in all chroot environments (e.g. minimal
	// initramfs deployments or systemd-boot systems). The /etc/default/grub edit
	// above is the durable change; grub2-mkconfig makes it take effect immediately.
	// Failure here is non-fatal but MUST be surfaced — the node may not boot with
	// the requested kernel arguments without manual intervention.
	grubCfgPath := findGrubCfg(mountRoot)
	if grubCfgPath != "" {
		logger().Info().Str("grub_cfg", grubCfgPath).Msg("running grub2-mkconfig in chroot")
		chrootArgs := []string{mountRoot, "grub2-mkconfig", "-o", grubCfgPath}
		if err := runAndLog(ctx, "grub2-mkconfig", exec.CommandContext(ctx, "chroot", chrootArgs...)); err != nil {
			return fmt.Errorf(
				"WARNING: grub configuration update failed — node may not boot with the requested "+
					"kernel arguments. Manual intervention may be required. "+
					"grub2-mkconfig: %w",
				err,
			)
		}
		logger().Info().Str("grub_cfg", grubCfgPath).Msg("grub2-mkconfig complete")
	}

	return nil
}

// findGrubCfg looks for grub.cfg in common locations within the mount root.
func findGrubCfg(mountRoot string) string {
	candidates := []string{
		"/boot/grub2/grub.cfg",
		"/boot/grub/grub.cfg",
		"/boot/efi/EFI/rocky/grub.cfg",
		"/boot/efi/EFI/redhat/grub.cfg",
		"/boot/efi/EFI/centos/grub.cfg",
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(mountRoot, c)); err == nil {
			return c // return path relative to chroot root
		}
	}
	return ""
}

// installKernelInChroot installs the kernel and GRUB2 bootloader packages inside
// the deployed chroot using dnf. This is needed when the image was captured
// without /boot contents (e.g. --one-file-system capture with a separate /boot
// partition on the source). After this function returns, /boot/grub2/grub.cfg
// and at least one vmlinuz + initramfs pair will exist in the chroot.
//
// The function mounts /proc, /sys, and /dev into the chroot before running dnf
// because dnf's scriptlets require these virtual filesystems.
func installKernelInChroot(ctx context.Context, mountRoot, targetDisk string) error {
	log := logger()

	// Bind-mount virtual filesystems required by dnf/rpm scriptlets.
	type bindMount struct{ src, dst string }
	binds := []bindMount{
		{"/proc", filepath.Join(mountRoot, "proc")},
		{"/sys", filepath.Join(mountRoot, "sys")},
		{"/dev", filepath.Join(mountRoot, "dev")},
		{"/dev/pts", filepath.Join(mountRoot, "dev", "pts")},
	}
	for _, b := range binds {
		if err := os.MkdirAll(b.dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", b.dst, err)
		}
		if out, err := exec.CommandContext(ctx, "mount", "--bind", b.src, b.dst).CombinedOutput(); err != nil {
			return fmt.Errorf("bind-mount %s → %s: %w\n%s", b.src, b.dst, err, string(out))
		}
	}
	defer func() {
		// Unmount in reverse order.
		for i := len(binds) - 1; i >= 0; i-- {
			_ = exec.Command("umount", "-l", binds[i].dst).Run()
		}
	}()

	// Copy the host's /etc/resolv.conf into the chroot so dnf can reach the network.
	resolvSrc := "/etc/resolv.conf"
	resolvDst := filepath.Join(mountRoot, "etc", "resolv.conf")
	if data, err := os.ReadFile(resolvSrc); err == nil {
		_ = os.WriteFile(resolvDst, data, 0o644)
	}

	// Ensure /tmp exists and is writable inside the chroot.
	// dnf requires /tmp for its download cache and RPM scriptlets.
	// Images captured without /tmp (or with sticky-bit /tmp permissions stripped)
	// will cause dnf to fail with "mkstemp: No such file or directory".
	tmpDir := filepath.Join(mountRoot, "tmp")
	if err := os.MkdirAll(tmpDir, 0o1777); err != nil {
		log.Warn().Err(err).Str("path", tmpDir).Msg("finalize/boot: could not create /tmp in chroot (non-fatal)")
	}
	// Ensure /boot/grub2 directory exists so grub2-mkconfig can write grub.cfg.
	grub2Dir := filepath.Join(mountRoot, "boot", "grub2")
	if err := os.MkdirAll(grub2Dir, 0o755); err != nil {
		log.Warn().Err(err).Str("path", grub2Dir).Msg("finalize/boot: could not create /boot/grub2 in chroot (non-fatal)")
	}

	// Install kernel and grub2-pc inside the deployed chroot.
	// We always use "chroot <mountRoot> dnf" because the initramfs (deployment host)
	// does not have dnf — it must be invoked from within the deployed image, which
	// is a full Rocky Linux 9 system with /usr/bin/dnf present.
	//
	// IMPORTANT: disable all third-party repos (epel, epel-next, etc.) during
	// this install. The initramfs environment has no external DNS resolution and
	// any attempt to reach mirrors.fedoraproject.org or similar will time out and
	// cause dnf to fail. Kernel and grub2 are in the base Rocky repos (baseos,
	// appstream, extras) which are served by the local mirrorlist / fastest-mirror
	// cache inside the chroot, but to be safe we explicitly enable only baseos and
	// appstream and disable everything else.
	packages := []string{"kernel", "grub2-pc", "grub2-tools"}
	log.Info().Strs("packages", packages).Msg("finalize/boot: installing kernel + grub2 via chroot dnf (baseos+appstream only)")
	chrootDnfArgs := append([]string{
		mountRoot, "dnf", "-y",
		"--setopt=install_weak_deps=False",
		"--disablerepo=*",
		"--enablerepo=baseos",
		"--enablerepo=appstream",
		"install",
	}, packages...)
	if err := runAndLog(ctx, "chroot-dnf-kernel-install", exec.CommandContext(ctx, "chroot", chrootDnfArgs...)); err != nil {
		return fmt.Errorf("chroot dnf install kernel+grub2: %w", err)
	}

	// Install GRUB to the target disk MBR now that the grub2-pc package is present.
	// grub2-install needs access to /boot (already mounted) and /dev.
	bootDir := filepath.Join(mountRoot, "boot")
	grubInstallArgs := []string{"--target=i386-pc", "--boot-directory=" + bootDir, "--recheck", targetDisk}
	log.Info().Str("disk", targetDisk).Msg("finalize/boot: re-running grub2-install after kernel install")
	if err := runAndLog(ctx, "grub2-install", exec.CommandContext(ctx, "grub2-install", grubInstallArgs...)); err != nil {
		log.Warn().Err(err).Msg("finalize/boot: grub2-install after kernel install failed (non-fatal)")
	}

	// Generate grub.cfg inside the chroot.
	grubCfgInChroot := "/boot/grub2/grub.cfg"
	log.Info().Str("grub_cfg", grubCfgInChroot).Msg("finalize/boot: generating grub.cfg via chroot grub2-mkconfig")
	mkcfgCmd := exec.CommandContext(ctx, "chroot", mountRoot, "grub2-mkconfig", "-o", grubCfgInChroot)
	if err := runAndLog(ctx, "grub2-mkconfig", mkcfgCmd); err != nil {
		return fmt.Errorf("finalize/boot: grub2-mkconfig after kernel install: %w", err)
	}
	log.Info().Msg("finalize/boot: kernel + grub2 installed and configured")
	return nil
}

// applyBootConfig performs the post-install steps required to produce a bootable
// deployed filesystem. It must be called after grub2-install has written the
// bootloader to the MBR/bios-boot partition and after the full tar extraction is
// complete. The steps are:
//
//  1. Regenerate /boot/grub2/grub.cfg via grub2-mkconfig inside a chroot with
//     /proc, /sys, and /dev bind-mounted (required by grub2-mkconfig).
//  2. Regenerate the initramfs via dracut --no-hostonly so the resulting image
//     contains drivers for all hardware, not just the capture source's hardware
//     (critical for virtio_blk/virtio_net on VMs).
//  3. Rewrite /etc/fstab with the UUIDs of the newly partitioned disks, replacing
//     the stale UUIDs from the capture source.
//  4. Truncate /etc/machine-id so systemd generates a new unique ID on first boot.
//  5. Remove /etc/ssh/ssh_host_* so sshd regenerates host keys on first boot,
//     preventing all deployed nodes from sharing identical host keys.
//
// partDevs must be in the same order as layout.Partitions (index i → device i+1).
func applyBootConfig(ctx context.Context, mountRoot, targetDisk string, layout api.DiskLayout, partDevs []string) error {
	log := logger()

	// ── 1. grub2-mkconfig ────────────────────────────────────────────────────
	// grub2-mkconfig probes /proc and /sys for bootable kernels — it must run
	// inside a chroot with the standard virtual filesystems mounted.
	grubCfgPath := findGrubCfg(mountRoot)
	if grubCfgPath == "" {
		// /boot is empty — the image was captured without /boot contents (common
		// when the source system has /boot on a separate partition and the capture
		// used --one-file-system). Install the kernel and grub2 packages inside the
		// chroot so that grub2-mkconfig has something to build a menu from.
		log.Warn().Str("mountRoot", mountRoot).
			Msg("finalize/boot: grub.cfg not found — /boot is empty; installing kernel + grub2 via dnf in chroot")
		if err := installKernelInChroot(ctx, mountRoot, targetDisk); err != nil {
			return fmt.Errorf("finalize/boot: kernel install: %w", err)
		}
		// Re-check after install.
		grubCfgPath = findGrubCfg(mountRoot)
		if grubCfgPath == "" {
			return fmt.Errorf("finalize/boot: grub.cfg still not found after kernel install — dnf may have failed silently")
		}
	}

	// ── 2. /etc/fstab UUID update (BEFORE grub2-mkconfig) ───────────────────
	// CRITICAL: writeFstab must run BEFORE grub2-mkconfig. grub2-mkconfig reads
	// /etc/fstab inside the chroot to determine the root filesystem UUID for the
	// kernel cmdline (root=UUID=...). If fstab still carries the capture source's
	// UUIDs when grub2-mkconfig runs, the generated grub.cfg will have wrong UUIDs
	// and the deployed node will fail to find its root filesystem on boot.
	//
	// writeFstab uses blkid on the HOST (outside the chroot) against the actual
	// target partition devices — it does not need the chroot session to be active.
	log.Info().Msg("finalize/boot: regenerating /etc/fstab with target disk UUIDs (before grub2-mkconfig)")
	if err := writeFstab(ctx, mountRoot, layout, partDevs); err != nil {
		// Fatal: a missing or incorrect fstab means the root filesystem won't
		// mount and the OS will drop to emergency mode on every boot.
		return fmt.Errorf("finalize/boot: fstab regen: %w", err)
	}
	log.Info().Msg("finalize/boot: /etc/fstab written with target UUIDs")

	log.Info().Str("grub_cfg", grubCfgPath).Msg("finalize/boot: regenerating GRUB config via grub2-mkconfig")

	// Ensure the standard virtual filesystem mount points exist in the deployed
	// root before calling cs.Enter(). These directories are NOT present in tar
	// archives captured via rsync (rsync skips empty stub directories for virtual
	// mounts like /proc, /sys, /dev). Without them, MountAll fails with ENOENT.
	for _, dir := range []string{"proc", "sys", "dev", "dev/pts", "run"} {
		target := filepath.Join(mountRoot, dir)
		if err := os.MkdirAll(target, 0o755); err != nil {
			log.Warn().Err(err).Str("dir", target).Msg("finalize/boot: could not create chroot mount dir (non-fatal)")
		}
	}

	cs, err := chroot.NewSession(mountRoot)
	if err != nil {
		return fmt.Errorf("finalize/boot: chroot session: %w", err)
	}
	if err := cs.Enter(); err != nil {
		return fmt.Errorf("finalize/boot: chroot enter: %w", err)
	}
	defer func() {
		if cerr := cs.Close(); cerr != nil {
			log.Warn().Err(cerr).Msg("finalize/boot: chroot close error (non-fatal)")
		}
	}()

	// grub2-mkconfig writes the grub.cfg to the path passed via -o (inside the
	// chroot, so the path is relative to the chroot root). At this point /etc/fstab
	// already has the correct target UUIDs, so grub2-mkconfig will generate the
	// correct root=UUID=... kernel cmdline.
	mkcfgCmd := exec.CommandContext(ctx, "chroot", mountRoot, "grub2-mkconfig", "-o", grubCfgPath)
	if err := runAndLog(ctx, "grub2-mkconfig", mkcfgCmd); err != nil {
		// Fatal: without a valid grub.cfg the node cannot boot.
		return fmt.Errorf("finalize/boot: grub2-mkconfig failed: %w", err)
	}
	log.Info().Str("grub_cfg", grubCfgPath).Msg("finalize/boot: grub2-mkconfig complete")

	// ── 3. dracut --regenerate-all ───────────────────────────────────────────
	// --no-hostonly is critical: the capture source may be bare metal with a
	// specific set of drivers. We need a generic initramfs that includes virtio_blk,
	// virtio_net, and xfs so the image boots on any target (VM or physical).
	// --force overwrites any existing initramfs images without prompting.

	// Ensure /var/tmp exists in the chroot — dracut requires it as a scratch
	// directory. Images captured via rsync may not include empty tmpdir stubs.
	if err := os.MkdirAll(filepath.Join(mountRoot, "var", "tmp"), 0o1777); err != nil {
		log.Warn().Err(err).Msg("finalize/boot: could not create /var/tmp in chroot (non-fatal)")
	}

	log.Info().Msg("finalize/boot: regenerating initramfs via dracut --no-hostonly --regenerate-all")
	dracutCmd := exec.CommandContext(ctx, "chroot", mountRoot,
		"dracut", "--force", "--no-hostonly", "--regenerate-all")
	if err := runAndLog(ctx, "dracut", dracutCmd); err != nil {
		// Non-fatal: the node may still boot if the capture source's initramfs
		// happens to contain the required drivers. Log loudly so the operator
		// knows to investigate if the node kernel-panics on boot.
		log.Warn().Err(err).
			Msg("WARNING finalize/boot: dracut --regenerate-all failed — initramfs may lack hardware drivers for target; node may not boot on different hardware")
	} else {
		log.Info().Msg("finalize/boot: dracut complete")
	}

	// ── 4. machine-id scrub ──────────────────────────────────────────────────
	// systemd uses /etc/machine-id as a stable unique identifier for the host.
	// A non-empty machine-id baked into the image would be shared by every node
	// deployed from it. Truncating (not removing) the file causes systemd to
	// generate a new ID on first boot and write it back.
	machineIDPath := filepath.Join(mountRoot, "etc", "machine-id")
	// Mode 0644: must be writable so systemd-machine-id-setup can write the new ID
	// on first boot. The previous 0444 (read-only) prevented systemd from updating
	// the file, leaving the machine-id empty and causing sshd-keygen to fail.
	if err := os.WriteFile(machineIDPath, []byte{}, 0o644); err != nil {
		log.Warn().Err(err).Str("path", machineIDPath).
			Msg("finalize/boot: could not truncate machine-id (non-fatal)")
	} else {
		log.Info().Msg("finalize/boot: machine-id truncated — new ID will be generated on first boot")
	}

	// ── 5. SSH host key scrub ────────────────────────────────────────────────
	// Host keys baked into the image would be identical on every deployed node,
	// making MITM attacks trivial. Remove them so sshd regenerates unique keys
	// on first boot via the ssh-keygen firstboot unit.
	hostKeys, _ := filepath.Glob(filepath.Join(mountRoot, "etc", "ssh", "ssh_host_*"))
	for _, k := range hostKeys {
		if err := os.Remove(k); err != nil {
			log.Warn().Err(err).Str("key", k).
				Msg("finalize/boot: could not remove SSH host key (non-fatal)")
		}
	}
	if len(hostKeys) > 0 {
		log.Info().Int("count", len(hostKeys)).
			Msg("finalize/boot: SSH host keys removed — sshd will regenerate on first boot")
	}

	return nil
}

// getUUID returns the filesystem UUID of a block device using blkid.
// Returns an error if blkid is unavailable or the device has no UUID (e.g.
// unformatted or a bios_grub partition).
func getUUID(ctx context.Context, device string) (string, error) {
	out, err := exec.CommandContext(ctx, "blkid", "-s", "UUID", "-o", "value", device).Output()
	if err != nil {
		return "", fmt.Errorf("blkid %s: %w", device, err)
	}
	uuid := strings.TrimSpace(string(out))
	if uuid == "" {
		return "", fmt.Errorf("blkid %s: no UUID (partition may be unformatted)", device)
	}
	return uuid, nil
}

// writeFstab generates /etc/fstab for the deployed filesystem from the actual
// UUIDs of the target partitions. It replaces whatever fstab the image carried
// from the capture source, which has the source system's UUIDs.
//
// Only partitions with a non-empty MountPoint and a formattable filesystem are
// written. biosboot/bios_grub partitions are silently skipped.
func writeFstab(ctx context.Context, mountRoot string, layout api.DiskLayout, partDevs []string) error {
	log := logger()

	var sb strings.Builder
	sb.WriteString("# /etc/fstab — generated by clonr during deployment\n")
	sb.WriteString("# <device>  <mountpoint>  <fstype>  <options>  <dump>  <pass>\n\n")

	for i, p := range layout.Partitions {
		if p.MountPoint == "" {
			continue // biosboot, unpartitioned, or no mount needed
		}
		switch p.Filesystem {
		case "", "biosboot", "bios_grub":
			continue // no filesystem, nothing to mount
		}

		if i >= len(partDevs) {
			log.Warn().Int("partition", i+1).Msg("writeFstab: partDevs slice shorter than layout — skipping")
			continue
		}
		dev := partDevs[i]

		uuid, err := getUUID(ctx, dev)
		if err != nil {
			// If we can't get a UUID the fstab entry would be useless and the
			// node would fail to boot. Return an error so Finalize aborts cleanly
			// rather than deploying a machine that will boot-loop.
			return fmt.Errorf("partition %d (%s): %w", i+1, dev, err)
		}

		// Standard mount options per filesystem and mountpoint.
		opts := fstabMountOpts(p.Filesystem, p.MountPoint)

		// dump/pass: root gets pass=1, /boot gets pass=2, everything else 0.
		dump := 0
		pass := 0
		switch p.MountPoint {
		case "/":
			dump, pass = 1, 1
		case "/boot":
			dump, pass = 1, 2
		}

		if p.Filesystem == "swap" {
			fmt.Fprintf(&sb, "UUID=%-36s  %-12s  %-6s  %-20s  %d  %d\n",
				uuid, "swap", "swap", "defaults", 0, 0)
		} else {
			fmt.Fprintf(&sb, "UUID=%-36s  %-12s  %-6s  %-20s  %d  %d\n",
				uuid, p.MountPoint, p.Filesystem, opts, dump, pass)
		}

		log.Info().Str("uuid", uuid).Str("mountpoint", p.MountPoint).
			Str("device", dev).Msg("finalize/boot: fstab entry written")
	}

	fstabPath := filepath.Join(mountRoot, "etc", "fstab")
	if err := os.WriteFile(fstabPath, []byte(sb.String()), 0o644); err != nil {
		return fmt.Errorf("write fstab: %w", err)
	}
	return nil
}

// fstabMountOpts returns appropriate mount options for a given filesystem type
// and mountpoint. These are conservative production defaults for Rocky Linux 9.
func fstabMountOpts(fstype, mountpoint string) string {
	switch fstype {
	case "xfs":
		if mountpoint == "/" {
			return "defaults,noatime"
		}
		return "defaults"
	case "ext4":
		if mountpoint == "/" {
			return "defaults,noatime"
		}
		return "defaults"
	case "vfat", "fat32":
		return "defaults,uid=0,gid=0,umask=077,shortname=winnt"
	case "swap":
		return "defaults"
	default:
		return "defaults"
	}
}

// applyExtraMounts appends custom fstab entries from NodeConfig.ExtraMounts and
// optionally creates the mount point directories inside the deployed filesystem.
// It must be called after applyBootConfig so the base fstab already exists.
//
// Invalid entries are skipped with a warning rather than aborting the deployment,
// because a bad shared-storage entry should not prevent the node from booting.
func applyExtraMounts(ctx context.Context, mountRoot string, mounts []api.FstabEntry) error {
	if len(mounts) == 0 {
		return nil
	}

	log := logger()

	fstabPath := filepath.Join(mountRoot, "etc", "fstab")
	f, err := os.OpenFile(fstabPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("applyExtraMounts: open fstab: %w", err)
	}
	defer f.Close()

	fmt.Fprintf(f, "\n# Additional mounts from clonr NodeConfig.ExtraMounts\n")

	written := 0
	for _, m := range mounts {
		if err := api.ValidateFstabEntry(m); err != nil {
			log.Warn().Err(err).Str("mount_point", m.MountPoint).
				Msg("finalize: skipping invalid extra fstab entry")
			continue
		}

		// Auto-create the mount point directory inside the deployed filesystem.
		if m.AutoMkdir {
			targetDir := filepath.Join(mountRoot, m.MountPoint)

			// Path traversal guard: the resolved absolute path must remain inside mountRoot.
			absTarget, err := filepath.Abs(targetDir)
			if err != nil || !strings.HasPrefix(absTarget, mountRoot+string(filepath.Separator)) {
				log.Warn().Str("mount_point", m.MountPoint).
					Msg("finalize: mount point escapes deployed filesystem root — skipping mkdir")
			} else if err := os.MkdirAll(absTarget, 0755); err != nil {
				// Non-fatal: the mount will fail at boot, but we still write the entry
				// so the operator can investigate rather than silently losing the config.
				log.Warn().Err(err).Str("path", absTarget).
					Msg("finalize: failed to create extra mount point (non-fatal)")
			} else {
				log.Info().Str("path", m.MountPoint).
					Msg("finalize: created mount point for extra mount")
			}
		}

		// Resolve mount options: default to "defaults" when empty, then auto-inject
		// _netdev for network filesystems that omit it (needed so systemd waits for
		// the network before attempting the mount).
		opts := m.Options
		if opts == "" {
			opts = "defaults"
		}
		if api.IsNetworkFS(m.FSType) && !strings.Contains(opts, "_netdev") {
			opts += ",_netdev"
			log.Info().Str("fs_type", m.FSType).Str("mount_point", m.MountPoint).
				Msg("finalize: auto-added _netdev to extra mount options for network filesystem")
		}

		if m.Comment != "" {
			fmt.Fprintf(f, "# %s\n", m.Comment)
		}
		// fstab format: source  mountpoint  fstype  options  dump  pass
		fmt.Fprintf(f, "%s\t%s\t%s\t%s\t%d\t%d\n",
			m.Source, m.MountPoint, m.FSType, opts, m.Dump, m.Pass)

		log.Info().
			Str("source", m.Source).
			Str("mount_point", m.MountPoint).
			Str("fs_type", m.FSType).
			Msg("finalize: wrote extra fstab entry")
		written++
	}

	log.Info().Int("count", written).Msg("finalize: extra fstab entries written")
	return nil
}
