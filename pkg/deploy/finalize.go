package deploy

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sqoia-dev/clonr/pkg/api"
	"github.com/sqoia-dev/clonr/pkg/ipmi"
)

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
	if err := writeHostname(mountRoot, cfg.Hostname, cfg.FQDN); err != nil {
		return fmt.Errorf("finalize: hostname: %w", err)
	}

	if err := writeNetworkConfig(mountRoot, cfg.Interfaces); err != nil {
		return fmt.Errorf("finalize: network: %w", err)
	}

	if len(cfg.SSHKeys) > 0 {
		if err := writeSSHKeys(mountRoot, cfg.SSHKeys); err != nil {
			return fmt.Errorf("finalize: ssh keys: %w", err)
		}
	}

	if cfg.KernelArgs != "" {
		if err := applyKernelArgs(ctx, mountRoot, cfg.KernelArgs); err != nil {
			// Non-fatal: the GRUB config file edit may have succeeded even if
			// grub2-mkconfig failed in the chroot. Log prominently so operators
			// know to verify the bootloader configuration manually.
			log.Printf("WARNING: finalize: kernel args: %v", err)
		}
	}

	// BMC / IPMI — configure local BMC network and credentials.
	// This operates on the physical BMC directly (not the chroot), so it is
	// done here rather than inside the deployed filesystem.
	if cfg.BMC != nil {
		if err := applyBMCConfig(ctx, cfg.BMC); err != nil {
			// Non-fatal: BMC configuration failure should not abort a deployment.
			// The operator can manually configure the BMC afterward.
			log.Printf("WARNING: finalize: bmc (non-fatal): %v", err)
		}
	}

	// InfiniBand / IPoIB — write udev rules and NetworkManager profiles into
	// the deployed filesystem so IB interfaces come up correctly on first boot.
	if len(cfg.IBConfig) > 0 {
		if err := writeIBConfig(mountRoot, cfg.IBConfig); err != nil {
			return fmt.Errorf("finalize: ib config: %w", err)
		}
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
		chrootArgs := []string{mountRoot, "grub2-mkconfig", "-o", grubCfgPath}
		if out, err := exec.CommandContext(ctx, "chroot", chrootArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf(
				"WARNING: grub configuration update failed — node may not boot with the requested "+
					"kernel arguments. Manual intervention may be required. "+
					"grub2-mkconfig: %w\noutput: %s",
				err, string(out),
			)
		}
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
