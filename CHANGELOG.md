# Changelog

## 0.1.1 — 2026-05-03

### Fixes

- **Image import (ISO URLs):** `from-url` requests now route through `Factory.PullImage` so ISO inputs hit the qemu+kickstart auto-install pipeline. Previously the web UI's "Add Image" form bypassed the pipeline entirely, silently producing an unusable raw ISO blob. Founder-reported regression; fixed in #237.
- **RPM packaging:** `clustr-serverd` now declares its full isoinstaller runtime deps as `Requires:` — `qemu-kvm`, `qemu-img`, `genisoimage`, `p7zip`, `p7zip-plugins`, `kpartx`, `rsync`, `edk2-ovmf` — so a fresh `dnf install clustr-serverd` pulls in everything the ISO build pipeline needs without manual intervention. A CI assertion step was added that fails the build if any declared dep goes missing (#238).

## 0.1.0 — 2026-05-03

Initial public release. Open-source HPC node cloning and image management suite.
Server (`clustr-serverd`) + privilege helper boundary (`clustr-privhelper`) + static CLI + web UI.
Distributed as signed RPMs for EL8/EL9/EL10 (x86_64/aarch64).

### Highlights

- **Chassis enclosures** — enclosure entity with unified node placement endpoint; datacenter rack diagram supports enclosure-scoped node assignment
- **Image auto-reconcile** — background reconciler self-heals orphaned staging artifacts; blob auto-reconcile on startup with resume on partial downloads
- **GPG-signed RPM repo** — auto-generates repo GPG key on first startup; `rpmsign` pipeline for per-EL signed packages published to `pkg.sqoia.dev`
- **Web build smoke CI** — `ci.yml` runs a full Vite build + route smoke check on every push to main
- **NAT keepalive on exec** — WebSocket ping/pong keepalive on `clustr exec` sessions prevents NAT idle-timeout disconnects
- **Health ping** — `clustr health --ping` reports round-trip latency to the server
- **UDPCast multicast** — `udp-sender`/`udp-receiver` vendored from source (GPL-2.0) for fleet-reimage multicast; attached source tarball on every release for §3a compliance
- **BIOS push** — Intel `syscfg`, Dell `racadm`, Supermicro `sum` providers; BIOS profile CRUD and diff+apply pipeline
- **Distro drivers** — `DistroDriver` interface covering EL8/EL9/EL10, Ubuntu 20/22/24, Debian 12, SLES 15
- **Slurm RPM pipeline** — clustr builds and signs Slurm RPMs into a per-cluster internal yum repo; nodes consume via `dnf` (no external network required). Bundles tab is the cluster's Slurm catalog.
- **clustr-privhelper** — single setuid privilege boundary for all host-root operations; replaces polkit/sudoers entries
- **Rack diagram** — drag-and-drop node placement, unassigned sidebar, height-U selector, multi-rack tile layout
- **Alert engine** — YAML-defined alert rules, async SMTP delivery worker pool, silence support
- **SEC-1/SEC-2 hardening** — Bearer token restricted to `Authorization:` header only; lsblk echo redacted from initramfs build logs
- **UID range split** — `ldap_user` (10000–60000) and `system_account` (200–999) allocated from separate ranges to prevent UID drift with DNF-managed daemons
