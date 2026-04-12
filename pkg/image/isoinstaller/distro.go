// Package isoinstaller runs an OS installer ISO inside a temporary QEMU VM,
// captures the installed root filesystem, and returns it ready for use as a
// clonr BaseImage. Think Packer, but purpose-built and embedded in the server.
package isoinstaller

import (
	"os"
	"os/exec"
	"strings"
)

// Distro identifies the Linux distribution family of an installer ISO.
// The value is used to select the correct automated-install config format
// (kickstart, autoinstall, preseed, AutoYaST, or answers file).
type Distro string

const (
	DistroRocky     Distro = "rocky"
	DistroAlmaLinux Distro = "almalinux"
	DistroCentOS    Distro = "centos"
	DistroRHEL      Distro = "rhel"
	DistroUbuntu    Distro = "ubuntu"
	DistroDebian    Distro = "debian"
	DistroSUSE      Distro = "suse"
	DistroAlpine    Distro = "alpine"
	DistroUnknown   Distro = "unknown"
)

// AutoInstallFormat describes which automated-install config format the distro uses.
type AutoInstallFormat string

const (
	FormatKickstart  AutoInstallFormat = "kickstart"  // RHEL family: Rocky, Alma, CentOS, RHEL
	FormatAutoInstall AutoInstallFormat = "autoinstall" // Ubuntu 20.04+
	FormatPreseed    AutoInstallFormat = "preseed"    // Debian
	FormatAutoYaST   AutoInstallFormat = "autoyast"   // SUSE / openSUSE
	FormatAnswers    AutoInstallFormat = "answers"    // Alpine
)

// Format returns the automated-install format for a given distro.
func (d Distro) Format() AutoInstallFormat {
	switch d {
	case DistroRocky, DistroAlmaLinux, DistroCentOS, DistroRHEL:
		return FormatKickstart
	case DistroUbuntu:
		return FormatAutoInstall
	case DistroDebian:
		return FormatPreseed
	case DistroSUSE:
		return FormatAutoYaST
	case DistroAlpine:
		return FormatAnswers
	default:
		return FormatKickstart // best guess for unknown
	}
}

// FamilyName returns a short human-readable label for the distro family.
func (d Distro) FamilyName() string {
	switch d {
	case DistroRocky:
		return "Rocky Linux"
	case DistroAlmaLinux:
		return "AlmaLinux"
	case DistroCentOS:
		return "CentOS"
	case DistroRHEL:
		return "RHEL"
	case DistroUbuntu:
		return "Ubuntu"
	case DistroDebian:
		return "Debian"
	case DistroSUSE:
		return "SUSE / openSUSE"
	case DistroAlpine:
		return "Alpine Linux"
	default:
		return "Unknown Linux"
	}
}

// DetectDistro attempts to identify the Linux distribution from the ISO URL
// and, if the ISO is already downloaded, from the ISO volume label.
//
// Detection order:
//  1. URL hostname/path pattern matching (fast, no disk access).
//  2. ISO volume label via isoinfo/blkid (requires isoPath to be non-empty
//     and the file to exist — used as a fallback when URL is ambiguous).
//
// Returns DistroUnknown when detection fails; callers should prompt the user
// to supply the distro explicitly rather than failing outright.
func DetectDistro(isoURL string, isoPath string) (Distro, error) {
	lower := strings.ToLower(isoURL)

	// ── URL-based detection ───────────────────────────────────────────────
	switch {
	case containsAny(lower, "rockylinux.org", "download.rockylinux.org"):
		return DistroRocky, nil
	case containsAny(lower, "almalinux.org"):
		return DistroAlmaLinux, nil
	case containsAny(lower, "centos.org", "mirror.centos.org"):
		return DistroCentOS, nil
	case containsAny(lower, "redhat.com", "rhel"):
		return DistroRHEL, nil
	case containsAny(lower, "ubuntu.com", "releases.ubuntu.com", "cdimage.ubuntu.com", "old-releases.ubuntu.com"):
		return DistroUbuntu, nil
	case containsAny(lower, "debian.org", "cdimage.debian.org", "deb.debian.org"):
		return DistroDebian, nil
	case containsAny(lower, "opensuse.org", "suse.com", "download.opensuse.org"):
		return DistroSUSE, nil
	case containsAny(lower, "alpinelinux.org", "dl-cdn.alpinelinux.org"):
		return DistroAlpine, nil
	}

	// ── Filename heuristics (for ISOs with ambiguous hosts) ───────────────
	base := strings.ToLower(isoURL)
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	switch {
	case strings.HasPrefix(base, "rocky-"):
		return DistroRocky, nil
	case strings.HasPrefix(base, "almalinux-"):
		return DistroAlmaLinux, nil
	case strings.HasPrefix(base, "centos-"):
		return DistroCentOS, nil
	case strings.HasPrefix(base, "ubuntu-"):
		return DistroUbuntu, nil
	case strings.HasPrefix(base, "debian-"):
		return DistroDebian, nil
	case strings.HasPrefix(base, "opensuse-"), strings.HasPrefix(base, "sle-"):
		return DistroSUSE, nil
	case strings.HasPrefix(base, "alpine-"):
		return DistroAlpine, nil
	}

	// ── ISO volume label fallback ─────────────────────────────────────────
	if isoPath != "" {
		if d, ok := detectFromVolumeLabel(isoPath); ok {
			return d, nil
		}
	}

	return DistroUnknown, nil
}

// detectFromVolumeLabel reads the ISO volume label using isoinfo or blkid
// and maps it to a known Distro. Returns (DistroUnknown, false) on failure.
func detectFromVolumeLabel(isoPath string) (Distro, bool) {
	label := ""

	// Try isoinfo first (part of genisoimage / cdrtools).
	if path, err := exec.LookPath("isoinfo"); err == nil {
		out, err := exec.Command(path, "-d", "-i", isoPath).CombinedOutput()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "Volume id:") {
					label = strings.TrimSpace(strings.TrimPrefix(line, "Volume id:"))
					break
				}
			}
		}
	}

	// Fall back to blkid.
	if label == "" {
		if path, err := exec.LookPath("blkid"); err == nil {
			out, err := exec.Command(path, "-o", "value", "-s", "LABEL", isoPath).CombinedOutput()
			if err == nil {
				label = strings.TrimSpace(string(out))
			}
		}
	}

	if label == "" {
		return DistroUnknown, false
	}

	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "rocky"):
		return DistroRocky, true
	case strings.Contains(lower, "alma"):
		return DistroAlmaLinux, true
	case strings.Contains(lower, "centos"):
		return DistroCentOS, true
	case strings.Contains(lower, "rhel"), strings.Contains(lower, "red hat"):
		return DistroRHEL, true
	case strings.Contains(lower, "ubuntu"):
		return DistroUbuntu, true
	case strings.Contains(lower, "debian"):
		return DistroDebian, true
	case strings.Contains(lower, "suse"), strings.Contains(lower, "opensuse"):
		return DistroSUSE, true
	case strings.Contains(lower, "alpine"):
		return DistroAlpine, true
	}
	return DistroUnknown, false
}

// containsAny returns true if s contains any of the provided substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// qemuCandidates lists the known binary paths for qemu-system-x86_64 across
// distributions. RHEL/Rocky/Alma ship it at /usr/libexec/qemu-kvm; Debian/Ubuntu
// at /usr/bin/qemu-system-x86_64.
var qemuCandidates = []string{
	"qemu-system-x86_64",    // Debian/Ubuntu/Arch (in $PATH)
	"/usr/bin/qemu-system-x86_64",
	"/usr/libexec/qemu-kvm", // RHEL/Rocky/AlmaLinux/CentOS
	"/usr/bin/qemu-kvm",
}

// FindQEMU returns the path to the qemu-system-x86_64 (or qemu-kvm) binary.
// Searches $PATH first, then well-known distribution-specific locations.
// Returns ("", false) when QEMU is not available.
func FindQEMU() (string, bool) {
	for _, candidate := range qemuCandidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, true
		}
		// LookPath only resolves names in $PATH; for absolute paths we stat directly.
		if strings.HasPrefix(candidate, "/") {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, true
			}
		}
	}
	return "", false
}

// CheckDependencies returns a list of missing host binaries required to run
// the ISO installer. Callers should surface these as actionable errors before
// attempting a build rather than failing deep in the goroutine.
func CheckDependencies() []string {
	var missing []string

	// qemu: check via FindQEMU which handles RHEL vs Debian binary paths.
	if _, ok := FindQEMU(); !ok {
		missing = append(missing, "qemu-system-x86_64 (or /usr/libexec/qemu-kvm on RHEL/Rocky)")
	}

	// qemu-img is always in $PATH even on RHEL.
	if _, err := exec.LookPath("qemu-img"); err != nil {
		missing = append(missing, "qemu-img")
	}

	// Either genisoimage or xorriso must be present (for building the seed ISO).
	geiso := false
	for _, bin := range []string{"genisoimage", "xorriso"} {
		if _, err := exec.LookPath(bin); err == nil {
			geiso = true
			break
		}
	}
	if !geiso {
		missing = append(missing, "genisoimage or xorriso")
	}

	return missing
}

// HasKVM returns true when /dev/kvm is accessible. When false the VM will
// fall back to software emulation (TCG), which is 10-20x slower.
func HasKVM() bool {
	f, err := os.Open("/dev/kvm")
	if err != nil {
		return false
	}
	f.Close()
	return true
}
