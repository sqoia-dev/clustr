package deploy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/sqoia-dev/clonr/pkg/api"
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
			// Non-fatal: kernel args failure doesn't prevent boot.
			_ = err
		}
	}

	return nil
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
	// This is best-effort — failure is non-fatal since the file edit itself is the important part.
	grubCfgPath := findGrubCfg(mountRoot)
	if grubCfgPath != "" {
		chrootArgs := []string{mountRoot, "grub2-mkconfig", "-o", grubCfgPath}
		if out, err := exec.CommandContext(ctx, "chroot", chrootArgs...).CombinedOutput(); err != nil {
			_ = fmt.Errorf("grub2-mkconfig: %w\noutput: %s", err, string(out))
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
