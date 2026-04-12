# clonr Test Lab Design

## Overview

This document specifies the complete Proxmox-based test lab for clonr — a node cloning and
image management suite for HPC/bare-metal environments. The lab emulates an HPC provisioning
environment with one server VM running clonr-serverd and two to three disposable test node VMs
that are PXE-booted, imaged, wiped, and reimaged in automated test cycles.

Everything here is copy-paste runnable. Commands are written for a Proxmox 8.x host running
on Debian 12. Adapt PVE node name and storage pool names to your environment.

---

## Architecture Summary

```
Proxmox Host
│
├── vmbr0  (existing LAN / management — NOT used by clonr test traffic)
│
└── vmbr10 (isolated test lab bridge — 10.99.0.0/24, no uplink, no NAT)
       │
       ├── clonr-server VM (VMID 200)
       │     ├── eth0 → vmbr0  (management: SSH, image uploads from your workstation)
       │     └── eth1 → vmbr10 (PXE/TFTP/DHCP server, image push — 10.99.0.1)
       │
       ├── test-node-01 VM (VMID 201) — single NVMe-style disk, UEFI
       │     └── eth0 → vmbr10 (PXE client — DHCP from clonr-server)
       │
       ├── test-node-02 VM (VMID 202) — single SATA disk, BIOS
       │     └── eth0 → vmbr10 (PXE client — DHCP from clonr-server)
       │
       └── test-node-03 VM (VMID 203) — two SATA disks, BIOS (multi-disk scenario)
             └── eth0 → vmbr10 (PXE client — DHCP from clonr-server)
```

The test bridge `vmbr10` has no physical uplink and no IP on the host itself. It is a
completely isolated Layer 2 segment. Test nodes have no route to the Proxmox management
network or to the internet. The clonr-server is the only VM with a foot on both networks.

---

## 1. Proxmox Bridge Configuration

### 1.1 Create the isolated bridge

Edit `/etc/network/interfaces` on the Proxmox host and add:

```
# clonr test lab — isolated, no uplink, no IP on host
auto vmbr10
iface vmbr10 inet manual
    bridge-ports none
    bridge-stp off
    bridge-fd 0
    bridge-vlan-aware no
    # No address here. clonr-server owns 10.99.0.1 inside a VM.
    # The host cannot route into this segment by design.
```

Apply without rebooting:

```bash
ifreload -a
# Verify
ip link show vmbr10
```

No routes are added on the host. Test node traffic is entirely contained within the bridge.

---

## 2. VM Specifications

### 2.1 clonr-server VM (VMID 200)

This VM runs clonr-serverd, dnsmasq (DHCP + TFTP), and holds disk image blobs.

| Resource | Value | Rationale |
|---|---|---|
| vCPUs | 2 | Image compression and rsync are CPU-bound |
| RAM | 2048 MB | SQLite + Go server + dnsmasq fit easily; headroom for image caching |
| OS disk | 32 GB (virtio-scsi, SSD-backed pool) | Rocky 9 base + clonr-serverd binary + tooling |
| Image storage disk | 100 GB (virtio-scsi) | Blob storage for disk images; resize as needed |
| NIC 0 | vmbr0 (VirtIO) | Management — SSH from workstation, image uploads |
| NIC 1 | vmbr10 (VirtIO) | PXE/provisioning network — static 10.99.0.1/24 |
| Boot | BIOS (SeaBIOS) | Server VM, no reason for UEFI overhead |
| Display | serial0 | Headless; use Proxmox console or SSH |

Create with qm:

```bash
# Storage pool names: adjust 'local-lvm' and 'local' to your pool names
PVE_NODE="pve"
STORAGE_FAST="local-lvm"   # SSD-backed thin pool
STORAGE_BULK="local-lvm"   # Or a separate spinning/bulk pool

qm create 200 \
  --name clonr-server \
  --node "${PVE_NODE}" \
  --memory 2048 \
  --balloon 0 \
  --cores 2 \
  --cpu host \
  --ostype l26 \
  --machine q35 \
  --bios seabios \
  --scsihw virtio-scsi-pci \
  --scsi0 "${STORAGE_FAST}:32,format=qcow2,discard=on,ssd=1" \
  --scsi1 "${STORAGE_BULK}:100,format=qcow2,discard=on" \
  --net0 virtio,bridge=vmbr0 \
  --net1 virtio,bridge=vmbr10 \
  --serial0 socket \
  --vga serial0 \
  --onboot 0 \
  --boot order=scsi0
```

Mount the Rocky 9 cloud image (or minimal ISO) and install the OS. After first boot, configure
networking inside the VM:

```bash
# /etc/sysconfig/network-scripts/ifcfg-eth0  (management)
DEVICE=eth0
BOOTPROTO=dhcp
ONBOOT=yes

# /etc/sysconfig/network-scripts/ifcfg-eth1  (provisioning — static)
DEVICE=eth1
BOOTPROTO=static
IPADDR=10.99.0.1
PREFIX=24
ONBOOT=yes
```

Mount the image storage disk to `/srv/clonr`:

```bash
mkfs.xfs /dev/sdb
echo '/dev/sdb /srv/clonr xfs defaults,noatime 0 2' >> /etc/fstab
mkdir -p /srv/clonr
mount /srv/clonr
```

### 2.2 Test Node VMs

Test nodes are intentionally minimal. They are wiped and reimaged repeatedly. They have no
persistent data worth keeping. Mark them visually in Proxmox with a tag so nobody accidentally
treats them as real machines.

#### test-node-01: NVMe-style disk, UEFI (VMID 201)

This exercises the `nvme0n1` device path in `DiscoverDisks()` and the UEFI boot path.
The virtio-blk controller with `iothread=1` presents as an NVMe-class device from lsblk's
`tran` field perspective when using the `virtio-scsi-single` driver with SSD flag set.

Note on NVMe simulation: Proxmox/QEMU does not expose a real NVMe controller to guests by
default. To get lsblk to report `tran=nvme`, use the `qemu-xhci` + `nvme` device type
available in QEMU 6.x+. The Proxmox GUI does not expose this; use qm set with `-args`.

```bash
qm create 201 \
  --name test-node-01 \
  --node "${PVE_NODE}" \
  --memory 1024 \
  --balloon 0 \
  --cores 2 \
  --cpu host \
  --ostype l26 \
  --machine q35 \
  --bios ovmf \
  --efidisk0 "${STORAGE_FAST}:1,format=qcow2,efitype=4m,pre-enrolled-keys=0" \
  --scsihw virtio-scsi-pci \
  --scsi0 "${STORAGE_FAST}:40,format=qcow2,discard=on,ssd=1" \
  --net0 virtio,bridge=vmbr10 \
  --serial0 socket \
  --vga serial0 \
  --onboot 0 \
  --boot order=net0

# Add QEMU NVMe controller via args (exposes nvme0n1 to guest lsblk)
qm set 201 --args "-drive file=/dev/zvol/${STORAGE_FAST}/vm-201-disk-0,if=none,id=nvme0 \
  -device nvme,drive=nvme0,serial=TESTLAB201NVMe"
```

Simpler alternative if the args approach is too fiddly: set the virtio-scsi disk and accept
that lsblk reports `tran=` empty (virtio). The `DiscoverDisks()` code handles empty `Tran`
gracefully — the field is just left blank. The disk name will be `sda` not `nvme0n1`. Use
this node primarily to test UEFI boot repair scenarios.

#### test-node-02: Single SATA disk, BIOS (VMID 202)

```bash
qm create 202 \
  --name test-node-02 \
  --node "${PVE_NODE}" \
  --memory 1024 \
  --balloon 0 \
  --cores 2 \
  --cpu host \
  --ostype l26 \
  --machine q35 \
  --bios seabios \
  --scsihw virtio-scsi-pci \
  --scsi0 "${STORAGE_FAST}:40,format=qcow2,discard=on" \
  --net0 virtio,bridge=vmbr10 \
  --serial0 socket \
  --vga serial0 \
  --onboot 0 \
  --boot order=net0
```

This gives lsblk a single `sda` device. Rotational flag is unset (SSD-backed), so `rota=0`.
To exercise the HDD/rotational path in `DiscoverDisks()`, omit `,ssd=1` from the disk arg and
the guest kernel will report `rota=1`.

#### test-node-03: Two SATA disks, BIOS (VMID 203)

```bash
qm create 203 \
  --name test-node-03 \
  --node "${PVE_NODE}" \
  --memory 1024 \
  --balloon 0 \
  --cores 2 \
  --cpu host \
  --ostype l26 \
  --machine q35 \
  --bios seabios \
  --scsihw virtio-scsi-pci \
  --scsi0 "${STORAGE_FAST}:40,format=qcow2,discard=on" \
  --scsi1 "${STORAGE_FAST}:40,format=qcow2,discard=on" \
  --net0 virtio,bridge=vmbr10 \
  --net1 virtio,bridge=vmbr10 \
  --serial0 socket \
  --vga serial0 \
  --onboot 0 \
  --boot order=net0
```

Two disks (`sda`, `sdb`) exercise the multi-disk enumeration loop in `parseLsblkJSON()`. Two
NICs on the same bridge exercise `DiscoverNICs()` returning multiple entries — useful for
testing NIC bond and VLAN discovery scenarios.

### 2.3 Hardware Profile Summary

| VM | VMID | Disk(s) | NIC(s) | Boot | lsblk tran |
|---|---|---|---|---|---|
| clonr-server | 200 | sda (OS), sdb (images) | eth0 (mgmt), eth1 (prov) | BIOS | — |
| test-node-01 | 201 | nvme0n1 or sda | eth0 | UEFI | nvme or virtio |
| test-node-02 | 202 | sda | eth0 | BIOS | sata/virtio |
| test-node-03 | 203 | sda, sdb | eth0, eth1 | BIOS | sata/virtio |

---

## 3. Network Design

### 3.1 IP Addressing

```
Network:   10.99.0.0/24
Mask:      255.255.255.0
Gateway:   none (isolated segment — no routing out)

clonr-server eth1:  10.99.0.1   (static, configured in VM)
test-node-01:       10.99.0.11  (DHCP reservation by MAC)
test-node-02:       10.99.0.12  (DHCP reservation by MAC)
test-node-03:       10.99.0.13  (DHCP reservation by MAC)
DHCP pool (dynamic): 10.99.0.50 - 10.99.0.99  (for ad-hoc VMs)
```

### 3.2 dnsmasq Configuration on clonr-server

Install dnsmasq on clonr-server:

```bash
dnf install -y dnsmasq
```

Write `/etc/dnsmasq.d/clonr-lab.conf`:

```ini
# clonr test lab — dnsmasq config
# Serves DHCP and TFTP on eth1 (10.99.0.1) only.
# This interface is isolated: no DNS forwarding needed.

interface=eth1
bind-interfaces
except-interface=lo
except-interface=eth0

# DHCP — only serve on the provisioning network
dhcp-range=10.99.0.50,10.99.0.99,12h

# Static leases — keyed by MAC. Get MACs from Proxmox after VM creation:
#   qm config 201 | grep net0
# VirtIO MACs are in the format BC:24:11:xx:xx:xx (Proxmox OUI)
dhcp-host=BC:24:11:AA:AA:01,test-node-01,10.99.0.11
dhcp-host=BC:24:11:AA:AA:02,test-node-02,10.99.0.12
dhcp-host=BC:24:11:AA:AA:03,test-node-03,10.99.0.13

# TFTP root for PXE
enable-tftp
tftp-root=/srv/tftp

# PXE boot — send different bootloader depending on client arch
# Tag UEFI clients (architecture 7 = x86_64 UEFI EFI BC, 9 = x86_64 UEFI)
dhcp-match=set:efi-x86_64,option:client-arch,7
dhcp-match=set:efi-x86_64,option:client-arch,9
dhcp-boot=tag:efi-x86_64,grub/grubx64.efi,,10.99.0.1

# Legacy BIOS PXE — send iPXE chainloader
dhcp-boot=tag:!efi-x86_64,pxelinux/pxelinux.0,,10.99.0.1

# If client is already running iPXE (has the iPXE user-class), send iPXE script
dhcp-userclass=set:ipxe,iPXE
dhcp-boot=tag:ipxe,http://10.99.0.1:8080/boot.ipxe

# DNS — minimal, just resolve clonr-server itself
address=/clonr-server.lab/10.99.0.1
domain=lab
local=/lab/
```

Replace the `dhcp-host` MAC addresses after creating the VMs:

```bash
# Get the actual MACs Proxmox assigned
qm config 201 | grep '^net0'
qm config 202 | grep '^net0'
qm config 203 | grep '^net0'
# Output looks like: net0: virtio=BC:24:11:xx:xx:xx,bridge=vmbr10
```

Enable and start:

```bash
systemctl enable --now dnsmasq
firewall-cmd --zone=trusted --add-interface=eth1 --permanent
firewall-cmd --reload
```

---

## 4. PXE Boot Chain

### 4.1 TFTP Root Layout

```
/srv/tftp/
├── pxelinux/
│   ├── pxelinux.0          # BIOS PXE NBP (from syslinux package)
│   ├── ldlinux.c32         # Required syslinux module
│   ├── libutil.c32
│   ├── menu.c32
│   └── pxelinux.cfg/
│       └── default         # Default boot menu
├── grub/
│   ├── grubx64.efi         # UEFI GRUB2 EFI binary
│   ├── grub.cfg            # GRUB2 config (loads iPXE or directly boots kernel)
│   └── fonts/
│       └── unicode.pf2
├── ipxe/
│   └── ipxe.lkrn           # iPXE as a kernel (chainloaded by pxelinux for BIOS)
└── clonr/
    ├── vmlinuz             # Rocky 9 kernel for initramfs boot
    └── initramfs.img       # clonr initramfs (built in Section 5)
```

Populate the syslinux files:

```bash
dnf install -y syslinux-tftpboot
mkdir -p /srv/tftp/pxelinux/pxelinux.cfg
cp /tftpboot/pxelinux.0 /srv/tftp/pxelinux/
cp /tftpboot/ldlinux.c32 /srv/tftp/pxelinux/
cp /tftpboot/libutil.c32 /srv/tftp/pxelinux/
cp /tftpboot/menu.c32 /srv/tftp/pxelinux/
```

Populate GRUB2 UEFI binary:

```bash
dnf install -y grub2-efi-x64
mkdir -p /srv/tftp/grub
cp /boot/efi/EFI/rocky/grubx64.efi /srv/tftp/grub/

# Or extract from the grub2-efi-x64 RPM directly:
rpm2cpio /path/to/grub2-efi-x64-*.rpm | cpio -idmv
# Then copy the .efi from the extracted path
```

### 4.2 BIOS PXE Config (pxelinux)

Write `/srv/tftp/pxelinux/pxelinux.cfg/default`:

```
DEFAULT clonr
PROMPT 0
TIMEOUT 30

LABEL clonr
  MENU LABEL clonr initramfs
  KERNEL ../clonr/vmlinuz
  INITRD ../clonr/initramfs.img
  APPEND console=ttyS0,115200 clonr.server=http://10.99.0.1:8080 quiet
```

The `clonr.server` kernel parameter is read by the clonr client startup script from
`/proc/cmdline` to know where to connect.

### 4.3 UEFI GRUB2 Config

Write `/srv/tftp/grub/grub.cfg`:

```
set default=0
set timeout=5

menuentry "clonr initramfs" {
  echo "Loading clonr kernel..."
  linuxefi /clonr/vmlinuz \
    console=ttyS0,115200 \
    clonr.server=http://10.99.0.1:8080 \
    quiet
  echo "Loading initramfs..."
  initrdefi /clonr/initramfs.img
  boot
}
```

Note: GRUB2 TFTP paths are relative to the TFTP root when using `linuxefi` over TFTP.

### 4.4 iPXE Script (HTTP, served by clonr-serverd or nginx)

This is served at `http://10.99.0.1:8080/boot.ipxe` when dnsmasq sends iPXE-capable clients
here. In the dnsmasq config above, clients already running iPXE hit this URL. This allows
dynamic boot logic per MAC address in future, but for now it boots all nodes into clonr.

Write `/srv/clonr/boot.ipxe` and serve it from clonr-serverd or a static nginx:

```ipxe
#!ipxe

echo clonr PXE boot — ${net0/mac}
echo Server: http://10.99.0.1:8080

set server http://10.99.0.1:8080

kernel ${server}/pxe/vmlinuz \
  console=ttyS0,115200 \
  clonr.server=${server} \
  clonr.mac=${net0/mac} \
  quiet
initrd ${server}/pxe/initramfs.img
boot
```

Note the `clonr.mac` parameter — the clonr client can read this from `/proc/cmdline` to
identify itself to the server without needing arp/ip discovery.

---

## 5. Initramfs Build Process

The clonr client binary is a statically linked Go binary. The initramfs only needs:
- Linux kernel + drivers (virtio-net, virtio-blk, virtio-scsi, ext4, xfs)
- Busybox for minimal shell, mount, ip, etc.
- `lsblk` from util-linux (required by `DiscoverDisks()` — the code shells out to it)
- `clonr` binary
- A startup script that runs on PID 1

### 5.1 Build Environment

Do this on clonr-server or a Rocky 9 build machine with internet access:

```bash
dnf install -y dracut dracut-network util-linux busybox kmod
```

### 5.2 Compile the clonr Static Binary

On a Go 1.24 build host (or inside the clonr-server VM with Go installed):

```bash
cd /path/to/clonr
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-s -w -extldflags=-static" \
  -o /tmp/clonr-static ./cmd/clonr
file /tmp/clonr-static
# Expected: ELF 64-bit LSB executable, x86-64, statically linked
```

Verify no dynamic links:

```bash
ldd /tmp/clonr-static
# Expected: not a dynamic executable
```

### 5.3 Startup Script (PID 1 init)

Write `/tmp/clonr-init.sh` — this becomes PID 1 inside the initramfs:

```bash
#!/bin/sh
# clonr initramfs init — PID 1
# Mounts essential filesystems, brings up the network, then hands off to clonr.
set -e

# Mount essential pseudo-filesystems
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
mkdir -p /dev/pts
mount -t devpts devpts /dev/pts

# Load virtio drivers (needed for disk and network discovery in QEMU/KVM)
modprobe virtio_net   2>/dev/null || true
modprobe virtio_blk   2>/dev/null || true
modprobe virtio_scsi  2>/dev/null || true
modprobe nvme         2>/dev/null || true

# Parse kernel cmdline for server address and MAC hint
SERVER=""
MAC=""
for param in $(cat /proc/cmdline); do
    case "$param" in
        clonr.server=*) SERVER="${param#clonr.server=}" ;;
        clonr.mac=*)    MAC="${param#clonr.mac=}" ;;
    esac
done

if [ -z "$SERVER" ]; then
    echo "FATAL: clonr.server not set in kernel cmdline" >&2
    echo "cmdline: $(cat /proc/cmdline)" >&2
    exec /bin/sh  # Drop to shell for debugging
fi

# Bring up the first non-loopback interface via DHCP
# udhcpc is busybox's DHCP client
IFACE=""
for iface in $(ls /sys/class/net/); do
    [ "$iface" = "lo" ] && continue
    ip link set "$iface" up
    IFACE="$iface"
    break
done

if [ -z "$IFACE" ]; then
    echo "FATAL: no network interface found" >&2
    exec /bin/sh
fi

echo "Bringing up ${IFACE} via DHCP..."
udhcpc -i "$IFACE" -T 10 -t 5 -q -s /etc/udhcpc/default.script

echo "Network up. Running clonr client..."
echo "  Server:  ${SERVER}"
echo "  MAC:     ${MAC}"

# Run clonr — it connects to clonr-serverd, reports hardware, and awaits commands
exec /usr/local/bin/clonr \
    --server "${SERVER}" \
    --mac "${MAC}"
```

Write `/tmp/udhcpc-default.sh` — udhcpc calls this after getting a lease:

```bash
#!/bin/sh
# Minimal udhcpc script for the clonr initramfs
case "$1" in
    bound|renew)
        ip addr add "${ip}/${mask}" dev "${interface}" 2>/dev/null || true
        ip route add default via "${router}" 2>/dev/null || true
        ;;
    deconfig)
        ip addr flush dev "${interface}" 2>/dev/null || true
        ;;
esac
```

### 5.4 Build the Initramfs with dracut

Dracut is the standard initramfs builder on RHEL/Rocky. We use it to create a minimal
initramfs that includes our custom files.

Create a dracut module directory:

```bash
mkdir -p /usr/lib/dracut/modules.d/99clonr
```

Write `/usr/lib/dracut/modules.d/99clonr/module-setup.sh`:

```bash
#!/bin/bash
# dracut module: clonr client initramfs

check() {
    # Always include this module
    return 0
}

depends() {
    # We need network support
    echo "network"
}

install() {
    # Install the clonr binary
    inst_binary /tmp/clonr-static /usr/local/bin/clonr

    # Install lsblk (required by DiscoverDisks — it shells out to lsblk --json --bytes)
    inst_binary /usr/bin/lsblk

    # Install busybox and create symlinks for the tools we use in init
    inst_binary /sbin/busybox
    for tool in sh mount ip modprobe; do
        ln -sf /sbin/busybox "${initdir}/bin/${tool}" 2>/dev/null || true
    done

    # Install udhcpc (busybox DHCP client)
    ln -sf /sbin/busybox "${initdir}/sbin/udhcpc"

    # Install our udhcpc script
    inst_dir /etc/udhcpc
    inst /tmp/udhcpc-default.sh /etc/udhcpc/default.script
    chmod +x "${initdir}/etc/udhcpc/default.script"

    # Install init script — this becomes /init (PID 1)
    inst /tmp/clonr-init.sh /init
    chmod +x "${initdir}/init"

    # Install kernel modules for virtio and common disk controllers
    instmods virtio_net virtio_blk virtio_scsi nvme nvme_core ext4 xfs
}
```

Build the initramfs:

```bash
KERNEL_VERSION=$(uname -r)

dracut \
  --no-hostonly \
  --no-hostonly-cmdline \
  --force \
  --modules "base network 99clonr" \
  --omit "plymouth resume usrmount" \
  --add-drivers "virtio_net virtio_blk virtio_scsi nvme ext4 xfs" \
  --compress gzip \
  /srv/tftp/clonr/initramfs.img \
  "${KERNEL_VERSION}"

# Copy the matching kernel
cp "/boot/vmlinuz-${KERNEL_VERSION}" /srv/tftp/clonr/vmlinuz
```

Verify the initramfs contents:

```bash
lsinitrd /srv/tftp/clonr/initramfs.img | grep -E '(clonr|lsblk|init)'
# Expected: ./usr/local/bin/clonr, ./usr/bin/lsblk, ./init
```

Check initramfs size — keep it under 100 MB for reasonable PXE boot times:

```bash
du -sh /srv/tftp/clonr/initramfs.img
```

### 5.5 Manual Initramfs Build (Alternative — No Dracut)

If dracut is not available, build the cpio archive manually:

```bash
#!/bin/bash
set -euo pipefail

BUILD=$(mktemp -d)
trap 'rm -rf "$BUILD"' EXIT

# Skeleton
mkdir -p "${BUILD}"/{proc,sys,dev,dev/pts,tmp,etc/udhcpc,usr/local/bin,usr/bin,bin,sbin,lib64,lib/modules}

# Static clonr binary
cp /tmp/clonr-static "${BUILD}/usr/local/bin/clonr"
chmod 755 "${BUILD}/usr/local/bin/clonr"

# lsblk — must be statically linked or carry its deps
# Build static lsblk from util-linux source, or use a pre-built static lsblk:
cp /path/to/static-lsblk "${BUILD}/usr/bin/lsblk"
chmod 755 "${BUILD}/usr/bin/lsblk"

# Busybox static
cp /path/to/busybox-static "${BUILD}/sbin/busybox"
chmod 755 "${BUILD}/sbin/busybox"

# Busybox symlinks
for tool in sh mount ip modprobe udhcpc; do
    ln -sf /sbin/busybox "${BUILD}/bin/${tool}"
done

# Init script
cp /tmp/clonr-init.sh "${BUILD}/init"
chmod 755 "${BUILD}/init"

# udhcpc script
cp /tmp/udhcpc-default.sh "${BUILD}/etc/udhcpc/default.script"
chmod 755 "${BUILD}/etc/udhcpc/default.script"

# Copy kernel modules for virtio and NVMe
KVER=$(uname -r)
for mod in virtio_net virtio_blk virtio_scsi nvme nvme_core ext4 xfs; do
    modpath=$(find "/lib/modules/${KVER}" -name "${mod}.ko*" 2>/dev/null | head -1)
    if [ -n "$modpath" ]; then
        relpath="${modpath#/lib/modules/${KVER}/}"
        mkdir -p "${BUILD}/lib/modules/${KVER}/$(dirname "$relpath")"
        cp "$modpath" "${BUILD}/lib/modules/${KVER}/${relpath}"
    fi
done

# modules.dep for modprobe
depmod -b "${BUILD}" "${KVER}" 2>/dev/null || true

# Pack
(cd "${BUILD}" && find . -print0 | cpio --null --create --format=newc) \
  | gzip -9 > /srv/tftp/clonr/initramfs.img

echo "Built: $(du -sh /srv/tftp/clonr/initramfs.img | cut -f1)"
```

---

## 6. Proxmox Automation Scripts

### 6.1 VM Lifecycle Helper

Write `/usr/local/bin/clonr-lab` on the Proxmox host:

```bash
#!/bin/bash
# clonr-lab — test node lifecycle management
# Usage: clonr-lab <command> [vmid]
# Commands: pxe-boot, stop, wipe-disk, status, cycle
set -euo pipefail

VMIDS=(201 202 203)
SERVER_VMID=200
PVE_HOST="localhost"

die() { echo "ERROR: $*" >&2; exit 1; }

require_vmid() {
    local vmid="${1:-}"
    [[ "$vmid" =~ ^20[123]$ ]] || die "VMID must be 201, 202, or 203. Got: ${vmid}"
    echo "$vmid"
}

vm_status() {
    local vmid="$1"
    pvesh get "/nodes/localhost/qemu/${vmid}/status/current" --output-format json 2>/dev/null \
      | python3 -c "import sys,json; print(json.load(sys.stdin)['status'])"
}

pxe_boot() {
    local vmid=$(require_vmid "$1")
    local current
    current=$(vm_status "$vmid")

    if [ "$current" = "running" ]; then
        echo "Stopping ${vmid}..."
        qm stop "$vmid"
        sleep 3
    fi

    # Force next boot to PXE by setting boot order to net0 first
    qm set "$vmid" --boot order=net0
    echo "Starting ${vmid} in PXE boot mode..."
    qm start "$vmid"
    echo "VM ${vmid} booting via PXE. Watch console: qm terminal ${vmid}"
}

disk_boot() {
    local vmid=$(require_vmid "$1")
    local current
    current=$(vm_status "$vmid")

    if [ "$current" = "running" ]; then
        echo "Stopping ${vmid}..."
        qm stop "$vmid"
        sleep 3
    fi

    # Restore disk-first boot order
    qm set "$vmid" --boot order=scsi0
    echo "Starting ${vmid} from disk..."
    qm start "$vmid"
}

wipe_disk() {
    local vmid=$(require_vmid "$1")
    local current
    current=$(vm_status "$vmid")

    [ "$current" = "stopped" ] || die "VM ${vmid} must be stopped before wiping disk. Run: qm stop ${vmid}"

    # Find the disk — assumes scsi0 is the target disk
    local disk_path
    disk_path=$(pvesh get "/nodes/localhost/qemu/${vmid}/config" --output-format json \
      | python3 -c "
import sys, json
cfg = json.load(sys.stdin)
scsi0 = cfg.get('scsi0', '')
# Extract volume ID (e.g. local-lvm:vm-201-disk-0)
vol = scsi0.split(',')[0]
print(vol)
")
    echo "Wiping disk: ${disk_path}"
    # Zero the first 100MB to clear partition table and bootloader
    qemu-img info "${disk_path}" > /dev/null  # validate path
    dd if=/dev/zero of="/dev/$(pvesm path ${disk_path})" bs=1M count=100 status=progress 2>/dev/null || \
      echo "Direct wipe failed — use qm snapshot or recreate disk instead"
    echo "Disk ${disk_path} wiped."
}

snapshot_save() {
    local vmid=$(require_vmid "$1")
    local snapname="${2:-before-test-$(date +%Y%m%d%H%M%S)}"
    qm snapshot "$vmid" "$snapname" --description "clonr test lab snapshot"
    echo "Snapshot saved: ${snapname}"
}

snapshot_restore() {
    local vmid=$(require_vmid "$1")
    local snapname="${2:-}"
    [ -n "$snapname" ] || die "Usage: clonr-lab rollback <vmid> <snapname>"
    qm rollback "$vmid" "$snapname"
    echo "Rolled back ${vmid} to ${snapname}"
}

status_all() {
    echo "=== clonr test lab status ==="
    for vmid in "${VMIDS[@]}"; do
        local st
        st=$(vm_status "$vmid" 2>/dev/null || echo "unknown")
        printf "  VM %d (%s): %s\n" "$vmid" "$(qm config "$vmid" | grep '^name:' | awk '{print $2}')" "$st"
    done
    echo ""
    echo "=== clonr-server (VM ${SERVER_VMID}) ==="
    vm_status "${SERVER_VMID}"
}

cycle_test() {
    # Full cycle: PXE boot a node, wait for it to register with server,
    # then boot from disk and verify
    local vmid=$(require_vmid "${1:-201}")
    echo "=== Starting full deploy cycle for VM ${vmid} ==="

    snapshot_save "$vmid" "pre-test-$(date +%s)"
    pxe_boot "$vmid"

    echo "Waiting for node to register with clonr-server..."
    local timeout=120
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        if ssh -i /path/to/lab-key -o StrictHostKeyChecking=no \
           root@10.99.0.1 \
           "curl -sf http://localhost:8080/api/nodes | grep -q '10.99.0.1[123]'" 2>/dev/null; then
            echo "Node registered."
            break
        fi
        sleep 5
        elapsed=$((elapsed + 5))
        echo "  ${elapsed}s / ${timeout}s..."
    done

    [ "$elapsed" -lt "$timeout" ] || die "Node did not register within ${timeout}s"

    echo "Triggering image deploy from server..."
    # clonr-serverd API call — adjust endpoint to match actual clonr API
    ssh -i /path/to/lab-key root@10.99.0.1 \
      "curl -sf -X POST http://localhost:8080/api/deploy \
        -H 'Content-Type: application/json' \
        -d '{\"node_ip\":\"10.99.0.1${vmid: -1}\",\"image_id\":\"rocky9-base\"}'"

    echo "Waiting for deploy to complete..."
    sleep 30

    disk_boot "$vmid"
    echo "VM ${vmid} rebooted into deployed image."

    echo "Verifying deployed OS..."
    local node_ip="10.99.0.1${vmid: -1}"
    sleep 60  # wait for boot
    if ssh -o StrictHostKeyChecking=no root@"${node_ip}" "hostname && uname -r"; then
        echo "SUCCESS: VM ${vmid} booted from deployed image."
    else
        echo "WARN: SSH verification failed — check console manually."
    fi
}

CMD="${1:-help}"
shift || true

case "$CMD" in
    pxe-boot)      pxe_boot "$1" ;;
    disk-boot)     disk_boot "$1" ;;
    wipe)          wipe_disk "$1" ;;
    snapshot)      snapshot_save "$@" ;;
    rollback)      snapshot_restore "$@" ;;
    status)        status_all ;;
    cycle)         cycle_test "${1:-201}" ;;
    *)
        echo "clonr-lab — test node lifecycle"
        echo ""
        echo "Usage: clonr-lab <command> [vmid]"
        echo ""
        echo "Commands:"
        echo "  pxe-boot <vmid>              PXE boot a test node"
        echo "  disk-boot <vmid>             Boot a test node from disk"
        echo "  wipe <vmid>                  Zero the test node's disk"
        echo "  snapshot <vmid> [name]       Save a VM snapshot"
        echo "  rollback <vmid> <name>       Roll back to a snapshot"
        echo "  status                       Show all VM states"
        echo "  cycle [vmid]                 Run full deploy cycle (default: 201)"
        ;;
esac
```

```bash
chmod +x /usr/local/bin/clonr-lab
```

### 6.2 Terraform Alternative

If you prefer Terraform over shell scripts, the `proxmox-ve/proxmox` Terraform provider can
manage the VMs. Below is the essential resource definition for one test node. Repeat for 202
and 203 with adjusted disk/NIC configs.

Write `lab/main.tf`:

```hcl
terraform {
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.66"
    }
  }
}

provider "proxmox" {
  endpoint  = "https://your-proxmox-host:8006/api2/json"
  api_token = var.proxmox_api_token
  insecure  = true  # Set to false if using a valid TLS cert on Proxmox
}

variable "proxmox_api_token" {
  description = "Proxmox API token in format USER@REALM!TOKENID=SECRET"
  sensitive   = true
}

locals {
  node    = "pve"
  storage = "local-lvm"
}

# ---- test-node-01 (NVMe-style, UEFI) ----
resource "proxmox_virtual_environment_vm" "test_node_01" {
  name      = "test-node-01"
  node_name = local.node
  vm_id     = 201
  tags      = ["clonr-test-lab", "disposable"]

  # UEFI with OVMF
  bios    = "ovmf"
  machine = "q35"

  efi_disk {
    datastore_id = local.storage
    file_format  = "qcow2"
    type         = "4m"
  }

  cpu {
    cores = 2
    type  = "host"
  }

  memory {
    dedicated = 1024
  }

  disk {
    datastore_id = local.storage
    interface    = "scsi0"
    size         = 40
    file_format  = "qcow2"
    discard      = "on"
    ssd          = true
  }

  network_device {
    bridge = "vmbr10"
    model  = "virtio"
  }

  boot_order = ["net0"]  # Always PXE boot by default

  serial_device {}

  # No cloud-init, no OS image — node is deliberately empty
  # clonr-serverd will write an OS to it
}

# ---- test-node-02 (single SATA, BIOS) ----
resource "proxmox_virtual_environment_vm" "test_node_02" {
  name      = "test-node-02"
  node_name = local.node
  vm_id     = 202
  tags      = ["clonr-test-lab", "disposable"]

  bios    = "seabios"
  machine = "q35"

  cpu {
    cores = 2
    type  = "host"
  }

  memory {
    dedicated = 1024
  }

  disk {
    datastore_id = local.storage
    interface    = "scsi0"
    size         = 40
    file_format  = "qcow2"
    discard      = "on"
  }

  network_device {
    bridge = "vmbr10"
    model  = "virtio"
  }

  boot_order = ["net0"]
  serial_device {}
}

# ---- test-node-03 (two disks, two NICs, BIOS) ----
resource "proxmox_virtual_environment_vm" "test_node_03" {
  name      = "test-node-03"
  node_name = local.node
  vm_id     = 203
  tags      = ["clonr-test-lab", "disposable"]

  bios    = "seabios"
  machine = "q35"

  cpu {
    cores = 2
    type  = "host"
  }

  memory {
    dedicated = 1024
  }

  disk {
    datastore_id = local.storage
    interface    = "scsi0"
    size         = 40
    file_format  = "qcow2"
    discard      = "on"
  }

  disk {
    datastore_id = local.storage
    interface    = "scsi1"
    size         = 40
    file_format  = "qcow2"
    discard      = "on"
  }

  network_device {
    bridge = "vmbr10"
    model  = "virtio"
  }

  network_device {
    bridge = "vmbr10"
    model  = "virtio"
  }

  boot_order = ["net0"]
  serial_device {}
}
```

```bash
cd lab
terraform init
terraform plan -var="proxmox_api_token=root@pam!clonr-lab=YOURTOKENHERE"
terraform apply
```

Generate a scoped API token for Terraform in Proxmox:

```bash
# On the Proxmox host
pveum token add root@pam clonr-lab --privsep 0
# Save the returned token value — it is shown only once
```

### 6.3 CI-Friendly Full-Cycle Test Script

Write `/usr/local/bin/clonr-lab-ci-test` on the Proxmox host:

```bash
#!/bin/bash
# clonr-lab-ci-test — end-to-end deploy and capture cycle for CI
# Exit code 0 = all tests passed, non-zero = failure
# Designed to run from a CI runner that can SSH to the Proxmox host.
set -euo pipefail

VMID="${1:-202}"            # Default to test-node-02 (simplest config)
IMAGE_ID="rocky9-base"
SERVER_IP="10.99.0.1"
SERVER_API="http://${SERVER_IP}:8080"
LAB_KEY="/root/.ssh/clonr-lab"  # SSH key with access to clonr-server
TIMEOUT_BOOT=180            # seconds to wait for PXE boot + OS registration
TIMEOUT_DEPLOY=300          # seconds to wait for image deployment
TIMEOUT_SSH=120             # seconds to wait for SSH after reboot

pass() { echo "[PASS] $*"; }
fail() { echo "[FAIL] $*" >&2; exit 1; }
log()  { echo "[INFO] $(date +%H:%M:%S) $*"; }

ssh_server() {
    ssh -i "$LAB_KEY" -o StrictHostKeyChecking=no -o ConnectTimeout=5 \
        root@"$SERVER_IP" "$@"
}

wait_for() {
    local desc="$1"
    local timeout="$2"
    local check_cmd="$3"
    local elapsed=0
    log "Waiting for: ${desc} (timeout: ${timeout}s)"
    while [ "$elapsed" -lt "$timeout" ]; do
        if eval "$check_cmd" > /dev/null 2>&1; then
            log "${desc}: ready after ${elapsed}s"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    fail "${desc}: timed out after ${timeout}s"
}

log "=== clonr CI test: VM ${VMID} ==="

# 0. Pre-test: snapshot for rollback
log "Saving pre-test snapshot..."
qm snapshot "$VMID" "ci-pre-$(date +%s)" --description "clonr CI pre-test" || true

# 1. PXE boot
log "PXE booting VM ${VMID}..."
qm stop "$VMID" 2>/dev/null || true
sleep 2
qm set "$VMID" --boot order=net0
qm start "$VMID"

# 2. Wait for node to register with clonr-server
NODE_IP="10.99.0.${VMID: -1}"
wait_for "node ${NODE_IP} registered with clonr-server" "$TIMEOUT_BOOT" \
    "ssh_server curl -sf ${SERVER_API}/api/nodes | python3 -c \"import sys,json; nodes=json.load(sys.stdin); exit(0 if any(n.get('ip')=='${NODE_IP}' for n in nodes) else 1)\""

pass "Hardware discovery complete — node registered"

# 3. Verify hardware discovery output
log "Checking hardware report..."
HW_REPORT=$(ssh_server "curl -sf '${SERVER_API}/api/nodes/${NODE_IP}/hardware'")
echo "$HW_REPORT" | python3 -c "
import sys, json
hw = json.load(sys.stdin)
assert hw.get('Disks'), 'No disks discovered'
assert hw.get('NICs'),  'No NICs discovered'
assert hw.get('CPUs'),  'No CPUs discovered'
assert hw.get('Memory',{}).get('TotalKB',0) > 0, 'Memory is zero'
print('  Disks:', len(hw['Disks']))
print('  NICs: ', len(hw['NICs']))
print('  CPUs: ', len(hw['CPUs']))
print('  RAM:  ', hw['Memory']['TotalKB'] // 1024, 'MB')
"
pass "Hardware discovery valid"

# 4. Deploy image
log "Deploying image '${IMAGE_ID}' to ${NODE_IP}..."
DEPLOY_JOB=$(ssh_server "curl -sf -X POST '${SERVER_API}/api/deploy' \
    -H 'Content-Type: application/json' \
    -d '{\"node_ip\":\"${NODE_IP}\",\"image_id\":\"${IMAGE_ID}\"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"job_id\"])'")

log "Deploy job: ${DEPLOY_JOB}"

wait_for "deploy job ${DEPLOY_JOB} complete" "$TIMEOUT_DEPLOY" \
    "ssh_server curl -sf '${SERVER_API}/api/jobs/${DEPLOY_JOB}' | python3 -c \"import sys,json; j=json.load(sys.stdin); exit(0 if j.get('status')=='done' else 1)\""

pass "Image deployed"

# 5. Reboot into deployed OS
log "Rebooting VM ${VMID} from disk..."
qm stop "$VMID"
sleep 2
qm set "$VMID" --boot order=scsi0
qm start "$VMID"

# 6. Verify SSH access to deployed OS
wait_for "SSH on deployed node ${NODE_IP}" "$TIMEOUT_SSH" \
    "ssh -i $LAB_KEY -o StrictHostKeyChecking=no -o ConnectTimeout=5 root@${NODE_IP} true"

DEPLOYED_HOST=$(ssh -i "$LAB_KEY" -o StrictHostKeyChecking=no root@"$NODE_IP" hostname)
DEPLOYED_KERNEL=$(ssh -i "$LAB_KEY" -o StrictHostKeyChecking=no root@"$NODE_IP" uname -r)
log "Deployed OS: hostname=${DEPLOYED_HOST} kernel=${DEPLOYED_KERNEL}"
pass "Node booted from deployed image"

# 7. Capture image back from running node
log "Capturing node back to new image 'rocky9-captured'..."
CAPTURE_JOB=$(ssh_server "curl -sf -X POST '${SERVER_API}/api/capture' \
    -H 'Content-Type: application/json' \
    -d '{\"node_ip\":\"${NODE_IP}\",\"image_id\":\"rocky9-captured\"}' | python3 -c 'import sys,json; print(json.load(sys.stdin)[\"job_id\"])'")

wait_for "capture job ${CAPTURE_JOB} complete" "$TIMEOUT_DEPLOY" \
    "ssh_server curl -sf '${SERVER_API}/api/jobs/${CAPTURE_JOB}' | python3 -c \"import sys,json; j=json.load(sys.stdin); exit(0 if j.get('status')=='done' else 1)\""

pass "Image captured"

# 8. Rollback to clean state
log "Rolling back VM ${VMID} to pre-test snapshot..."
qm stop "$VMID"
qm rollback "$VMID" "ci-pre-$(date +%s)" 2>/dev/null || \
    log "WARN: rollback failed — wipe disk manually before next run"

log "=== CI test complete: VM ${VMID} ALL PASS ==="
```

```bash
chmod +x /usr/local/bin/clonr-lab-ci-test
```

---

## 7. Test Scenarios Matrix

| # | Scenario | VM | Boot Mode | What It Tests |
|---|---|---|---|---|
| T-01 | Fresh deploy to empty disk | 202 | PXE then disk | Basic image deployment — zero the disk first, deploy, verify boot |
| T-02 | Capture running node → redeploy | 202 | PXE then disk | Round-trip image fidelity: deploy, boot, capture, redeploy, compare checksums |
| T-03 | Multi-disk node deployment | 203 | PXE then disk | `parseLsblkJSON()` enumerating sda + sdb; image written to primary disk, secondary untouched |
| T-04 | UEFI boot repair after deploy | 201 | PXE then disk | `fix-efiboot` or equivalent command: verify EFI partition and grubx64.efi are valid post-deploy |
| T-05 | Chroot customization → deploy | 202 | PXE then disk | Image factory: capture base, chroot-install packages, redeploy, verify packages present |
| T-06 | Concurrent multicast deploy | 201+202+203 | PXE (all three) | All three nodes PXE boot simultaneously; server pushes same image to all; verify all three boot |
| T-07 | Network bond/VLAN NIC discovery | 203 | PXE | `DiscoverNICs()` with two NICs: eth0 + eth1 both appear in report; MAC/state/driver correct |
| T-08 | HDD rotational flag detection | 202 (rota=1 disk) | PXE | lsblk `rota` field: recreate VM disk without `ssd=1` flag; verify `Rotational=true` in discovery |
| T-09 | DMI identity fields | Any | PXE | `/sys/class/dmi/id/` values in QEMU/KVM match expected QEMU strings (QEMU, pc-i440fx, etc.) |
| T-10 | Deploy to wrong node (safety) | N/A | N/A | clonr-server refuses deploy if node MAC not registered or not in initiated session |

### T-01: Fresh Deploy to Empty Disk

```bash
# On Proxmox host
clonr-lab wipe 202
clonr-lab pxe-boot 202
# Wait for registration, then on clonr-server:
curl -X POST http://localhost:8080/api/deploy \
  -H 'Content-Type: application/json' \
  -d '{"node_ip":"10.99.0.12","image_id":"rocky9-base"}'
# After deploy completes:
clonr-lab disk-boot 202
# SSH to 10.99.0.12 and verify
```

### T-02: Round-Trip Capture

```bash
# After T-01 completes (node booted from deployed image):
# Capture the running node
curl -X POST http://localhost:8080/api/capture \
  -H 'Content-Type: application/json' \
  -d '{"node_ip":"10.99.0.12","image_id":"rocky9-rt-test"}'
# Wipe and redeploy the captured image
clonr-lab wipe 202
clonr-lab pxe-boot 202
curl -X POST http://localhost:8080/api/deploy \
  -H 'Content-Type: application/json' \
  -d '{"node_ip":"10.99.0.12","image_id":"rocky9-rt-test"}'
clonr-lab disk-boot 202
# Checksum key files to verify fidelity:
# md5sum /etc/os-release /etc/fstab (compare pre- and post-capture)
```

### T-03: Multi-Disk Node

```bash
clonr-lab pxe-boot 203
# On server — check discovery output for two disks
curl http://localhost:8080/api/nodes/10.99.0.13/hardware | python3 -m json.tool | grep -A5 Disks
# Expected: [{Name:sda,...},{Name:sdb,...}]
# Deploy specifying target disk
curl -X POST http://localhost:8080/api/deploy \
  -H 'Content-Type: application/json' \
  -d '{"node_ip":"10.99.0.13","image_id":"rocky9-base","target_disk":"sda"}'
```

### T-06: Concurrent Multicast Deploy

```bash
# PXE boot all three simultaneously
for vmid in 201 202 203; do
    clonr-lab pxe-boot "$vmid" &
done
wait
# Trigger multicast from server
curl -X POST http://localhost:8080/api/multicast \
  -H 'Content-Type: application/json' \
  -d '{"node_ips":["10.99.0.11","10.99.0.12","10.99.0.13"],"image_id":"rocky9-base"}'
# Verify all three boot
for ip in 10.99.0.11 10.99.0.12 10.99.0.13; do
    for vmid in 201 202 203; do clonr-lab disk-boot "$vmid" & done; wait
    sleep 60
    for ip in 10.99.0.11 10.99.0.12 10.99.0.13; do
        ssh root@"$ip" hostname && echo "OK: $ip" || echo "FAIL: $ip"
    done
done
```

---

## 8. Safety and Isolation

### 8.1 Network Isolation

The `vmbr10` bridge has no physical uplink (`bridge-ports none`). This is not a VLAN — it is
a completely disconnected virtual switch. Traffic from test nodes physically cannot leave the
Proxmox host. There is no NAT, no iptables FORWARD rule, and no route from vmbr10 to vmbr0
on the host.

Verification:

```bash
# On Proxmox host — confirm no route between bridges
ip route show
# vmbr10 must not appear with a gateway

# Confirm no FORWARD rules bridging vmbr0 and vmbr10
iptables -L FORWARD -n
# Should show DROP or no rules passing traffic between the two bridges

# From inside a test node VM, confirm no external connectivity
ping -c1 8.8.8.8    # must time out
ping -c1 10.99.0.1  # must succeed (clonr-server)
```

If you need the test nodes to reach the internet temporarily (e.g., to pull packages during a
test), add a time-limited NAT rule and remove it immediately after:

```bash
# TEMPORARY — enable internet for test nodes
iptables -t nat -A POSTROUTING -s 10.99.0.0/24 -o vmbr0 -j MASQUERADE
iptables -A FORWARD -i vmbr10 -o vmbr0 -j ACCEPT
iptables -A FORWARD -i vmbr0 -o vmbr10 -m state --state RELATED,ESTABLISHED -j ACCEPT

# REMOVE when done
iptables -t nat -D POSTROUTING -s 10.99.0.0/24 -o vmbr0 -j MASQUERADE
iptables -D FORWARD -i vmbr10 -o vmbr0 -j ACCEPT
iptables -D FORWARD -i vmbr0 -o vmbr10 -m state --state RELATED,ESTABLISHED -j ACCEPT
```

### 8.2 Disk Safety

Test node disks are QEMU virtual disks (qcow2 files inside a Proxmox storage pool). They are
identified by Proxmox volume IDs like `local-lvm:vm-201-disk-0`. There is no path by which
clonr can write to a physical disk on the Proxmox host or any other real server, because:

1. The test node VMs have no visibility of the host filesystem.
2. The only storage presented to test nodes is their own virtio-scsi disk.
3. The clonr-server VM does not mount test node disks — it pushes images over the network.

The clonr binary on the initramfs discovers disks via `lsblk`. In a QEMU/KVM test node, the
only disks lsblk sees are `sda` (or `nvme0n1`) which are the VM's own qcow2-backed virtual
disks. These are obviously not real hardware — the vendor field (`/sys/class/dmi/id/sys_vendor`)
will read `QEMU` and the product name will contain `Standard PC`.

To make the DMI distinction machine-readable in the clonr client, consider adding a guard:

```go
// In the clonr client's deploy or wipe logic:
if strings.Contains(dmi.SystemManufacturer, "QEMU") {
    log.Println("Running in QEMU test environment")
}
```

This is a belt-and-suspenders advisory — the network isolation is the real safety guarantee.

### 8.3 Proxmox API Token Scoping

Create a dedicated Proxmox API token for the `clonr-lab` script with minimum permissions:

```bash
# Create a role with only what the lab scripts need
pveum role add ClonrLabAutomation \
  --privs "VM.PowerMgmt,VM.Snapshot,VM.Config.Options,VM.Console"

# Create a user
pveum user add clonr-lab@pve

# Assign role to user, scoped to the /vms/201, /vms/202, /vms/203 paths only
pveum aclmod /vms/201 --user clonr-lab@pve --role ClonrLabAutomation
pveum aclmod /vms/202 --user clonr-lab@pve --role ClonrLabAutomation
pveum aclmod /vms/203 --user clonr-lab@pve --role ClonrLabAutomation

# Generate token
pveum token add clonr-lab@pve lab-token --privsep 1
```

This token cannot touch the clonr-server VM (VMID 200), cannot modify storage, and cannot
access any VM outside 201-203. If a CI runner is compromised, blast radius is three
disposable test VMs.

### 8.4 Snapshot Before Every Test Run

Every test run should snapshot the test node before doing anything destructive. The
`clonr-lab-ci-test` script does this automatically. For manual runs:

```bash
clonr-lab snapshot 202 clean-base
# ... run tests ...
clonr-lab rollback 202 clean-base
```

This means a bad deploy that bricks the VM disk is a 10-second rollback, not a VM rebuild.

---

## 9. Quick Start Checklist

Run through these steps in order when standing up the lab from scratch:

```
[ ] 1. Add vmbr10 bridge to /etc/network/interfaces and run ifreload -a
[ ] 2. Create VMs: qm create 200, 201, 202, 203 (commands in Section 2)
[ ] 3. Install Rocky 9 on VM 200 (clonr-server) via ISO or cloud image import
[ ] 4. Configure eth1 on VM 200 as 10.99.0.1/24
[ ] 5. mkfs.xfs /dev/sdb && mount to /srv/clonr on VM 200
[ ] 6. Install dnsmasq on VM 200, write /etc/dnsmasq.d/clonr-lab.conf with real MACs
[ ] 7. systemctl enable --now dnsmasq
[ ] 8. Build static clonr binary and copy to clonr-server
[ ] 9. Build initramfs (dracut method, Section 5.4)
[ ] 10. Populate /srv/tftp/ with pxelinux, grub, and clonr/ files
[ ] 11. Install clonr-serverd, configure to use /srv/clonr as blob root
[ ] 12. Pull rocky9 cloud image, import into clonr-serverd as "rocky9-base"
[ ] 13. Copy clonr-lab script to Proxmox host, chmod +x
[ ] 14. Run: clonr-lab status
[ ] 15. Run: clonr-lab pxe-boot 202 and watch serial console: qm terminal 202
[ ] 16. Verify node registers at http://10.99.0.1:8080/api/nodes
[ ] 17. Run: clonr-lab-ci-test 202
```

---

## 10. Notes on DMI Fields in KVM Guests

The clonr hardware package reads `/sys/class/dmi/id/` for system identity. In QEMU/KVM, these
fields are populated from the QEMU DMI tables. Default values (relevant to test validation):

| DMI Field | KVM Default Value |
|---|---|
| `sys_vendor` (SystemManufacturer) | `QEMU` |
| `product_name` (SystemProductName) | `Standard PC (Q35 + ICH9, 2009)` for q35 |
| `product_uuid` (SystemUUID) | QEMU-generated UUID, unique per VM |
| `product_serial` (SystemSerial) | Empty string by default |
| `bios_vendor` (BIOSVendor) | `SeaBIOS` or `EFI Development Kit II / OVMF` |

The `product_uuid` will be different for every VM even with identical configs. This is the
field most likely used by clonr-serverd to uniquely identify a node across reboots. Verify
that the UUID is stable across reboots for a given VM (it should be — Proxmox writes a fixed
UUID into the QEMU config and SMBIOS tables).

To set a custom serial number on a VM (useful for testing the serial-based identity path):

```bash
qm set 202 --smbios1 "uuid=12345678-0000-0000-0000-000000000202,serial=TESTLAB202,manufacturer=SQOIA,product=clonr-test-node"
```

This will make `lsblk` and `/sys/class/dmi/id/product_serial` return `TESTLAB202`.
