# clustr — Architectural Overview

> Single-binary HPC bare-metal cluster manager: node registration, image
> management, PXE/iPXE provisioning, Slurm bundling, LDAP identity, and an
> embedded React web UI. Targets RHEL-family distros (EL8 / EL9 / EL10).

This document is a working snapshot of the clustr codebase as of the date the
repo was cloned. It is meant for engineers who want to come up to speed on
what is in the tree, where things live, and how the major subsystems
interact. Pair this with `README.md` (operator quick-start) and
`PACKAGING.md` (release pipeline + RPM layout).

---

## Table of contents

1. [Goals & non-goals](#goals--non-goals)
2. [Components & binaries](#components--binaries)
3. [Repository layout](#repository-layout)
4. [Server architecture](#server-architecture)
5. [Provisioning lifecycle](#provisioning-lifecycle)
6. [Image system](#image-system)
7. [Slurm bundling](#slurm-bundling)
8. [Identity (LDAP, system accounts, sudoers)](#identity-ldap-system-accounts-sudoers)
9. [Power & IPMI](#power--ipmi)
10. [Storage layout on disk](#storage-layout-on-disk)
11. [Configuration model](#configuration-model)
12. [Web UI](#web-ui)
13. [Database](#database)
14. [Authentication & authorization](#authentication--authorization)
15. [Build, package, release](#build-package-release)
16. [Glossary](#glossary)

---

## Goals & non-goals

**clustr is**:

- A purpose-built **provisioning + management plane** for HPC / lab / bare-metal
  RHEL-family clusters.
- Distributed as **two static Go binaries** (`clustr-serverd`, `clustr`) plus a
  small node agent (`clustr-clientd`) and a privilege-helper (`clustr-privhelper`).
- A **single-process server** that embeds the React web UI, the HTTP API, the
  PXE DHCP/TFTP server, the iPXE chainload endpoints, the SQLite store, the
  reimage orchestrator, optional slapd-backed LDAP, the SMTP notifier, and a
  webhook dispatcher.
- Distributed via **signed dnf repositories** at `pkg.sqoia.dev/clustr/{el8,el9,el10}`.

**clustr is not**:

- A general-purpose cluster monitoring system. Per-node stats collection and an
  alert rule engine are built in (see `internal/clientd/stats/` and
  `internal/alerts/`), but clustr is not a replacement for a full observability
  stack. Prometheus metrics are also exposed at `/metrics`.
- A user-job scheduler. Slurm is treated as the workload manager; clustr
  manages Slurm's lifecycle (build, install, accounting users) but is not a
  scheduler itself.
- Cross-distro in the general case. EL8 / EL9 / EL10 are the primary targets;
  Ubuntu 20.04 / 22.04 / 24.04 and Debian are supported via the `DistroDriver`
  interface (see `internal/deploy/distro/`). SLES is not supported.

---

## Components & binaries

| Binary | Source | Lives at | Role |
|---|---|---|---|
| `clustr-serverd` | `cmd/clustr-serverd/` | `/usr/sbin/clustr-serverd` | Long-running daemon. HTTP API + web UI + PXE + reimage orchestrator. |
| `clustr` | `cmd/clustr/` | `/usr/bin/clustr` | Operator CLI. Talks to `clustr-serverd` over HTTPS. Also runs locally on nodes for hardware inventory and IPMI ops. |
| `clustr-clientd` | `cmd/clustr-clientd/` | `/usr/local/bin/clustr-clientd` (on managed nodes) | WebSocket-attached node agent. Heartbeats, applies pushed config, runs Slurm install/upgrade tasks. |
| `clustr-privhelper` | `cmd/clustr-privhelper/` | (helper) | Runs the small set of root-only operations the server needs from a non-root context. 657 LoC — narrow surface. |

Build entry points are tiny; almost all logic lives under `internal/`.

---

## Repository layout

```
cmd/                    # Binary entry points (main packages)
  clustr/               # Operator CLI (~21 cobra subcommands)
  clustr-serverd/       # Server: bootstrap-admin, bundle, doctor, version, run
  clustr-clientd/       # Node agent
  clustr-privhelper/    # Root operations broker

internal/               # Application code (private)
  allocation/           # Auto-policy / auto-compute project allocations
  bootassets/           # Embedded kernel + initramfs assets
  chroot/               # Chroot helpers (image build / capture)
  clientd/              # Node-side agent: heartbeat, slurm install/upgrade,
                        # config apply, journal, exec
  config/               # ServerConfig + PXEConfig (env-var driven)
  db/                   # SQLite + 84 numbered SQL migrations
  deploy/               # Embedded deploy scripts (autodeploy, systemd, etc.)
  hardware/             # Hardware inventory: CPU, memory, NICs, IB, GPUs
  image/                # Image factory, layouts, ISO installer, shell mgr
  initramfs/            # Kernel selection + initramfs build
  ipmi/                 # ipmitool wrapper
  ldap/                 # Embedded slapd lifecycle, DIT, sudoers, write-bind
  metrics/              # Prometheus client wiring
  network/              # Network config generation, cabling, lint
  notifications/        # SMTP + templates
  posixid/              # POSIX UID/GID allocator
  power/                # Power abstraction; ipmi/, proxmox/ providers
  privhelper/           # Privhelper IPC client
  pxe/                  # DHCP server + TFTP server + iPXE assets
  reimage/              # Orchestrator: per-node + group reimage state machine
  secrets/              # Master-key-encrypted secrets at rest
  server/               # HTTP/chi server, sessions, RBAC, embedded web
    handlers/           # All API handlers; portal/ + scripts/ subgroups
    web/                # Embedded compiled React bundle (build artifact)
  slurm/                # Slurm bundle build/install/upgrade, deps_matrix.json
  sysaccounts/          # System account reconciliation across nodes
  sysd/                 # systemd unit helpers
  webhook/              # Webhook dispatcher

pkg/                    # Public-ish API surface
  api/                  # Request/response types
  client/               # Go HTTP client used by `cmd/clustr`

web/                    # React 19 + Vite + Tailwind + shadcn/ui front-end
  src/routes/           # Login, setup, set-password, nodes, images, identity,
                        # groups, slurm, settings, activity
  src/components/       # AppShell, CommandPalette, ImageShell, SessionGate
                        # ErrorBoundary, StatusDot, UserPicker, ui/ (shadcn)

deploy/                 # Operator-facing deployment artifacts
  ansible/roles/clustr  # Single-role Ansible deploy
  docker-compose/       # (legacy; container support removed per PACKAGING.md)
  modprobe.d/           # Kernel module config installed on the host
  modules-load.d/       # Boot-time module loads
  pxe/                  # iPXE assets
  ssh/                  # Embedded SSH known-hosts / keys
  systemd/              # All clustr-* unit files (serverd, autodeploy,
                        # backup, builders.slice, shells.slice, …)
  clustr-serverd.conf   # Default EnvironmentFile (heavily commented)

scripts/                # Build + deploy helpers (initramfs, dev VMs, ops)
build/                  # Slurm bundle build pipeline (versions.yml, deps,
                        # signed manifests/release notes), GPG public key

.github/workflows/      # CI (ci, initramfs, ipxe-build, lab-validate,
                        # release, slurm-build)
nfpm.yaml               # RPM build for clustr-serverd
nfpm-cli.yaml           # RPM build for clustr CLI
PACKAGING.md            # Authoritative release pipeline reference
```

Quick facts:

- **~85,000 LoC of Go** (excluding tests). Generated counts:
  - `internal/server/` is the largest area (handlers + factory + workers).
  - `internal/server/handlers/portal/` houses the PI / Director portal.
  - `internal/slurm/` and `internal/ldap/` are each large enough to deserve
    their own subdirectory of templates and tests.
- **84 SQL migrations** under `internal/db/migrations/`, monotonically
  increasing from `001_initial.sql` to `084_posixid_roles.sql`.
- **No CGO** — `clustr` is built `CGO_ENABLED=0` and statically linked. SQLite
  is pure-Go via `modernc.org/sqlite`.
- **Go 1.25** + Node 24 + pnpm 10 are the build prerequisites.

---

## Server architecture

`clustr-serverd` is a single Go process. The `Server` struct
(`internal/server/server.go`) is the composition root and holds every long-lived
dependency: the chi router, the SQLite handle, the audit service, the log /
progress / image-event / build-progress pubsub stores, the image shell manager,
the power cache & registry, the reimage orchestrator, the LDAP manager, the
Slurm manager, the network manager, the system accounts manager, the clientd
hub (WebSocket fan-out), the webhook dispatcher, the SMTP notifier, the session
HMAC key, and the build info.

### Request lifecycle

1. **HTTP entry** — `chi` router with `RequestID`, `RealIP`, `Recoverer`, and
   structured zerolog middleware.
2. **Session / auth** — Cookie-based browser sessions (HMAC-signed via
   `CLUSTR_SESSION_SECRET`) or `Authorization: Bearer` API tokens. Tokens are
   stored hashed (bcrypt) with optional **node-scoping** (`017_node_scoped_keys.sql`)
   and **rotation** (`020_api_keys_rotation.sql`).
3. **RBAC** — Roles: `admin`, `viewer`, `pi`, `director`. Many handlers gate
   themselves with `requireScope(true)` + `requireRole(role)`. The PI / Director
   portals are entirely separate handler trees under `internal/server/handlers/portal/`.
4. **Audit log** — Every mutation flows through `db.AuditService`, retained for
   `CLUSTR_AUDIT_RETENTION` (default 90d).

### Background workers

Started during `Server.StartBackgroundWorkers`:

- **Reconcile loop** — keeps DB-stored desired state in sync with the cluster.
- **Reimage workers** — drive the per-node and group reimage state machines
  to completion across reboots.
- **Tech-trigger worker** — `tech_trig_worker.go` handles deferred admin actions.
- **Log purger / audit purger** — TTL-based cleanup with archive write to
  `CLUSTR_LOG_ARCHIVE_DIR`.
- **Power cache refresher** — cached IPMI power-state polling.
- **clientd hub** — WebSocket fan-out: each `clustr-clientd` opens a long-lived
  WS, the hub routes pushed messages and aggregates heartbeats.
- **Notifier digest queue** — coalesces multi-event notifications before SMTP send.

### Embedded subsystems

- **DHCP server** — pure-Go implementation (`github.com/insomniacslk/dhcp`)
  bound to `CLUSTR_PXE_INTERFACE`, allocating from `CLUSTR_PXE_RANGE`.
- **TFTP server** — `github.com/pin/tftp/v3` serving iPXE binaries from
  `CLUSTR_TFTP_DIR`.
- **iPXE chain** — HTTP endpoints `/api/v1/boot/{ipxe,vmlinuz,initramfs.img,
  ipxe.efi,undionly.kpxe}` provide the boot menu and assets.
- **slapd** (optional) — `internal/ldap/slapd.go` orchestrates the system slapd
  daemon, generates its own CA + TLS, owns the DIT, and exposes a `Manager`
  for users / groups / sudoers / project plugins.
- **Shell manager** — `internal/image/shell.ShellManager` brokers PTY shells
  inside chroots for image-edit workflows; piped to the browser via
  `/api/v1/.../shell` WebSocket.

---

## Provisioning lifecycle

The end-to-end "blank machine → running compute node" flow:

1. **Register** — Either an operator clicks **Add Node** in the UI (or
   `clustr node add ...` via CLI) and provides hostname + primary MAC, or the
   node is allowed to PXE-boot first and registers itself when the iPXE
   script hits `/api/v1/nodes/register`.
2. **Discover** — On first PXE boot, the clustr-built initramfs (kernel + busybox
   userland + statically linked `clustr` CLI) gathers hardware inventory
   (`internal/hardware/`) — CPU, memory, NICs, IPoIB / RDMA fabric (Mellanox,
   OPA), RAID controllers, GPUs, BMC info — and POSTs it to `/api/v1/nodes/{id}`.
3. **Plan** — Operator (or auto-policy) assigns an image, disk layout, network
   config, and (optionally) a node group. Auto-policy
   (`internal/allocation/auto_policy.go`) can create projects + grants
   automatically when an admin enables it.
4. **Reimage** — `internal/reimage/orchestrator.go` flips the node to PXE-first
   in the BMC (`ipmitool chassis bootdev pxe`), reboots it, the node netboots
   the clustr installer, the installer fetches the chosen image blob from
   `/api/v1/images/{id}/blob`, writes the disk layout, and deploys.
5. **Verify** — After install, the orchestrator flips the node back to
   disk-first (`flipNodeToDiskFirst`), waits up to `CLUSTR_VERIFY_TIMEOUT`
   (default 5 m, min 2 m, max 30 m) for the node to come back on its
   normal IP, and runs the post-reboot verifier (`024_post_reboot_verification.sql`).
6. **Heartbeat** — `clustr-clientd` is installed in the image; on first boot it
   reads `/etc/clustr/node-token` + `/etc/clustr/clustrd-url`, opens a WebSocket,
   and starts heartbeating. From this point the node is "managed".

The **group reimage** path (`internal/reimage/group.go`) is a fan-out version
of the same state machine with concurrency caps and per-node state persistence,
intended for "reimage all of partition `gpu-h100`" operations.

---

## Image system

Images are **bootable disk artifacts** with metadata. The factory
(`internal/image/factory.go`) ingests them several ways:

| Ingestion path | Endpoint(s) | Notes |
|---|---|---|
| Pull URL (HTTP/HTTPS) | `POST /api/v1/factory/pull`, `POST /api/v1/images/from-url` | Resumable downloads (`018_image_build_resumable.sql`). |
| Resumable upload (TUS) | `POST /api/v1/uploads/`, `PATCH /api/v1/uploads/{id}` | Chunked upload using the TUS 1.0 protocol. |
| Local file | `POST /api/v1/images/from-local-file` | Pick a file already on the server. |
| ISO ingest | `POST /api/v1/factory/import-iso`, `POST /api/v1/factory/probe-iso`, `POST /api/v1/factory/build-from-iso` | Inspect an ISO, then build a bootable image from it. |
| Image capture | `POST /api/v1/factory/capture` | Snapshot a running node's disk into an image. |

Each image gets:

- A **disk layout** (`013_image_roles.sql`, `005_bmc_ib_config.sql`,
  `cloner_disk_layout.schema` analog under `internal/image/layout/`).
- A **role** (`013_image_roles.sql`) — `compute`, `head`, `login`, `storage`.
- **Tags** (`023_image_metadata.sql`).
- **Firmware metadata** (`016_image_firmware.sql`, `026_node_detected_firmware.sql`).
- A **build progress stream** at `/api/v1/images/{id}/build-progress/stream` (SSE).
- A **build log** + signed **build manifest** at `/api/v1/images/{id}/build-{log,manifest}`.
- An **edit shell** — `/api/v1/.../shell` opens a chrooted PTY into a writable
  copy of the image so an operator can `dnf install`, edit `/etc/`, etc.,
  with all changes captured into a new image revision.

The **initramfs** that PXE-boots installer nodes is built by
`internal/initramfs/kernel.go` + `scripts/build-initramfs.sh`. It contains the
clustr CLI statically linked plus busybox and a small init script
(`scripts/initramfs-init.sh`).

---

## Slurm bundling

`internal/slurm/` is the largest non-server subsystem. It builds, signs, and
serves Slurm RPMs as **bundles**.

- `versions.yml` (`build/slurm/versions.yml`) declares which Slurm versions
  clustr supports, plus per-version dep matrix (`deps_matrix.json`).
- `internal/slurm/builder.go` runs the bundle build inside a controlled
  environment (the systemd `clustr-builders.slice` cgroup) and produces a
  versioned tarball (e.g. `v24.11.4-clustr5.tar.gz`).
- The bundle is signed with the **Slurm bundle key**
  (`build/slurm/keys/clustr-release.asc.pub`) — a different trust domain from
  the package signing key, so they can be rotated independently
  (see `PACKAGING.md` "Why not reuse the Slurm bundle key?").
- `internal/slurm/manager.go` exposes admin endpoints under
  `/api/v1/admin/slurm/...` for build / list / install / upgrade / repo URL
  management. The web UI surfaces this on the **Slurm** route.
- `clustr-clientd` consumes Slurm install/upgrade tasks and runs them on the
  target node via `slurminstall.go` / `slurmupgrade.go`.
- `cv-* equivalent` mapping is intentional: clustr's `bundle install` /
  `bundle list` (CLI subcommand on `clustr-serverd`) replaces the Python
  `cv-slurmadm download / install-{compute,server,submit}` workflow with a
  signed-bundle-first model.

---

## Identity (LDAP, system accounts, sudoers)

`internal/ldap/` ships an **embedded slapd** lifecycle:

- On first config, clustr provisions a self-managed `slapd` instance under
  `/var/lib/clustr/ldap/` (data) + `/etc/clustr/ldap/` (cn=config + TLS) +
  `/etc/clustr/pki/` (CA).
- Modes: **Internal** (clustr is the directory) or **External** (clustr binds
  to an existing directory) — `080_ldap_source_mode.sql`.
- The post-install script grants the `ldap` system user membership in the
  `clustr` group so slapd can traverse the data parent directory. See
  `PACKAGING.md` for the full rationale.
- TLS is bootstrapped by `internal/ldap/cert.go`; the bind password is
  encrypted at rest with the master key (`038_ldap_credential_encryption.sql`).
- A **write-bind** identity (`079_ldap_write_bind.sql`) is used for managed
  writes; admin creds are stored encrypted (`028_ldap_admin_passwd.sql`).
- A **project plugin** (`069_ldap_project_plugin.sql`) extends entries with
  PI-portal data.

`internal/sysaccounts/` reconciles **per-node Unix user/group state** against
the directory: which accounts should exist where, with what UID/GID. POSIX
UID/GID allocation is centralised in `internal/posixid/`
(`081_posixid_config.sql`, `084_posixid_roles.sql`) so the same user gets a
deterministic UID across the cluster.

`internal/ldap/sudoers.go` + `036_sudoers.sql` + `077_node_sudoers.sql` manage
**per-node sudoers** entries via the LDAP sudoers schema, surfaced in the
node-detail Sheet of the web UI.

`internal/server/handlers/identity_groups.go` + `078_identity_groups.sql`
adds first-class identity groups distinct from POSIX groups.

---

## Power & IPMI

Power management is provider-pluggable (`internal/power/`):

- `internal/power/ipmi/provider.go` — talks IPMI over the network via
  `ipmitool`. Default and only provider in production.
- `internal/power/proxmox/provider.go` — Proxmox VE provider for VM testing /
  Lab use (531 LoC).
- The **Power registry** in `internal/power/` chooses the provider per-node
  (`076_node_provider.sql`).

A **PowerCache** (`internal/server/powercache.go`) coalesces frequent power-state
polls into a single periodic refresh per node so the UI's "X nodes online"
counter doesn't IPMI-storm a 1000-node cluster.

The CLI ships local IPMI commands too (`clustr ipmi {status, power, configure,
pxe, sensors, test-boot-flip-direct}`) — these run from the operator's
workstation against an `ipmitool`-style host/user/pass.

BMC credentials are encrypted at rest with the master key
(`039_bmc_credential_encryption.sql`).

---

## Storage layout on disk

When installed via the RPM:

```
/etc/clustr/
  clustr-serverd.conf          # EnvironmentFile (config|noreplace)
  secrets.env                  # CLUSTR_SESSION_SECRET (operator-created, 0400)
  secret-master.key            # Encryption key for secrets-at-rest (0400)
  ldap/                        # slapd cn=config + TLS
  pki/                         # CA key + cert for LDAP TLS

/var/lib/clustr/
  db/clustr.db                 # SQLite primary database
  images/                      # Image blobs (50 GB+ for a small library)
  boot/                        # vmlinuz + initramfs
  tftpboot/                    # ipxe.efi, undionly.kpxe (TFTP roots)
  repo/                        # Bundled Slurm repo (signed)
  ldap/                        # slapd MDB + backups (root:clustr 0750)
  ldap/data/                   # The DIT itself (ldap:ldap 0700)
  log-archive/                 # Log purge summaries
  tmp/                         # Build / capture scratch

/var/log/clustr/               # Service logs (also captured by journald)

/usr/sbin/clustr-serverd       # Server binary
/usr/bin/clustr                # CLI binary
/usr/lib/systemd/system/clustr-serverd.service
```

The `clustr` system user/group is created by the post-install script
(`scripts/pkg-postinstall.sh`). The **service runs as root** because nspawn
imaging, loop-device mounts, raw socket DHCP, and CAP_NET_RAW for the TFTP
server collectively make a `User=clustr` setup not viable today; the `clustr`
group is a file-ownership convention, not an effective drop. See `PACKAGING.md`.

---

## Configuration model

`internal/config/config.go` is **environment-variable based**. The full set:

```
CLUSTR_LISTEN_ADDR              :8080                       (HTTP bind)
CLUSTR_DB_PATH                  /var/lib/clustr/db/clustr.db
CLUSTR_IMAGE_DIR                /var/lib/clustr/images
CLUSTR_REPO_DIR                 /var/lib/clustr/repo
CLUSTR_BOOT_DIR                 /var/lib/clustr/boot
CLUSTR_TFTP_DIR                 /var/lib/clustr/tftpboot
CLUSTR_LOG_ARCHIVE_DIR          /var/lib/clustr/log-archive
CLUSTR_BIN_PATH                 /usr/sbin/clustr-serverd
CLUSTR_CLIENTD_BIN_PATH         /usr/local/bin/clustr-clientd

CLUSTR_PXE_ENABLED              false                       (master switch)
CLUSTR_PXE_INTERFACE            (required if enabled)
CLUSTR_PXE_SERVER_IP            (required if enabled)
CLUSTR_PXE_RANGE                10.99.0.100-10.99.0.200
CLUSTR_PXE_SUBNET_CIDR          24                          (1..30)

CLUSTR_LDAP_DATA_DIR            /var/lib/clustr/ldap
CLUSTR_LDAP_CONFIG_DIR          /etc/clustr/ldap
CLUSTR_LDAP_PKI_DIR             /etc/clustr/pki

CLUSTR_LOG_LEVEL                info                        (debug|info|warn|error)
CLUSTR_LOG_RETENTION            7d                          (0 = default)
CLUSTR_LOG_MAX_ROWS_PER_NODE    50000                       (0 = default)
CLUSTR_AUDIT_RETENTION          90d                         (0 = default)
CLUSTR_VERIFY_TIMEOUT           5m                          (clamped to [2m, 30m])

CLUSTR_SESSION_SECRET           (required, in secrets.env)  (HMAC for cookies)
CLUSTR_SESSION_SECURE           0                           (1 if behind TLS)
CLUSTR_SECRET_MASTER_KEY_PATH   /etc/clustr/secret-master.key
CLUSTR_AUTH_DEV_MODE            (dev only)
CLUSTR_AUTH_TOKEN               (dev/legacy)
```

There is also a JSON-file mode (`Load(path)`) kept around for legacy callers
and the `CLUSTR_CONFIG` escape hatch, but the systemd unit ships
`EnvironmentFile=/etc/clustr/clustr-serverd.conf` and `secrets.env`. All env
vars have sane defaults — the only required values for a real deployment are
`CLUSTR_SESSION_SECRET` and (if enabling PXE) the three `CLUSTR_PXE_*` values.

---

## Web UI

`web/` is a separate Vite + React 19 + TypeScript app:

- **Stack**: React Router, TanStack Query, Tailwind v4, shadcn/ui (Radix
  primitives), Lucide icons, Sonner toasts, Vitest for tests.
- **Routes**: `login`, `set-password`, `setup`, `nodes`, `images`, `identity`,
  `groups`, `slurm`, `settings`, `activity`.
- **Components**: `AppShell` (top bar + nav), `CommandPalette` (Cmd-K),
  `ImageShell` (in-image edit terminal), `SessionGate` (auth wall),
  `StatusDot`, `UserPicker`, plus the shadcn `ui/` primitive layer.
- **Build artifact**: `web/dist/` is copied to `internal/server/web/dist/` and
  embedded into the Go binary at compile time via `embed.FS`. This bundle is
  **not** committed — every build path must rebuild it (see PACKAGING.md
  "Web Bundle — Build Artifact Policy").
- **Auth**: cookie session set by `/api/v1/auth/login`. `bootstrap-admin`
  creates the initial `clustr/clustr` admin and forces a password change on
  first login.

---

## Database

SQLite via `modernc.org/sqlite` (pure-Go, no CGO). Single-writer, multi-reader
with WAL mode. Some tables track contention rate
(`Server.lastContentionRate`) to surface lock pressure on `/health`.

84 sequential SQL migrations live in `internal/db/migrations/`, embedded with
`embed.FS` and applied at startup. Migrations are append-only — once a number
is taken it stays taken; corrections happen as new migrations
(e.g. `014_fix_completed_at_type.sql`, `041_node_tags_rename.sql`,
`048_drop_group_id.sql`).

Major schema areas:

- **Nodes & hardware** — `001_initial.sql`, `004_node_hardware_profile.sql`,
  `005_bmc_ib_config.sql`, `032_node_heartbeats.sql`, `076_node_provider.sql`.
- **Images** — `001`, `013_image_roles.sql`, `016_image_firmware.sql`,
  `018_image_build_resumable.sql`, `019_initramfs_builds.sql`,
  `023_image_metadata.sql`, `040_image_status_check.sql`.
- **Reimage** — `008_reimage_requests.sql`, `009_reimage_pending.sql`,
  `010_node_deploy_timestamps.sql`, `022_reimage_terminal_state.sql`,
  `024_post_reboot_verification.sql`, `046_reimage_inject_vars.sql`.
- **Identity & access** — `015_api_keys.sql`, `017_node_scoped_keys.sql`,
  `020_api_keys_rotation.sql`, `025_users.sql`, `044_audit_log.sql`,
  `053_viewer_role.sql`, `055_pi_role.sql`, `058_users_role_check.sql`,
  `059_director_role.sql`, `075_gpg_keys.sql`, `078_identity_groups.sql`.
- **LDAP module** — `027_ldap_module.sql`, `028_ldap_admin_passwd.sql`,
  `038_ldap_credential_encryption.sql`, `069_ldap_project_plugin.sql`,
  `070_node_group_ldap_restrictions.sql`, `079_ldap_write_bind.sql`,
  `080_ldap_source_mode.sql`, `082_node_ldap_ready.sql`.
- **Network** — `030_network_module.sql`, `031_switch_discovery.sql`.
- **Slurm** — `033_slurm_module.sql`, `034_slurm_extended.sql`,
  `035_slurm_sprint8.sql`, `050_slurm_repo_url.sql`,
  `051_slurm_slurmdbd_managed.sql`, `052_slurm_config_files_clustr_default.sql`,
  `083_slurm_repo.sql`.
- **Portal (PI / Director)** — `056_node_group_pi.sql`,
  `057_pi_expansion_requests.sql`, `060_grants.sql`, `061_publications.sql`,
  `062_review_cycles.sql`, `063_smtp_config.sql`,
  `064_allocation_change_requests.sql`, `065_fields_of_science.sql`,
  `066_attribute_visibility.sql`, `067_user_notification_prefs.sql`,
  `068_node_group_expires_at.sql`, `071_project_managers.sql`.
- **Auto-policy** — `072_auto_compute_policy.sql`, `073_auto_policy_config.sql`.
- **Misc** — `037_node_logs_summary.sql`, `045_webhooks.sql`,
  `047_node_config_history.sql`, `074_tech_trig_state.sql`.

---

## Authentication & authorization

Two credential types:

1. **Browser sessions** — `POST /api/v1/auth/login` returns a Set-Cookie carrying
   an HMAC-signed session ID. `force_password_change` is honoured on first login
   from `bootstrap-admin`.
2. **API keys** — `Authorization: Bearer <token>`. Created via
   `/api/v1/admin/api-keys`. Optionally **node-scoped** (the key only authorises
   actions on a specific node, used for clientd) and **rotatable** with
   overlap windows.

Roles: `admin`, `director`, `pi`, `viewer`. Some endpoints additionally
require **scope** (`requireScope(true)` — typically "this is an admin scope,
not a portal scope"). The PI portal endpoints under `/api/v1/portal/pi/...`
and the Director endpoints under `/api/v1/portal/director/...` are partitioned
namespaces with their own auth gates.

The CLI authenticates via `--token` / `CLUSTR_TOKEN` and `--server` /
`CLUSTR_SERVER`.

---

## Build, package, release

- **Local build**: `make all` (web + binaries) or `make web` / `make server`.
  Binaries land in `bin/`.
- **CI**: `.github/workflows/ci.yml` runs `go test`, `golangci-lint`, `gosec`,
  Vitest, and a build smoke. `lab-validate.yml` runs against the lab.
- **Release**: `release.yml` on tag push builds amd64 + arm64 binaries, runs
  `nfpm pkg --packager rpm` per `{el8,el9,el10}`, signs every RPM with the
  release GPG key, runs `createrepo_c`, signs `repomd.xml`, and rsyncs the
  repo tree to `pkg.sqoia.dev` (a Caddy vhost on the Sqoia Linode).
- **Initramfs**: `initramfs.yml` builds the PXE installer initramfs.
- **iPXE**: `ipxe-build.yml` builds the iPXE EFI/legacy binaries with the
  embedded chainload script.
- **Slurm**: `slurm-build.yml` builds new Slurm bundles when `versions.yml`
  changes.

`PACKAGING.md` is the authoritative reference for everything signing- and
distribution-related: GPG fingerprint, secret storage, repo layout on
`pkg.sqoia.dev`, the cloner dev-host policy, and the verification log.

---

## Glossary

- **Bundle** — A signed, versioned tarball of Slurm RPMs (e.g.
  `v24.11.4-clustr5`) consumable by clustr-clientd's installer.
- **clientd** — The node-side agent (`clustr-clientd`). One per managed node.
- **Cloner** — The internal dev host (192.168.1.151) running
  `clustr-autodeploy`. Source-built, never RPM-installed.
- **DIT** — Directory Information Tree (LDAP).
- **Factory** — The image ingestion subsystem (`internal/image/factory.go`).
- **Group reimage** — Bulk reimage of a node group (rolling, capped concurrency).
- **iPXE** — The bootloader chain-loaded after PXE; serves the clustr boot menu.
- **Master key** — Symmetric key at `/etc/clustr/secret-master.key` used to
  encrypt LDAP/BMC creds and other secrets at rest.
- **nspawn** — `systemd-nspawn`. Used by the image factory for isolated
  package operations during build.
- **PI portal** — Self-service area for Principal Investigators to manage
  their groups, members, grants, publications, and allocation requests.
- **Privhelper** — `clustr-privhelper`, the narrow-surface root broker.
- **Reimage orchestrator** — State-machine-based reimage driver
  (`internal/reimage/orchestrator.go`).
- **Tech trigger** — A deferred administrative action queued in the DB and
  executed by `tech_trig_worker.go`.
- **TUS** — `tus.io` resumable upload protocol used for ISO ingest.
