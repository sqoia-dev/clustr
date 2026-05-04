# Changelog

## 0.1.6 — 2026-05-03

### Fixes

- **Datacenter chassis (Sprint 31 followups):** `ListEnclosuresByRack` no longer panics on rack refresh after a chassis is added (was caused by nested SQLite queries while the outer cursor was still open). Migration 100 relaxes `node_rack_position` columns (`rack_id`, `slot_u`, `height_u`) to nullable so nodes can actually be placed inside chassis — Sprint 31's NOT NULL constraint was incompatible with its own XOR trigger, making nodes unplaceable in any enclosure since Sprint 31 shipped.
- **SELF-MON:** `collectSystemd` now logs an ERR when its 5s timeout fires (was silently returning nil, invisible in logs). Heartbeat write moved to the start of each collect cycle so slow collectors no longer trip `WatchdogSec=90`.

## 0.1.5 — 2026-05-03

### Features

- **Server:** ISO downloads via the `from-url` import are now cached at `/var/lib/clustr/iso-cache/` and resume on interruption. Re-importing the same URL hits cache instantly. Operators can `rm -rf` the cache dir to reclaim space (no eviction policy yet — v0.1.6 follow-up).
- **Server:** Build serial-log ring bumped 100 → 1000 lines server-side to match UI capacity. Anaconda output no longer truncated.

### Refactor

- **Server:** ISO build phase emission deduplicated. Each phase fires exactly once now.

## 0.1.4 — 2026-05-03

### Features

- **Web UI:** The Add Image dialog now shows live download progress (bytes / total / %) with ETA estimated from a rolling 10-sample rate average, replacing the static "Downloading…" placeholder. When Content-Length is absent, an indeterminate spinner is shown instead.
- **Web UI:** Install phases (generating_config through finalizing) show a scrollable monospace serial console panel streaming the last 500 lines of anaconda qemu output in real time. Auto-scrolls to bottom; stops auto-scroll when the user scrolls up (sticky-scroll).
- **Server:** ISO download phase now emits `BuildHandle.SetProgress` events per-chunk during HTTP read. The `pullAsync` → `pullAndExtract` → `buildFromISOFile` chain wires `OnPhase`, `OnSerialLine`, and `OnStderrLine` callbacks through to the existing SSE event store.

## 0.1.3 — 2026-05-03

### Features

- **Self-monitoring (SELF-MON):** clustr-serverd now monitors its own control-plane host — root disk, data disk, scratch space, memory, PSI, systemd unit state, time drift, cert expiry, and image-store orphans. Persistent status strip in the web UI; new `/control-plane` detail route. 17 default alert rules baked in.
- **Schema:** new `hosts` table with `role` column distinguishing `control_plane` from `cluster_node`. Migration 099. Cluster `nodes` carry a nullable `host_id` FK back to `hosts`.
- **Anti-regression:** `WatchdogSec=90` on the `clustr-serverd` systemd unit; new `clustr-selfmon-watchdog.timer` fires every 5 minutes, checks `/run/clustr/selfmon.heartbeat` staleness, and posts `crit` to syslog plus an optional fallback webhook (`/etc/clustr/fallback-alert-url`) if the metrics goroutine has hung.
- **Packaging:** `chrony` declared as a `Requires:` dep for `chronyc tracking` (NTP drift metric).

## 0.1.2 — 2026-05-03

### Fixes

- **isoinstaller:** qemu VMs now get a `virtio-rng` device and 4 GB default memory — fixes early-boot hang on Rocky 10 (entropy starvation + memory pressure).
- **isoinstaller:** Default build timeout bumped 30 m → 60 m to fit real-world Rocky 10 anaconda + dnf-update wall time.

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
