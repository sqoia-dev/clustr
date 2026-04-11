package image

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ScrubNodeIdentity removes or truncates files that make a filesystem image
// unique to a particular machine. After scrubbing, the image can be deployed
// to any node and will regenerate identity on first boot.
//
// Files removed:
//   - /etc/machine-id          (systemd regenerates on boot)
//   - /etc/ssh/ssh_host_*      (sshd regenerates on first start)
//   - /etc/sysconfig/network-scripts/ifcfg-* (except lo)
//   - /etc/NetworkManager/system-connections/*
//   - /etc/udev/rules.d/70-persistent-net.rules
//   - /root/.bash_history, /home/*/.bash_history
//   - /tmp/*, /var/tmp/*
//
// Files truncated (zeroed but left in place so systemd can write to them):
//   - /etc/hostname
//   - /var/log/* (recursively, regular files only)
func ScrubNodeIdentity(rootDir string) error {
	var errs []string

	scrubFile := func(rel string) {
		p := filepath.Join(rootDir, rel)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("remove %s: %v", rel, err))
		}
	}

	truncateFile := func(rel string) {
		p := filepath.Join(rootDir, rel)
		if _, err := os.Stat(p); os.IsNotExist(err) {
			return
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Sprintf("truncate %s: %v", rel, err))
		}
	}

	removeGlob := func(pattern string) {
		matches, _ := filepath.Glob(filepath.Join(rootDir, pattern))
		for _, m := range matches {
			if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
				rel := strings.TrimPrefix(m, rootDir)
				errs = append(errs, fmt.Sprintf("remove %s: %v", rel, err))
			}
		}
	}

	removeDir := func(rel string) {
		p := filepath.Join(rootDir, rel)
		entries, err := os.ReadDir(p)
		if os.IsNotExist(err) {
			return
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("readdir %s: %v", rel, err))
			return
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(p, e.Name())); err != nil {
				errs = append(errs, fmt.Sprintf("remove %s/%s: %v", rel, e.Name(), err))
			}
		}
	}

	// machine-id: remove so systemd regenerates it.
	scrubFile("etc/machine-id")

	// SSH host keys: sshd creates new ones on first start.
	removeGlob("etc/ssh/ssh_host_*")

	// Hostname: truncate so the deployed node can set its own.
	truncateFile("etc/hostname")

	// RHEL/CentOS network scripts (keep lo).
	ifcfgDir := filepath.Join(rootDir, "etc/sysconfig/network-scripts")
	if entries, err := os.ReadDir(ifcfgDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "ifcfg-") && e.Name() != "ifcfg-lo" {
				if err := os.Remove(filepath.Join(ifcfgDir, e.Name())); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Sprintf("remove ifcfg %s: %v", e.Name(), err))
				}
			}
		}
	}

	// NetworkManager connection profiles.
	removeDir("etc/NetworkManager/system-connections")

	// udev persistent-net rules.
	scrubFile("etc/udev/rules.d/70-persistent-net.rules")

	// Bash histories.
	scrubFile("root/.bash_history")
	homeDir := filepath.Join(rootDir, "home")
	if entries, err := os.ReadDir(homeDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				scrubFile(filepath.Join("home", e.Name(), ".bash_history"))
			}
		}
	}

	// /tmp and /var/tmp: remove contents.
	removeDir("tmp")
	removeDir("var/tmp")

	// Logs: truncate all regular files under /var/log recursively.
	logDir := filepath.Join(rootDir, "var/log")
	_ = filepath.WalkDir(logDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if err := os.WriteFile(path, nil, 0o640); err != nil && !os.IsNotExist(err) {
			rel := strings.TrimPrefix(path, rootDir)
			errs = append(errs, fmt.Sprintf("truncate log %s: %v", rel, err))
		}
		return nil
	})

	if len(errs) > 0 {
		return fmt.Errorf("scrub: %s", strings.Join(errs, "; "))
	}
	return nil
}
