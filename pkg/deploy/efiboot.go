package deploy

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// EFIBootEntry represents a parsed efibootmgr boot entry.
type EFIBootEntry struct {
	BootNum string // e.g. "0001"
	Label   string
	Active  bool
}

// FixEFIBoot creates or repairs EFI boot entries for a freshly deployed system.
// It creates a new boot entry pointing to the ESP partition and sets it as the
// first boot option.
//
// Parameters:
//   - disk: the full device path of the target disk, e.g. /dev/nvme0n1
//   - espPartNum: the partition number of the ESP (usually 1), 1-indexed
//   - label: the boot menu label, e.g. "Rocky Linux"
//   - loader: the EFI loader path relative to the ESP, e.g. "\EFI\rocky\grubx64.efi"
func FixEFIBoot(ctx context.Context, disk string, espPartNum int, label, loader string) error {
	if label == "" {
		label = "Linux"
	}
	if loader == "" {
		loader = `\EFI\rocky\grubx64.efi`
	}

	// Remove stale entries with the same label to avoid duplicates.
	if err := removeStaleEntries(ctx, label); err != nil {
		// Non-fatal — proceed even if cleanup fails.
		_ = err
	}

	// Create new boot entry.
	// efibootmgr --create --disk /dev/nvme0n1 --part 1 --label "Linux" --loader '\EFI\...'
	args := []string{
		"--create",
		"--disk", disk,
		"--part", fmt.Sprintf("%d", espPartNum),
		"--label", label,
		"--loader", loader,
	}

	cmd := exec.CommandContext(ctx, "efibootmgr", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("efiboot: create entry: %w\noutput: %s", err, string(out))
	}

	// Set boot order so the new entry is first.
	newNum, err := parseNewBootNum(string(out))
	if err != nil {
		// Cannot determine the new boot number — set order based on existing list.
		return setBootOrderFirst(ctx)
	}

	return setBootEntry(ctx, newNum)
}

// removeStaleEntries deletes existing efibootmgr entries matching label.
func removeStaleEntries(ctx context.Context, label string) error {
	entries, err := listBootEntries(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.EqualFold(e.Label, label) {
			cmd := exec.CommandContext(ctx, "efibootmgr", "--delete-bootnum", "--bootnum", e.BootNum)
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("efiboot: remove %s: %w\noutput: %s", e.BootNum, err, string(out))
			}
		}
	}
	return nil
}

// listBootEntries parses efibootmgr output and returns all entries.
func listBootEntries(ctx context.Context) ([]EFIBootEntry, error) {
	out, err := exec.CommandContext(ctx, "efibootmgr").Output()
	if err != nil {
		return nil, fmt.Errorf("efiboot: list entries: %w", err)
	}

	var entries []EFIBootEntry
	for _, line := range strings.Split(string(out), "\n") {
		// Lines look like: "Boot0001* Rocky Linux" or "Boot0002  Windows"
		if !strings.HasPrefix(line, "Boot") || len(line) < 8 {
			continue
		}
		num := line[4:8]
		active := len(line) > 8 && line[8] == '*'
		label := ""
		if len(line) > 9 {
			label = strings.TrimSpace(line[9:])
		}
		entries = append(entries, EFIBootEntry{
			BootNum: num,
			Label:   label,
			Active:  active,
		})
	}
	return entries, nil
}

// parseNewBootNum extracts the new boot entry number from efibootmgr --create output.
// Output typically contains a line like: "Boot0001* label"
func parseNewBootNum(output string) (string, error) {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Boot") && strings.Contains(line, "*") {
			if len(line) >= 8 {
				return line[4:8], nil
			}
		}
	}
	return "", fmt.Errorf("efiboot: cannot parse new boot number from output")
}

// setBootEntry sets the specified boot entry as first in the boot order and activates it.
func setBootEntry(ctx context.Context, bootNum string) error {
	// Activate the entry.
	activateCmd := exec.CommandContext(ctx, "efibootmgr", "--bootnum", bootNum, "--active")
	if out, err := activateCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("efiboot: activate %s: %w\noutput: %s", bootNum, err, string(out))
	}

	// Set it as first in the boot order.
	orderCmd := exec.CommandContext(ctx, "efibootmgr", "--bootnext", bootNum)
	if out, err := orderCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("efiboot: set boot order %s: %w\noutput: %s", bootNum, err, string(out))
	}
	return nil
}

// setBootOrderFirst reads the current boot order and makes the first active entry
// the next boot target. Used as fallback when new entry number is unknown.
func setBootOrderFirst(ctx context.Context) error {
	entries, err := listBootEntries(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Active {
			return setBootEntry(ctx, e.BootNum)
		}
	}
	return fmt.Errorf("efiboot: no active boot entries found")
}
