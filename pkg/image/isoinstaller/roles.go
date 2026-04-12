package isoinstaller

import "sort"

// Role defines an HPC node role with curated package and service lists keyed
// by distro. Roles are the first-pass implementation: static, code-defined.
// A follow-up will make them DB-backed so admins can define custom roles via
// the API/UI without a server redeploy.
type Role struct {
	// ID is the machine-readable identifier used in API requests, e.g. "compute".
	ID string `json:"id"`

	// Name is the human-readable display name shown in the UI.
	Name string `json:"name"`

	// Description explains what this role installs and why.
	Description string `json:"description"`

	// Notes carries optional caveats, e.g. "CUDA is installed via vendor repo in %post".
	Notes string `json:"notes,omitempty"`

	// Packages maps distro → list of package names to install.
	// Packages that require a vendor repo (CUDA, Lustre, BeeGFS) are handled
	// in the generated %post section rather than the %packages stanza.
	Packages map[Distro][]string `json:"-"`

	// Services maps distro → list of systemd unit names to enable.
	Services map[Distro][]string `json:"-"`
}

// HPCRoles is the ordered list of built-in HPC node role presets.
// Displayed in this order in the UI role picker.
var HPCRoles = []Role{
	{
		ID:          "head-node",
		Name:        "Head Node / Login Node",
		Description: "SLURM controller, LDAP server, login shell, NFS exports, management tooling",
		Packages: map[Distro][]string{
			DistroRocky: {
				"slurm", "slurm-slurmctld", "slurm-slurmdbd", "mariadb-server",
				"openldap-servers", "openldap-clients", "sssd", "sssd-ldap",
				"nfs-utils", "rpcbind",
				"bash-completion", "vim-enhanced", "tmux", "screen", "htop", "tree",
				"rsync", "wget", "curl", "git",
				"firewalld", "fail2ban",
				"chrony", "dnf-automatic",
			},
			DistroAlmaLinux: {
				"slurm", "slurm-slurmctld", "slurm-slurmdbd", "mariadb-server",
				"openldap-servers", "openldap-clients", "sssd", "sssd-ldap",
				"nfs-utils", "rpcbind",
				"bash-completion", "vim-enhanced", "tmux", "screen", "htop", "tree",
				"rsync", "wget", "curl", "git",
				"firewalld", "fail2ban",
				"chrony", "dnf-automatic",
			},
			DistroUbuntu: {
				"slurm-wlm", "slurmctld", "slurmdbd", "mariadb-server",
				"slapd", "ldap-utils", "sssd", "sssd-ldap",
				"nfs-kernel-server",
				"vim", "tmux", "screen", "htop", "tree",
				"rsync", "wget", "curl", "git",
			},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd", "chronyd", "slurmctld"},
			DistroAlmaLinux: {"sshd", "chronyd", "slurmctld"},
			DistroUbuntu:    {"ssh", "slurmctld"},
		},
	},
	{
		ID:          "compute",
		Name:        "CPU Compute Node",
		Description: "SLURM worker, LDAP client, NFS client, MPI, Lmod, scientific computing base tooling",
		Packages: map[Distro][]string{
			DistroRocky: {
				"slurm", "slurm-slurmd", "slurm-pmi",
				"sssd", "sssd-ldap", "oddjob-mkhomedir",
				"nfs-utils",
				"openmpi", "openmpi-devel",
				"environment-modules",
				"hwloc", "hwloc-devel", "numactl",
				"gcc", "gcc-c++", "gcc-gfortran", "make", "cmake",
				"bash-completion", "vim-minimal", "htop", "tmux",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
			DistroAlmaLinux: {
				"slurm", "slurm-slurmd", "slurm-pmi",
				"sssd", "sssd-ldap", "oddjob-mkhomedir",
				"nfs-utils",
				"openmpi", "openmpi-devel",
				"environment-modules",
				"hwloc", "hwloc-devel", "numactl",
				"gcc", "gcc-c++", "gcc-gfortran", "make", "cmake",
				"bash-completion", "vim-minimal", "htop", "tmux",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
			DistroUbuntu: {
				"slurmd", "slurm-client",
				"sssd", "sssd-ldap",
				"nfs-common",
				"openmpi-bin", "libopenmpi-dev",
				"environment-modules",
				"libhwloc-dev", "numactl",
				"build-essential", "gfortran", "cmake",
				"vim", "htop", "tmux",
				"rsync", "wget", "curl", "git",
			},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd", "chronyd", "slurmd", "sssd"},
			DistroAlmaLinux: {"sshd", "chronyd", "slurmd", "sssd"},
			DistroUbuntu:    {"ssh", "slurmd", "sssd"},
		},
	},
	{
		ID:          "gpu-compute",
		Name:        "GPU Compute Node",
		Description: "Everything in CPU compute plus CUDA toolkit and NVIDIA drivers via vendor repo",
		Notes:       "CUDA toolkit and NVIDIA drivers are installed via the NVIDIA CUDA repo in %post. kernel-devel must match the running kernel at driver build time.",
		Packages: map[Distro][]string{
			DistroRocky: {
				"slurm", "slurm-slurmd", "slurm-pmi",
				"sssd", "sssd-ldap", "oddjob-mkhomedir",
				"nfs-utils", "openmpi", "openmpi-devel",
				"environment-modules",
				"hwloc", "numactl",
				"gcc", "gcc-c++", "gcc-gfortran", "make", "cmake",
				"kernel-devel", "kernel-headers", "dkms",
				"pciutils", "lshw",
				"bash-completion", "vim-minimal", "htop", "tmux",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
			DistroAlmaLinux: {
				"slurm", "slurm-slurmd", "slurm-pmi",
				"sssd", "sssd-ldap", "oddjob-mkhomedir",
				"nfs-utils", "openmpi", "openmpi-devel",
				"environment-modules",
				"hwloc", "numactl",
				"gcc", "gcc-c++", "gcc-gfortran", "make", "cmake",
				"kernel-devel", "kernel-headers", "dkms",
				"pciutils", "lshw",
				"bash-completion", "vim-minimal", "htop", "tmux",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
			DistroUbuntu: {
				"slurmd", "slurm-client",
				"sssd", "sssd-ldap",
				"nfs-common", "openmpi-bin", "libopenmpi-dev",
				"environment-modules",
				"libhwloc-dev", "numactl",
				"build-essential", "gfortran", "cmake",
				"linux-headers-generic", "dkms",
				"pciutils", "lshw",
				"vim", "htop", "tmux",
				"rsync", "wget", "curl", "git",
			},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd", "chronyd", "slurmd", "sssd"},
			DistroAlmaLinux: {"sshd", "chronyd", "slurmd", "sssd"},
			DistroUbuntu:    {"ssh", "slurmd", "sssd"},
		},
	},
	{
		ID:          "storage",
		Name:        "Storage Node",
		Description: "NFS server, Lustre OSS/MDS, BeeGFS storage daemon, ZFS, XFS utilities",
		Notes:       "Lustre and BeeGFS are installed via vendor repos added in %post. ZFS requires the zfsrepo.rpm repo.",
		Packages: map[Distro][]string{
			DistroRocky: {
				"nfs-utils", "rpcbind", "samba",
				"zfs", "zfs-dkms",
				"xfsprogs", "xfsdump",
				"smartmontools", "lvm2", "mdadm",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
			DistroAlmaLinux: {
				"nfs-utils", "rpcbind", "samba",
				"zfs", "zfs-dkms",
				"xfsprogs", "xfsdump",
				"smartmontools", "lvm2", "mdadm",
				"rsync", "wget", "curl", "git",
				"chrony",
			},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd", "chronyd", "nfs-server", "rpcbind", "smartd"},
			DistroAlmaLinux: {"sshd", "chronyd", "nfs-server", "rpcbind", "smartd"},
		},
	},
	{
		ID:          "management",
		Name:        "Management / Monitoring",
		Description: "Prometheus, Grafana, Alertmanager, DNS, DHCP, log aggregation",
		Packages: map[Distro][]string{
			DistroRocky: {
				"prometheus", "prometheus-node-exporter",
				"grafana", "alertmanager",
				"bind", "bind-utils", "dhcp-server",
				"rsyslog", "logrotate",
				"firewalld", "fail2ban",
				"chrony",
				"vim-enhanced", "htop", "tmux",
				"rsync", "wget", "curl", "git",
			},
			DistroAlmaLinux: {
				"prometheus", "prometheus-node-exporter",
				"grafana", "alertmanager",
				"bind", "bind-utils", "dhcp-server",
				"rsyslog", "logrotate",
				"firewalld", "fail2ban",
				"chrony",
				"vim-enhanced", "htop", "tmux",
				"rsync", "wget", "curl", "git",
			},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd", "chronyd", "prometheus", "grafana-server"},
			DistroAlmaLinux: {"sshd", "chronyd", "prometheus", "grafana-server"},
		},
	},
	{
		ID:          "minimal",
		Name:        "Minimal Base",
		Description: "Bare minimum install — no HPC packages. Add what you need via the chroot shell.",
		Packages: map[Distro][]string{
			DistroRocky:     {"openssh-server", "vim-minimal", "curl", "wget"},
			DistroAlmaLinux: {"openssh-server", "vim-minimal", "curl", "wget"},
			DistroUbuntu:    {"openssh-server", "vim", "curl", "wget"},
		},
		Services: map[Distro][]string{
			DistroRocky:     {"sshd"},
			DistroAlmaLinux: {"sshd"},
			DistroUbuntu:    {"ssh"},
		},
	},
}

// RoleByID returns the Role with the given ID, or (Role{}, false) if not found.
func RoleByID(id string) (Role, bool) {
	for _, r := range HPCRoles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

// MergeRoles combines packages and services from the named roles for a given
// distro. Packages and services are deduplicated; order is deterministic.
// Unknown role IDs are silently ignored (callers validate separately).
func MergeRoles(roleIDs []string, distro Distro) (packages []string, services []string) {
	pkgSet := map[string]struct{}{}
	svcSet := map[string]struct{}{}

	for _, id := range roleIDs {
		role, ok := RoleByID(id)
		if !ok {
			continue
		}
		for _, pkg := range role.Packages[distro] {
			pkgSet[pkg] = struct{}{}
		}
		for _, svc := range role.Services[distro] {
			svcSet[svc] = struct{}{}
		}
	}

	for pkg := range pkgSet {
		packages = append(packages, pkg)
	}
	for svc := range svcSet {
		services = append(services, svc)
	}

	sort.Strings(packages)
	sort.Strings(services)
	return packages, services
}

// hasRole returns true when roleIDs contains the given id.
func hasRole(roleIDs []string, id string) bool {
	for _, rid := range roleIDs {
		if rid == id {
			return true
		}
	}
	return false
}
