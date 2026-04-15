# Admin Image Customization Requirements for ADR-0009

**Date:** 2026-04-13
**Author:** Gilfoyle (Infra/Platform), input to ADR-0009 "Content-only images"
**Status:** Draft — for Richard's review before ADR-0009 is authored

---

## Purpose

This document enumerates every class of customization an HPC cluster administrator needs to apply to a base OS image before it can be deployed to a production fleet. It is written from the perspective of an HPC sysadmin managing a heterogeneous cluster of compute, login, storage, and GPU nodes running Rocky Linux 8/9 or similar RHEL derivatives.

The ADR-0009 content-only image model — where images carry only rootfs content and deploy-time regenerates all bootloader/mount artifacts — must support every category below without regression. Where the image model alone cannot address a requirement, the boundary between image, node_config, and group template must be stated explicitly.

---

## 1. Package Set

### What admins actually need

A base Rocky 9 minimal image ships approximately 300 packages. A production HPC node typically has 600–900. The delta includes:

- **MPI stacks:** OpenMPI, MVAPICH2, Intel MPI (as RPMs or module-managed tarballs under `/opt/`). Multiple versions coexist managed by environment modules or Lmod.
- **Slurm:** `slurm`, `slurm-slurmd`, `slurm-contribs`, `munge`, `munge-libs`. Version must match the Slurm controller exactly — a one minor-version mismatch causes job submission failure.
- **Infiniband/RDMA stack:** Either the distro `rdma-core` / `libibverbs` / `librdmacm` stack or the vendor MLNX_OFED tarball (which replaces distro IB packages and must be installed via `mlnxofedinstall`, not dnf). These conflict; the choice is per-image.
- **NVIDIA drivers:** For GPU nodes. Not in any standard repo. Installed from NVIDIA's CUDA repo RPMs or via runfile. Requires `kernel-devel` matching the pinned kernel. DKMS rebuilds the module on each kernel update.
- **Lustre client:** `lustre-client` RPMs from the Lustre project repo, pinned to match the Lustre server version. The client version must match the server exactly or mounts fail silently or produce data corruption.
- **BeeGFS / GPFS clients:** Similar story — vendor RPMs, version-pinned.
- **Monitoring agents:** `prometheus-node-exporter`, `collectd`, `ganglia-gmond` depending on site tooling.
- **Management tooling:** `pdsh`, `clustershell`, `ipmitool`, `freeipmi`, `stress-ng`, `numactl`, `hwloc`, `likwid`.
- **In-house RPMs:** Built and served from a local Pulp or Nexus repo. Often include: site license managers (FlexLM/RLM), prolog/epilog helper binaries, custom health check scripts packaged as RPMs.
- **Removals:** `NetworkManager` replaced by `systemd-networkd` on many sites; `firewalld` removed on compute nodes (cluster firewalling is done at the network layer); `postfix` removed if no local MTA needed.

### Why it matters

If clonr images cannot express a package manifest, admins will hand-install packages into the rootfs via the chroot shell and never track what changed. The image becomes a snowflake. Reproducibility is lost. This is the primary source of "image worked last month, broken now" failures.

Dependency resolution must happen at image-build time, not at deploy time, because compute nodes often have no internet access. All RPMs must be pre-installed in the rootfs.

### Priority

MUST for v1.0.

### Suggested mechanism

A declarative **package manifest** embedded in the image definition: a list of repos (baseurl + GPG key) and a list of packages with optional pinned versions (`slurm-23.11.4`). The image build process runs `dnf install` inside the rootfs at build time. For packages that cannot be installed via dnf (MLNX_OFED, NVIDIA runfile), a **post-install script hook** runs inside the chroot after the dnf pass completes.

---

## 2. Configuration Files

### What admins actually need

The following `/etc` files require site-specific content that is not in any upstream package and must be present before the node can join the cluster:

**Authentication and directory:**
- `/etc/sssd/sssd.conf` — LDAP/Kerberos realm, search base, bind DN. Different per domain. Mode must be 0600 or sssd refuses to start.
- `/etc/krb5.conf` — KDC addresses, default realm, realm-to-domain mappings.
- `/etc/nsswitch.conf` — ordering of `passwd`, `group`, `shadow` sources (files, sss). Wrong ordering means root can lose login access.
- `/etc/pam.d/system-auth`, `/etc/pam.d/password-auth` — SSSD integration, mkhomedir, account expiry. These are overwritten by `authselect` if not locked in place.

**Name resolution and time:**
- `/etc/resolv.conf` or `/etc/NetworkManager/conf.d/` dispatch scripts — cluster DNS. On systemd-networkd nodes, this is a `.network` file. On NM nodes, it is managed via DHCP or static config. The wrong DNS means Kerberos fails, LDAP fails, NFS hostname resolution fails.
- `/etc/chrony.conf` — cluster NTP servers. Slurm requires clock skew < 5 seconds across all nodes; Kerberos requires < 5 minutes. Without correct NTP config, Slurm jobs fail with `Authentication failure`.

**Job scheduler:**
- `/etc/slurm/slurm.conf` — cluster topology, partition definitions, node specifications. This file is often identical across all compute nodes but different between compute, login, and storage roles. On large clusters it is symlinked from a shared filesystem (`/software/slurm/etc/slurm.conf`) — but the symlink itself must be installed.
- `/etc/slurm/gres.conf` — GPU/accelerator resource definitions. Per-node (GPU count, topology), but often templated per node type.
- `/etc/munge/munge.key` — shared secret for Slurm auth. This is a **per-cluster secret**, not per-node, but must not be baked into the image (see §12 on secrets).

**Sudo and privilege:**
- `/etc/sudoers.d/` — site sudo rules. Typically: cluster admins get NOPASSWD ALL; users get nothing; Slurm prolog/epilog binaries get specific targeted rules.

**SSH:**
- `/etc/ssh/sshd_config.d/` — site overrides: `PermitRootLogin yes` (many HPC sites allow root SSH from the admin VLAN), `AllowGroups`, `AuthorizedKeysCommand` for LDAP-backed SSH keys, `MaxSessions` tuning.

**Prolog/epilog:**
- `/etc/slurm/prolog.d/`, `/etc/slurm/epilog.d/` — scripts that run before/after each job. These set up GPU isolation, bind-mount scratch directories, collect accounting data, and clean up `/tmp`. They are part of the OS image, not per-node.

### Why it matters

A node without correct sssd.conf cannot authenticate users. A node without correct chrony.conf will have Slurm jobs fail authentication. A node without correct sshd_config will either deny admin access or expose the cluster. These files must be present and correct at first boot — there is no "fix it later" for a node that can't authenticate.

### Per-image vs per-node boundary

**Per-image config** (same for all nodes of a given role): sssd.conf domain block, krb5.conf, nsswitch.conf, pam.d/, sshd_config.d/, sudoers.d/, chrony.conf, slurm.conf (for single-partition clusters). These belong in the image as **rootfs overlay files** — files placed at exact paths with exact permissions.

**Per-node config** (varies per node): gres.conf GPU topology, any config that references the node's hostname or IP explicitly. These must be injected at deploy time via `node_config`, not baked into the image.

### Priority

MUST for v1.0.

### Suggested mechanism

**Rootfs overlay directory** — a directory tree in the image definition that is merged onto the rootfs at build time. Files are placed at their exact target path with specified uid:gid:mode. This is the cleanest mechanism for config files: declarative, diffable, auditable.

---

## 3. Users and Authentication

### What admins actually need

**System accounts with fixed UIDs/GIDs:**
HPC clusters use shared filesystems (NFS, Lustre, GPFS) where file ownership is by UID/GID number, not name. If `uid=1001` maps to `slurm` on the storage server and `daemon` on a compute node, every Slurm job produces files with wrong ownership. The classic failure mode: user data appears owned by `slurm` on compute nodes, unreadable on login nodes.

Required accounts (examples):
- `munge` (uid typically 992 or similar — must match cluster-wide)
- `slurm` (uid must match the Slurm controller's slurm uid exactly)
- Application service accounts: `namd`, `vasp`, `lammps` if these run as dedicated users

**Root SSH keys:**
Admin SSH access to nodes requires root's `authorized_keys` to be populated. The public keys change when admins rotate credentials. This must NOT be baked into the image because:
1. A compromised image leaks which public keys were active at image build time (minor)
2. More importantly, key rotation requires a full image rebuild and redeploy — unacceptable operationally

Correct model: root `authorized_keys` is injected at deploy time as a per-site or per-group secret.

**LDAP/SSSD bind credentials:**
sssd.conf references a bind DN and bind password for LDAP queries (if using simple bind). The password MUST NOT be in the image. See §12.

**Local user creation:**
Most HPC sites do NOT want local `useradd` entries for regular users — they come from LDAP. But system accounts (munge, slurm, monitoring agent users) must be created with specific UIDs before any package that owns those UIDs is installed. RPMs create the users on install — if the UID is already taken by a different user, the RPM install fails or creates the account with a random UID.

### Why it matters

UID/GID mismatches on shared filesystems are silent until a user reports wrong ownership on their output files. By then, months of job output may have wrong permissions. This is a MUST-fix-at-image-build-time concern.

### Priority

MUST for v1.0 (fixed UID/GID system accounts, root authorized_keys injection hook). SHOULD for v1.1 (explicit account creation directives in the image manifest).

### Suggested mechanism

**Image manifest:** Declare required system accounts with explicit uid/gid in the image definition. Build process creates them via `useradd`/`groupadd` in the chroot before package installation.

**Deploy-time injection:** Root `authorized_keys` and SSSD bind password delivered via the node-scoped secrets injection channel (see §12), not baked into the image.

---

## 4. Kernel and Kernel Modules

### What admins actually need

**Kernel version pinning:**
This is non-negotiable on most HPC sites. Admins pin the kernel because:
- NVIDIA drivers are built against a specific kernel ABI (`nvidia-dkms` or a prebuilt `.ko`). An unplanned kernel update breaks the driver until a new build is produced.
- MLNX_OFED builds `.ko` files for the running kernel. It must be reinstalled for every new kernel.
- Lustre client `.ko` is built for a specific kernel. Version mismatch = mount fails.
- Some MPI tuning (`mpirun -mca btl openib`) depends on verbs ABI stability.

The image must specify exactly one kernel package and version to install, and `dnf` must be configured to never update it automatically.

**Module blacklist:**
- `nouveau` must be blacklisted on GPU nodes — it conflicts with the proprietary NVIDIA driver and causes boot failure or GPU unavailability.
- `bluetooth`, `rfkill` blacklisted on compute nodes (no hardware, no reason to load, reduces attack surface).
- Occasionally `amdgpu` or `radeon` on GPU nodes using NVIDIA-only configs.

**Module parameters:**
- `options nvidia NVreg_OpenRmEnableUnsupportedGpus=1` for some GPU generations.
- `options mlx5_core num_vfs=4` for SR-IOV Infiniband on high-density nodes.
- These go in `/etc/modprobe.d/` files.

**DKMS:**
For NVIDIA drivers and MLNX_OFED installed as DKMS packages, the kernel-devel headers must be installed in the rootfs at the pinned version, and the DKMS build must run inside the chroot at image build time. The resulting `.ko` files for the pinned kernel must be present in the rootfs before the image is sealed.

**Initramfs:**
Nodes that mount Lustre or NFS from initramfs (for diskless or thin deployments) need the relevant drivers in the initramfs. For clonr's disk-based deployment model, initramfs must include: any storage controller drivers needed for the target disk (nvme, megaraid_sas, mpt3sas), md/LVM if applicable, and the network driver for the management NIC if different from the default.

With the ADR-0009 content-only model, the initramfs in the image rootfs is generated by clonr's deploy-time `dracut` run against the target's actual hardware, so the image's `/boot/initramfs-*.img` is irrelevant — but the kernel modules in `/lib/modules/<kver>/` must be correct and complete.

### Why it matters

A kernel update on a compute node without coordinating the NVIDIA driver or Lustre client update takes down every GPU job or every filesystem-dependent job on that node. In production, this means job failures counted in core-hours. Sites have had scheduled maintenance windows cancelled because an uncoordinated kernel update broke 500 nodes simultaneously.

### Priority

MUST for v1.0 (kernel version pinning, module blacklist, module parameters). MUST for v1.0 (DKMS build inside chroot at image build time). SHOULD for v1.1 (initramfs driver inclusion directives for diskless extensions).

### Suggested mechanism

**Image manifest:** `kernel.version: "5.14.0-427.13.1.el9_4.x86_64"` — the build process installs exactly this kernel and sets `exclude=kernel*` in the dnf config to prevent updates. Module parameters declared as a list of `/etc/modprobe.d/` file contents in the overlay. DKMS builds triggered as a post-install script hook after kernel and dkms packages are installed.

---

## 5. Boot and Kernel Cmdline

### What admins actually need

The default kernel cmdline for a node image is not the same as the generic distro default. Production HPC cmdlines look like:

```
BOOT_IMAGE=/vmlinuz-5.14.0-427.13.1.el9_4.x86_64
root=UUID=<deploy-time>
ro crashkernel=auto
console=tty0 console=ttyS0,115200
rd.lvm.lv=rhel/root rd.lvm.lv=rhel/swap  # if LVM, generated at deploy time
hugepagesz=1G hugepages=64               # for MPI jobs that need 1G pages
transparent_hugepage=never               # required by some databases and MPI benchmarks
isolcpus=1-63                            # isolate cores for NOHZ_FULL on realtime nodes
nohz_full=1-63 rcu_nocbs=1-63           # associated NOHZ params
mitigations=off                          # performance clusters; security clusters keep this on
iommu=pt intel_iommu=on                  # GPU passthrough or SRIOV
numa_balancing=disable                   # common on MPI nodes; auto-NUMA migration kills performance
elevator=none                            # NVMe needs no I/O scheduler
```

These are **per-image** defaults, not per-node. A compute node image always gets hugepages and NOHZ. A login node image never gets isolcpus. A GPU node image always gets iommu=pt. A storage node image might get `elevator=deadline`.

Note: the `root=UUID=<>` and any LVM/RAID arguments are **deploy-time generated** per ADR-0009 and must NOT be in the image's cmdline template. The image provides only the role-specific extras.

### Why it matters

`hugepages=0` on a node where all jobs request 1G huge pages means every job fails at memory allocation. `mitigations=off` is a deliberate performance choice that the admin must opt into per image role — it cannot be a default. Wrong isolcpus breaks realtime workloads silently: the kernel still runs the job, but OS jitter causes timing anomalies that look like application bugs.

### Priority

MUST for v1.0.

### Suggested mechanism

**Image manifest:** `cmdline_extras: "hugepagesz=1G hugepages=64 transparent_hugepage=never mitigations=off"` — a string appended to the deploy-time-generated cmdline. Deploy-time assembly: `<generated root= and initrd= args> <image cmdline_extras>`. This is clean because ADR-0009's model already generates the cmdline at deploy time; it just needs a hook to append image-provided extras.

---

## 6. Services and Systemd

### What admins actually need

The set of enabled/disabled/masked services differs by node role:

**Compute nodes:**
- Enable: `slurmd`, `munge`, `sshd`, `chronyd`, `sssd`, `prometheus-node-exporter`, `clonr-verify-boot`
- Disable: `firewalld`, `NetworkManager` (if using systemd-networkd), `cups`, `avahi-daemon`, `bluetooth`, `postfix`
- Mask: `firewalld`, `iptables`, `ip6tables` (compute nodes rely on network-level firewalling; local firewall adds overhead and complexity)
- Custom target: Many sites run compute nodes at a stripped `multi-user.target` without `graphical.target` or `display-manager`

**Login nodes:**
- Enable: everything above PLUS `sssd-kcm`, `autofs`, `nfs-client.target`
- Keep enabled: `NetworkManager` (users need network tools)
- Do NOT mask `firewalld` — login nodes face users

**GPU nodes:**
- Enable: `nvidia-persistenced` (keeps GPU context alive between jobs, required for CUDA MPS)
- Mask: `nouveau` (handled via module blacklist, but belt-and-suspenders)

**Storage/management nodes:**
- Completely different service set: NFS server, Lustre server, BeeGFS management daemon, etc.

### Why it matters

A node that boots with `firewalld` active and default-deny rules will block all MPI traffic and all Slurm connections. A node without `munge` enabled will fail all Slurm job authentication. A compute node running NetworkManager with random DHCP renewals at job runtime causes MPI rank disconnects.

Systemd unit masking (not just disabling) is important: packages that install a service will `systemctl enable` it on package install if it is only disabled. Masking prevents this.

### Priority

MUST for v1.0.

### Suggested mechanism

**Image manifest:** Three lists: `services.enable`, `services.disable`, `services.mask`. Applied at image build time inside the chroot via `systemctl enable/disable/mask`. These become part of the image's rootfs state (symlinks in `/etc/systemd/system/` and `/etc/systemd/system/*.wants/`).

---

## 7. Filesystem Layout and Mounts

### What admins actually need

Beyond the OS disk (clonr's responsibility): every HPC node mounts shared filesystems. These vary by site, by role, and sometimes by rack.

**Typical mount landscape:**
- `/home` — NFS from home file server, `auth_sys` or Kerberos, all nodes
- `/scratch` — Lustre parallel filesystem, compute nodes only (login nodes may get a different Lustre path)
- `/software` — NFS or GPFS, read-only, all nodes, contains Lmod module tree and application installs
- `/opt/tools` — NFS, admin tools, all nodes
- `/dev/shm` — always present, but sometimes size-limited via tmpfs options
- `/tmp` — often replaced with a node-local `tmpfs` of specific size (prevents jobs from filling root disk)

**fstab vs systemd mount units vs autofs:**
- Static mounts that must be present before Slurm starts: fstab or systemd `.mount` units (for dependency ordering via `RequiresMountsFor=`).
- Home directories: autofs via `auto.home` map for large user bases; avoids mounting 10k home directories on every node.
- Project directories: autofs via `auto.project` map.

**NFS options matter:**
- `nfsvers=4.1,proto=tcp,timeo=600,retrans=5,hard,intr` for Lustre-backed NFS
- `nfsvers=3,rsize=1048576,wsize=1048576,async` for high-throughput scratch scratch NFS
- Wrong options cause silent performance degradation (soft mount + wrong rsize) or hung processes on server failure (soft vs hard).

### Per-image vs per-node boundary

**Per-image (role-based):** which filesystems a role mounts, the NFS server hostname or IP (usually a cluster-internal alias), mount options. These go in the image as fstab entries or systemd mount units in the rootfs overlay.

**Per-node:** nothing in this category is truly per-individual-node for filesystem mounts. Node-local scratch disk is handled by clonr's partition layout, not by fstab.

**Critical for ADR-0009:** fstab entries for **shared network filesystems** (NFS, Lustre, BeeGFS) must be in the image's rootfs. fstab entries for the **OS disk itself** (`/`, `/boot`, swap) are generated at deploy time and must NOT be in the image. The image model must explicitly separate these two classes of fstab entry. If the deploy-time fstab generation overwrites the entire fstab, shared filesystem mounts are lost. If it merges, it must have a principled merge strategy.

### Priority

MUST for v1.0. The fstab merge boundary is one of the hardest architectural requirements in this document.

### Suggested mechanism

**Rootfs overlay** for network filesystem mounts: fstab snippets placed in `/etc/fstab.d/` (a clonr convention, not a distro standard), with deploy-time fstab assembly concatenating the OS-disk-generated block with the overlay fstab.d/ entries. Alternatively: systemd `.mount` units for network filesystems (placed in `/etc/systemd/system/` via overlay) and a deploy-time-generated `/etc/fstab` covering only the OS disk. Systemd mount units are cleaner because they have explicit dependency ordering and failure behavior.

---

## 8. Network Configuration

### What admins actually need

HPC nodes are not typical cloud VMs. Network configuration is complex:

**Bonding:**
- Compute nodes typically have a bonded management interface (`bond0`: two 1GbE ports, mode=active-backup or 802.3ad) and a separate high-speed fabric interface (100GbE or IB, not bonded). The bond must be up before network-online.target so that Slurm, NFS, and Kerberos work.
- Login nodes often have an additional external-facing interface with a different gateway and routing table (policy routing).

**VLAN tagging:**
- Some sites use VLAN-tagged management interfaces: `bond0.101` for management, `bond0.102` for storage.

**MTU:**
- Jumbo frames (MTU 9000) are almost universal on storage and MPI networks. Wrong MTU = 40% throughput loss on NFS or MPI.
- The OS MTU setting must match the switch port and the NIC driver. Setting MTU requires `ip link set` or network config before the interface is up.

**Infiniband / RDMA:**
- IB interfaces (mlx5_0, etc.) are managed by the IB stack, not NetworkManager. They need `opensm` on the fabric, not a local config. But the IPoIB address (if used) must be configured.

**DNS and NTP:**
- Managed via config files (see §2), not interface config. But the interface that reaches the DNS server must be up first.

### Per-image vs per-node boundary

**Per-image:** bond configuration template (which interface names to bond, mode, LACP parameters), MTU settings, VLAN IDs for known VLANs, IPoIB config template. This goes in the rootfs overlay as systemd-networkd `.network` and `.netdev` files or NM keyfiles.

**Per-node:** IP address, hostname (obviously). These are set at deploy time via `node_config`. Interface names may also vary per node if NIC hardware differs.

**Architectural concern:** Systemd-networkd `.network` files match on interface name or MAC address. If the image has `.network` files that match on MAC address (`[Match] MACAddress=`), they are per-node and belong in `node_config`. If they match on interface name (`[Match] Name=eth0`), they are role-based and can go in the image. The image model must document this convention.

### Priority

MUST for v1.0 (MTU, bond template, DNS/NTP via config files in overlay). SHOULD for v1.1 (per-node IP injection, VLAN automation). Per-node IP assignment is likely out of scope for the image model and belongs in `node_config`.

### Suggested mechanism

**Rootfs overlay** for network config templates. Per-node IP/hostname via `node_config` at deploy time. clonr writes per-node network config into the deployed rootfs during finalize (already partially established in ADR-0008 for `node-token`).

---

## 9. Monitoring and Management Agents

### What admins actually need

Every production HPC node runs at least one monitoring agent. The mix depends on site tooling:

**Open source:**
- `prometheus-node-exporter` (systemd service, exposes metrics on :9100, needs firewall allow from Prometheus scrape IPs)
- `collectd` with custom plugins (many legacy HPC sites)
- `ganglia-gmond` (very common in older HPC sites, still maintained)
- `telegraf` (InfluxDB-centric sites)

**Commercial/vendor:**
- Dell OpenManage (iDRAC-adjacent, on Dell nodes only)
- HPE iLO Amplifier pack agents
- Bright Cluster Manager agent (common in commercial HPC clusters)
- Altair PBS Works or IBM Platform LSF management agents (on non-Slurm sites)

**In-house:**
- Site-specific health check daemons that run local hardware tests and report to a management database
- IPMI data collectors that scrape BMC sensor data and push to time-series stores
- Custom GPU health monitors (DCGM for NVIDIA, often wrapped in a site script)

All of these are installed as packages (see §1) and enabled as services (see §6). Their configuration — which server to report to, authentication — is either in the package config file (rootfs overlay, §2) or in a per-node injected credential (§12).

### Why it matters

A node not visible to monitoring is a node that silently fails. A disk filling up, a NIC dropping packets, a GPU thermal throttling — all invisible without agents. Operational teams use the monitoring layer to catch problems before users report jobs failing.

### Priority

MUST for v1.0 (packages + service enablement). SHOULD for v1.1 (agent config templating in overlay).

### Suggested mechanism

No special mechanism needed beyond §1 (packages) and §6 (services). Agent configuration files go in the rootfs overlay (§2). Only credential injection is special and handled by §12.

---

## 10. Custom Post-Install Scripts

### What admins actually need

Not everything can be expressed declaratively. The escape hatch is a shell script that runs inside the chroot after all packages are installed, overlays are applied, and services are configured. This script can:

- Register the node with a commercial license manager (writes license server address to a config file that varies by cluster)
- Run `authselect select sssd --force` to apply PAM config (some sites require this after sssd package install)
- Run `mlnxofedinstall --kernel <kver> --force` to install MLNX_OFED against the pinned kernel
- Run `nvidia-smi` to verify the driver loaded correctly (sanity check)
- Run `dracut --force` to rebuild initramfs with specific modules (if the image model does not handle initramfs regeneration separately)
- Set SELinux booleans (`setsebool -P slurmd_disable_trans 1`)
- Apply any configuration that a package install step requires but that cannot be expressed as a file overlay

### Why it matters

HPC environments have enormous heterogeneity. MLNX_OFED alone requires a multi-step install process that produces files spread across dozens of paths — not expressible as a file overlay. Without a post-install script hook, admins will work around it by hacking the rootfs in the chroot shell (see §13), producing untracked mutations.

### Priority

MUST for v1.0. This is the escape hatch that makes the rest of the system practical rather than theoretical.

### Suggested mechanism

**Post-install script** — a shell script (or list of scripts, run in order) specified in the image definition. Runs inside the chroot with the full rootfs mounted. Scripts are stored as part of the image definition in the clonr database, versioned, and re-run if the image is rebuilt. Scripts must be idempotent (documented requirement, not enforced by clonr). Script stdout/stderr is captured to the image build log.

**Critical constraint for ADR-0009:** Post-install scripts run at **image build time**, inside the chroot, before the image is sealed as a content tarball. They do NOT run at deploy time on the target node. Any script that needs to run at first boot on the target node is a different mechanism (systemd oneshot unit, see ADR-0008).

---

## 11. Per-Node vs Per-Image vs Per-Group Customization

### The three layers

This is the hardest design question and the one most likely to produce architectural debt if answered wrong.

**In the image (same for all nodes of a given role):**
- Installed packages and versions
- Service enablement/disablement/masking
- Configuration files that reference cluster-level addresses (NFS server, Slurm controller, KDC, DNS) — NOT node-specific addresses
- Kernel version and module configuration
- Cmdline extras (hugepages, mitigations, isolcpus template)
- User accounts with fixed UIDs/GIDs
- Post-install scripts
- Monitoring agent config (which server to report to is cluster-level, not per-node)

**In `node_config` (per individual node):**
- IP address, hostname, FQDN
- Network interface names if hardware varies
- IPMI/BMC address
- GPU topology (`gres.conf` if per-node GPU count varies)
- Anything that contains the node's MAC address
- Injected secrets (see §12): munge.key, root authorized_keys, SSSD bind password, node token

**In a group template (same for a set of nodes with shared hardware):**
- Hardware-specific cmdline args (if node types differ in hugepage availability, NUMA topology, etc.)
- NIC bonding config if hardware varies across groups but is uniform within a group
- DKMS driver selection if GPU nodes and CPU-only nodes coexist

### The critical boundary

The line between "image" and "group template" is: if two nodes with identical hardware and role differ only by name and IP, they should get the same image and differ only in `node_config`. If two nodes with the same name and IP pattern differ by hardware (e.g., different GPU models), they need different images.

Group templates are a clonr-level feature that sits between images and individual node_config: they apply a set of node_config overrides to a set of nodes. This avoids N per-node config entries for 500 identical compute nodes.

### Priority

The three-layer model is MUST for v1.0 at the conceptual level. Group templates as a first-class UI feature are SHOULD for v1.1 (for v1.0, admins can tolerate per-node config for small clusters).

---

## 12. Secrets

### What admins actually need

The following secrets must reach deployed nodes but must NEVER be baked into the image:

| Secret | Target path | Mode | Delivery |
|---|---|---|---|
| `munge.key` | `/etc/munge/munge.key` | 0400, owner munge | Per-cluster (same key, all nodes) |
| SSSD bind password | `/etc/sssd/sssd.conf` bind_pw field | 0600, owner root | Per-cluster |
| Root `authorized_keys` | `/root/.ssh/authorized_keys` | 0600, owner root | Per-cluster or per-group |
| Kerberos keytab | `/etc/krb5.keytab` | 0600, owner root | Per-cluster (for host principals) |
| TLS certificates and keys | `/etc/pki/tls/` or `/etc/ssl/` | 0600 for keys | Per-node (subject=node FQDN) |
| Node token | `/etc/clonr/node-token` | 0600, owner root | Per-node (already handled by ADR-0008) |
| Commercial license keys | Varies by product | Varies | Per-cluster |
| IPMI/BMC credentials | `/etc/ipmi/credentials` or similar | 0600 | Per-node or per-cluster |

### Why secrets must not be in images

An image tarball is copyable by anyone with access to the clonr server's Object Storage bucket or image store. If `munge.key` is in the image, it is accessible to anyone who can pull the image. `munge.key` is the root of Slurm's authentication — compromise of `munge.key` allows arbitrary job submission and impersonation of any user in the cluster.

TLS private keys in images mean every node deployment exposes the key in the build artifact. For TLS certificates with a FQDN subject, the cert is node-specific anyway and cannot be pre-baked (the node's FQDN isn't known until node_config assignment).

### Delivery model

Secrets should be delivered via the node-scoped token channel established in ADR-0008 — the same channel used for `clonr-verify-boot`. After the node boots and verifies, it can request its secrets from the clonr server using its per-node token. The server delivers secrets as encrypted payloads that the node decrypts and writes to the correct paths with correct permissions.

Alternative for v1.0 simplicity: secrets injected into the deployed rootfs during the finalize phase by `clonr-static`, retrieved from the clonr server at deploy time using the node token. This follows the same pattern as the node-token write in ADR-0008 §3 — Write path.

**Never acceptable:**
- Secrets in image tarballs
- Secrets in the clonr database as plaintext (encrypted at rest is acceptable)
- Secrets printed to deploy logs
- Secrets in environment variables visible to other processes

### Priority

MUST for v1.0. Specifically: munge.key and root authorized_keys delivery. Other secrets can be handled by the operator via post-first-boot configuration scripts in v1.0, but munge.key is load-bearing for Slurm, and without root SSH access the admin cannot fix anything.

### Suggested mechanism

**Per-node secret injection at finalize time:** `clonr-static` fetches declared secrets from the server (authenticated with the node token), writes them to the rootfs at the declared path with declared uid:gid:mode. The server stores secrets encrypted (AES-256-GCM, key managed separately from the DB). This extends the finalize phase consistently with ADR-0008's precedent.

---

## 13. Image Chroot Shell (Interactive Mutation)

### What admins actually need

The existing "Shell" feature — `systemd-nspawn` into the image rootfs — is a first-class operational tool. Admins use it to:

- Inspect installed packages: `rpm -qa | grep slurm`
- Test configuration files: `sssctl domain-status`
- Run `authselect` interactively to see what it changes
- Install a package they forgot to add to the manifest (the fast feedback loop before formalizing the change)
- Debug a boot failure by inspecting the rootfs state after a deploy that didn't work

### The problem in the content-only model

In the content-only model, every mutation to the rootfs via the chroot shell changes the sealed image content without going through the declared manifest. This is exactly the "rocky101 sidecar mismatch" class of bug: the image's declared state and its actual content diverge. An admin runs `dnf install missing-package` in the chroot, the image now has that package, but the manifest does not. The next time the image is rebuilt from the manifest, the package is gone. The admin can't tell whether the image they're looking at in the UI matches what was last deployed.

### Required behavior in ADR-0009

The chroot shell must remain available — removing it would break operator workflows. But mutations must be handled in one of two ways:

**Option A: Overlay capture.** The chroot session runs in an overlay filesystem (upperdir on temp storage, lowerdir on the sealed image rootfs). At shell exit, the admin is prompted: "You made N changes. Commit as overlay? Discard? Export as post-install script?" The overlay diff is captured and either promoted to a named overlay in the image definition or discarded. The sealed rootfs is never directly mutated. This is the cleanest model architecturally but requires overlay FS infrastructure in the server.

**Option B: Dirty flag + rebuild required.** Direct mutations are allowed to the rootfs, but the image is marked `dirty: true` after any chroot session. A dirty image cannot be deployed (or deploys with an explicit admin acknowledgment warning). The admin must either rebuild the image from manifest (losing the shell mutations) or explicitly promote the mutations by extracting them as a post-install script or manifest delta. This is simpler to implement but puts the burden on the admin to track what they changed.

**Option C: Read-only shell with diff output.** The shell runs read-only (or in a throwaway overlay that is always discarded). The admin can inspect but not mutate. Any desired change must go through the manifest. This is the strictest model, most aligned with content-only principles, but breaks existing admin workflows that rely on interactive package installation as an exploration tool.

### Recommendation for ADR-0009

Option A is the right long-term answer. Option B is acceptable for v1.0 as a transitional measure with clear UI indication of dirty state. Option C is too disruptive to adopt without significant operator buy-in.

**Shell session write tracking requirements regardless of option chosen:**
1. Every chroot session must be logged: start time, end time, admin user, commands run (if possible via script/typescript capture).
2. The image must carry a `content_hash` or `manifest_hash` that is updated only when a clean rebuild completes from the declared manifest.
3. A dirty image must be clearly distinguished from a clean image in the UI — not just a flag in the DB, but a visible indicator on the image card/detail page.
4. Deploys from a dirty image must generate a warning event in the deploy log.

### Priority

MUST for v1.0: chroot shell remains available, dirty-flag mechanism, visible dirty indicator in UI.
SHOULD for v1.1: overlay capture with promote/discard flow.

---

## Priority Summary

### MUST for v1.0

| # | Requirement | Category |
|---|---|---|
| 1 | Package manifest with repo and version pinning, including post-install script hook for non-dnf installs | §1 |
| 2 | Rootfs overlay for site config files with explicit path, uid:gid:mode | §2 |
| 3 | Kernel version pinning in image manifest, prevent kernel update via dnf exclude | §4 |
| 4 | Per-node secret injection at finalize time (munge.key, root authorized_keys at minimum) | §12 |
| 5 | fstab generation boundary: deploy-time writes only OS-disk entries; network filesystem mounts from image overlay survive untouched | §7 |
| 6 | Post-install script hook (the escape hatch) | §10 |
| 7 | Image cmdline extras appended to deploy-time-generated cmdline | §5 |
| 8 | Service enable/disable/mask declarative list | §6 |
| 9 | Chroot shell dirty-flag mechanism with visible UI indicator | §13 |
| 10 | Fixed UID/GID system account creation at image build time | §3 |

### SHOULD for v1.1

- Group template layer for multi-node shared config
- Overlay FS capture in chroot shell sessions (Option A)
- Per-node TLS certificate delivery
- Network config templating for bonds and VLANs
- Monitoring agent config templating

### COULD for v1.2+

- Automated drift detection (live node state vs manifest)
- Continuous health agent (successor to ADR-0008 oneshot)
- Policy enforcement: prevent deploy of dirty images without override

---

## Top 5 MUST-Haves That Catch Naive Image Models Off Guard

1. **The fstab split.** A naive model regenerates `/etc/fstab` entirely at deploy time and silently drops all NFS/Lustre/GPFS mounts from the image. The cluster looks healthy until the first user tries to access `/home` or `/scratch`. This failure is silent at boot if mounts are `nofail`.

2. **Kernel version pinning + DKMS.** A naive model installs the latest kernel at image build time. NVIDIA driver, MLNX_OFED, and Lustre client all break on the next dnf update. The image model must express "pin this exact kernel and never update it" as a first-class declaration.

3. **munge.key delivery.** A naive model either bakes `munge.key` into the image (security failure) or omits it entirely (Slurm auth failure on first boot). Without munge, no jobs run. This is not an edge case — it is the authentication layer for the job scheduler.

4. **Systemd service masking vs disabling.** A naive model only disables unwanted services. On the next `dnf update`, a package's RPM scriptlet runs `systemctl enable firewalld` during post-install, and now compute nodes have a restrictive local firewall that blocks all MPI traffic. Masking prevents this; disabling does not.

5. **Post-install script for MLNX_OFED / NVIDIA.** These are not installable as simple dnf packages. A naive model with only a package list cannot install them. Without MLNX_OFED, IB fabric is unreachable. Without NVIDIA drivers, GPU nodes have no GPUs. Sites running these stacks require the post-install escape hatch on day one.

---

## Three Hardest Architectural Requirements

### 1. fstab ownership split (§7)

The content-only image model generates `/etc/fstab` at deploy time for OS-disk entries (root, boot, swap). But the image must contribute fstab entries for network filesystems that are unknown to clonr's deploy logic. These two classes of entries must be merged at deploy time without either overwriting the other. The naive implementation overwrites `/etc/fstab` entirely; the correct implementation either uses `/etc/fstab.d/` snippets (assembled at deploy time) or separates OS-disk fstab (deploy-time) from network-filesystem mounts (systemd `.mount` units in the image overlay). This requires a documented convention that both the image model and the deploy engine agree on.

### 2. DKMS build inside the image chroot (§4)

DKMS rebuilds kernel modules for a specific kernel version at install time. For the build to succeed inside the chroot, the chroot must have: the pinned kernel-devel headers installed, gcc and dkms installed, access to `/proc`, `/sys`, and `/dev` (which systemd-nspawn provides). The DKMS build produces `.ko` files at `/lib/modules/<kver>/extra/`. These must be present in the sealed image before it is tarred. The architectural challenge: DKMS build is not a pure file operation — it executes compiler toolchains inside the chroot and is sensitive to the build environment. It must be treated as a post-install script step with explicit success/failure detection, and the resulting `.ko` files must be verifiable (the image model should know that `/lib/modules/<kver>/extra/nvidia.ko` should exist for GPU images).

### 3. Secrets injection without baking into the image (§12)

The content-only image model seals the rootfs as a content tarball. Per-node secrets (munge.key, authorized_keys, TLS private keys, Kerberos keytab) cannot be in that tarball. They must be injected into the deployed rootfs during the finalize phase on the target node. This requires: (a) the clonr server to store secrets encrypted and associated with node or cluster scope, (b) `clonr-static` to fetch the secrets from the server using the node token and write them to the rootfs at finalize time, (c) the image definition to declare which secrets are needed and at which paths with which permissions. The finalize phase already writes `node-token` (ADR-0008 §3) — secrets injection is an extension of this pattern. The hardest part is the key management: the clonr server must not store secrets in plaintext, and the encryption key must be managed separately from the DB. For v1.0, a practical approach is envelope encryption: secrets encrypted with a per-deployment key derived from the node token, stored encrypted in the DB. The node decrypts using its token material. This avoids a full KMS dependency while providing meaningful protection against DB-level compromise.
