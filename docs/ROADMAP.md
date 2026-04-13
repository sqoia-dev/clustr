# clonr Product Roadmap

**Document status:** Living technical vision. Decisions marked [LOCKED] reflect architectural commitments already encoded in the codebase and API contract. Scope boundaries are defined in ADR-0005 and are not negotiable without a new ADR.

**Scope:** See ADR-0005. clonr owns provisioning-time operations — from bare metal to a running, cluster-joined node. Day-2 operations (scheduler tuning, user lifecycle management, patch management on running nodes) belong in your configuration management toolchain.

---

## Vision

An HPC sysadmin should be able to unbox 200 servers, plug in a provisioning cable, and reach `sbatch hello-world.sh` without writing a single Ansible playbook or reading a 200-page deployment guide. clonr does this by combining node imaging with cluster-aware first-boot automation: one tool that images the OS, configures the network, joins the scheduler, mounts storage, and validates the result — all driven from a single server binary with an embedded UI and a declarative node inventory. It is not a platform; it is a sharp tool for the one operation that every HPC deployment team dreads.

---

## Personas

Three personas drive the design. Dinesh owns the UI detail; this section defines what each persona accomplishes end-to-end in clonr.

**Junior sysadmin** — Has a rack of servers and a CSV from procurement. Can run commands but does not know DHCP internals or Slurm configuration. With clonr, they: import the CSV to populate node inventory, boot nodes to PXE, watch the dashboard as nodes image themselves, and hand a running cluster to the team lead. They do not need to know what iPXE is.

**Senior HPC admin** — Owns the cluster lifecycle. Builds and versions golden images via chroot customization. Manages node roles and groups. Triggers rolling reimages on compute partitions. Defines first-boot hooks for site-specific configuration (custom module paths, site license servers, IB fabric config). Uses the SLURM drain/resume integration to reimage nodes without disrupting running jobs. This person is clonr's primary user and the bar for feature completeness.

**Research engineer / lab manager** — Runs a small cluster (8-32 nodes) with no dedicated sysadmin. Needs a turn-key path: one command to stand up a cluster from bare metal. Wants the monitoring agent dropped automatically. Does not want to configure DHCP, TFTP, or PXE manually. For this persona, the "cluster recipe" feature (v1.1) is the entry point.

---

## Phases

### v0.x (Now) — Core Pipeline Proven

All core machinery exists and runs: PXE boot, DHCP/TFTP via dnsmasq, image management, REST API, embedded UI, hardware discovery, IPMI integration, InfiniBand support, block and filesystem deploy modes.

What it is not yet: trusted in production, operable at fleet scale, or observable. Several correctness bugs exist (target disk selection, silent error suppression, non-reproducible checksums, no deployment rollback). No auth model beyond a single pre-shared token.

This phase ends when Sprint 1 and Sprint 2 ship.

---

### v1.0 "Ready" — Turn-Key for 50 Nodes

**Goal:** A real 50-node compute cluster can be stood up end-to-end by a single sysadmin using clonr alone, with auth, audit, metrics, integrity verification, and first-boot cluster integration. No manual post-deploy steps required for the standard HPC stack.

This is the version that gets shown to the world. It must actually work on real hardware under real conditions.

**Sprint 1 — Stop the Bleeding (correctness + security foundation):**

- Correct target disk selection with type constraints and explicit override (`TargetDiskHint` in NodeConfig)
- Deployment rollback: on failure, set IPMI boot device to PXE and post a structured failure event
- Deployment integrity verification: post-deploy checksum verification against stored BaseImage.Checksum
- Silent error elimination: audit all `_ = err` in deploy/finalize paths; `FinalizeResult` type with warnings
- Download resume + per-request timeouts: Range header support, configurable timeout (default 30 min), exponential backoff
- Remove dead `internal/db/schema.go` stub
- TLS for clonr-serverd: self-signed on first run, bring-your-own cert supported
- API key auth with two scopes (`admin`, `node`): SHA-256 hashed storage, rotation via CLI (ADR-0001)
- AES-256-GCM envelope encryption for BMC credentials and kickstart secrets (ADR-0002)
- Structured deploy event enrichment: node hardware snapshot + image checksum at deploy time
- Config validation on startup: fail fast if ImageDir unwritable, TFTP inaccessible, SQLite unopenable
- API versioning enforcement: `Accept: application/vnd.clonr.v1+json`, `API-Version` response header

**Sprint 2 — Turn-Key Surface (first-boot cluster integration):**

- Node groups as first-class objects: `node_groups` + `node_group_members` tables, group-targeted deploy/reboot/assign-image API
- Bulk node import from CSV: `POST /api/v1/nodes/bulk` accepting CSV format `hostname,mac,ip,bmc_ip,group`
- Node roles: built-in role definitions (head-node, compute, gpu-compute, storage, login); role-driven service enable/disable; role assignment API
- First-boot network config: DNS registration, /etc/hosts update, VLAN tagging, NIC bonding config written to deployed OS during Finalize
- First-boot NTP/chrony config pushed to deployed OS
- First-boot NFS/autofs config pushed to deployed OS
- LDAP/SSSD client join hook in Finalize phase: config-driven, not hard-coded; connects to an existing LDAP server
- Scheduler join hook: SLURM `scontrol update State=DRAIN` before deploy, `State=RESUME` after successful deploy; PBS backend as second implementation; config-driven, not required
- Monitoring agent deployment: node_exporter (or configurable agent binary) dropped during Finalize as a role-driven step
- Rolling deployment with concurrency control: `deploy_jobs` + `deploy_job_nodes` tables; configurable concurrency; server queues and signals nodes via IPMI PXE boot
- Prometheus metrics endpoint: node counts, image counts, deploy duration histogram, blob stream throughput gauge
- Integration test suite on Proxmox lab (CI gate for v1.0 release)

**v1.0 MVP Turn-Key Checklist** — the definition of done:

- [ ] A 50-node cluster can be imaged from bare metal to running OS without manual post-deploy steps
- [ ] Deploy success rate >99% on a 50-node test run (measured in CI)
- [ ] Zero panics over a 24-hour load test against the provisioning server
- [ ] Blob stream throughput >= 200 MB/s aggregate with 20 concurrent clients on 1 GbE
- [ ] A failed deployment always results in IPMI boot device reset to PXE (verified in test)
- [ ] Post-deploy checksum verification passes or the deployment is reported as failed
- [ ] BMC credentials are stored encrypted in the database (AES-256-GCM, master key outside DB)
- [ ] Admin and node API key scopes are enforced: a node-scoped key cannot reach admin endpoints
- [ ] `clonr key rotate` generates a new admin key and invalidates the old one without touching the node-scoped key
- [ ] First-boot network config (static IP, hostname, /etc/hosts) is applied correctly on deployed nodes
- [ ] SLURM drain/resume hook fires correctly around a deploy job (verified with a real slurmctld in lab)
- [ ] NFS autofs config is present and mountable on deployed nodes
- [ ] NTP/chrony config is present and synchronized on deployed nodes
- [ ] node_exporter is running on deployed nodes with metrics reachable from the provisioning server
- [ ] Bulk CSV import of 50 nodes completes without error and all nodes are reachable via MAC lookup
- [ ] A rolling reimage of a 50-node group completes with configurable concurrency (tested at 5, 10, 20)
- [ ] All API endpoints return `API-Version` header
- [ ] `clonr-serverd --validate-config` fails fast with a clear error on misconfiguration

---

### v1.1 "Scale" — 500 Nodes, Multicast, OIDC

**Goal:** clonr can manage a 500-node cluster with concurrent fleet operations, multicast image distribution, and optional enterprise identity integration.

- udpcast multicast as primary distribution path: line-rate simultaneous imaging for full-cluster deploys (ADR-0003)
- OIDC as optional admin authentication overlay: Keycloak, Dex, Active Directory (ADR-0001)
- Redfish power management alongside IPMI: vendor-agnostic BMC control
- PostgreSQL as optional backend: DSN configuration, dialect-neutral migration shim (ADR-0004)
- Multi-cluster namespaces: a single clonr-serverd instance can manage logical clusters with independent node inventories and image stores
- Cluster recipe: a declarative YAML spec (`ClusterRecipe`) that maps node groups to images and roles, driving full cluster bring-up from one command (`clonr cluster provision --recipe cluster.yaml`)
- LDAP server deployment on head node: OpenLDAP/389DS stand-up, DIT population from clonr user store, client config distribution
- SLURM full deployment: slurm.conf generation from hardware profiles, munge key distribution, slurmdbd + MariaDB, initial accounting structure
- Node hardware inventory aggregation: aggregate API, mismatch detection, hardware diff endpoint
- NodeConfig templating: `NodeTemplate` with variable substitution for rack-scale uniform configs
- PBS/Torque integration backend
- Parallel SSH execution: `clonr exec --group <name> -- <command>`
- Node tagging and search
- Image versioning with parent lineage graph; `parent_image_id` in schema

---

---

### Multi-disk and RAID Support

Code-level analysis findings from parallel clone test (2026-04-12). These inform sequencing across v1.0, v1.1, and Sprint 2.

**RAID1 — fully wired end-to-end** ✅ v1.0  
RAID1 is complete from recommender through deploy. The recommender heuristic in `pkg/image/layout/recommend.go` identifies two-disk same-size configurations and emits a RAID1 layout. `deploy/raid.go` assembles the array with `mdadm` and hands it to the filesystem deploy path. No gaps for the two-disk case.

**RAID5 / RAID6 / RAID10 — mechanically supported, no recommender heuristic** — v1.1 gap  
`deploy/raid.go` has the mdadm invocations and parity logic for RAID5, RAID6, and RAID10. The recommender switch at `pkg/image/layout/recommend.go:121-143` has no cases for these levels — an operator can request them via explicit layout spec but the server will never auto-recommend them. This is the right conservative default for v1.0; the heuristics need to be added in v1.1 once we have real multi-disk test data.

**"Leave non-OS disks untouched" policy — operator UX gap** — Sprint 2 ticket  
There is currently no way for an operator to declare that `sdb`, `sdc`, etc. should not be touched during deploy. The recommender will attempt to use all discovered disks. For nodes with existing data volumes, scratch disks, or dedicated swap partitions, this is a correctness risk. Recommended fix: add a `DiskRole` annotation to `NodeConfig` (e.g., `role: os | data | scratch | skip`) or an explicit opt-in flag per disk. Until this is in place, operators must ensure only the target OS disk is visible during PXE deploy.

**`identicalSize` helper needs an N-disk variant** — Sprint 2 ticket, S effort  
The current helper checks two-disk size equality. Unlocking three- and four-disk heuristics (RAID5, RAID6, RAID10 auto-recommend) requires an N-disk variant. Straightforward change, gates the v1.1 recommender expansion.

**Bare-metal SCSI transport detection gap** — v1.1  
`pkg/hardware/disk.go` does not distinguish SCSI transport variants (SAS vs. SATA vs. NVMe-over-Fabric). Disk ordering and recommended layout can differ materially between transport types on real HPC hardware. The gap is benign for the lab (all virtio/SATA), but needs closing before production bare-metal validation at the v1.1 scale target.

---

### v1.2 "Polish" — Hardening and Enterprise Readiness

- Delta images: content-addressed overlay layers (server-side assembly, node receives full rootfs)
- Terraform provider for clonr: manage node configs and images as infrastructure code
- Grafana dashboard bundle: pre-built dashboards for deploy metrics, hardware health, fleet status
- FIPS 140-2 compliance mode: AES-256-GCM with FIPS-validated Go crypto build
- Air-gapped install bundle: offline installer with all dependencies bundled (dnsmasq, iPXE, node_exporter)
- PDU integration: power strip control for nodes without IPMI
- Automated patching pipeline: `POST /api/v1/images/:id/patch` — chroot + `dnf upgrade` + validate + version bump
- Image promotion workflow: draft → testing → staging → production lifecycle stages
- Clonrfile: declarative image build spec (Dockerfile-like, runs in chroot)

---

### v2.x — Multi-Site and Advanced Image Management

Not sequenced. Candidates:

- Multi-site federation: multiple clonr-serverd instances with shared image registry and centralized inventory
- Delta image distribution over BitTorrent: fully distributed, no server NIC bottleneck, for WAN deployments
- Active-active HA for clonr-serverd: Raft-based consensus for the metadata layer
- OCI image export: `GET /api/v1/images/:id/export?format=oci` — bare-metal to container parity
- Image content search: audit "which images have this package/config?"
- Hardware health monitoring with threshold alerting and Prometheus IPMI exporter integration

---

## What Is Out of Scope

See ADR-0005 for the full boundary. The short version: clonr gets nodes into the cluster. Configuration management, day-2 scheduler tuning, user lifecycle management, and storage array provisioning are your configuration management tool's job. We will decline feature requests that belong on the other side of that line.
