> **SUPERSEDED — Sprint 1 plan only. See [sprint-2-plan.md](sprint-2-plan.md) for Sprint 2 scope and [sprint-3-plan.md](sprint-3-plan.md) for the current active sprint. This file is kept for historical record.**
>
> Last Verified: 2026-04-13 — applies to clonr main @ fbccab9 (Sprint 1 baseline)

# clonr Sprint Plan — v1.0 "Ready"

**Last updated:** 2026-04-13
**Framing:** Two sprints, two weeks each. Sprint 1 makes clonr trustworthy. Sprint 2 makes it turn-key. Items are ruthlessly scoped — 10 done beats 25 half-done. Everything not in Sprint 1/2 is queued in the v1.1 backlog in ROADMAP.md.

**Source codes:** P0/P1/P2 = gap analysis priority; A/B/C/D = Gilfoyle HPC-ops tier; UI = Dinesh audit item.

---

## Sprint 1 — Stop the Bleeding (2 weeks)

**Goal:** A deployment that fails does not leave the cluster in an unknown state. Auth is real. Secrets are encrypted. The server can be trusted to run unattended.

Every item in Sprint 1 is a correctness or security fix. None are features.

| # | Item | Owner | Effort | Source | Notes |
|---|------|-------|--------|--------|-------|
| S1-01 | Correct target disk selection: type constraints (NVMe/SATA/SAS) + `TargetDiskHint` in NodeConfig + explicit confirmation when existing partition table present | Dinesh | M | P0, A1 | Correctness bug. Multi-disk nodes are broken today. |
| S1-02 | Deployment rollback: on failure, reset IPMI boot device to PXE and post structured failure event with full context | Dinesh | M | P0, A2 | Wiring work — IPMI integration already exists. |
| S1-03 | Post-deploy integrity verification: canonicalize checksumDir (alphabetical walk), verify deployed rootfs against BaseImage.Checksum before reporting success | Dinesh | M | P0 | Depends on S1-01 — checking wrong disk is worse than no check. |
| S1-04 | Silent error elimination: audit all `_ = err` in deploy + finalize paths; implement `FinalizeResult` with warnings list; `applyKernelArgs` failure is fatal | Dinesh | S | P0 | One afternoon of auditing + type work. |
| S1-05 | Download resume + timeouts: Range header support in block deployer, 30-minute default request timeout (configurable), exponential backoff, max retry count | Dinesh | S | P0, B1 | `progressReader` already tracks bytes for Range construction. |
| S1-06 | Remove dead `internal/db/schema.go` stub (stub schema diverges from real pkg/db schema) | Dinesh | XS | P0 | Delete it. No migration needed. |
| S1-07 | TLS for clonr-serverd: self-signed cert on first run, bring-your-own cert/key config; PXE path stays HTTP | Dinesh | S | P0, B2 | `http.Server` wiring + crypto/tls cert generation. |
| S1-08 | API key auth (ADR-0001): two scopes (admin, node), SHA-256 hashed storage, `api_keys` migration, scope enforcement middleware, `clonr key create/rotate/list` CLI | Dinesh | M | P0, A3 | Replaces single pre-shared token. Node-scoped key goes in initramfs build. |
| S1-09 | AES-256-GCM envelope encryption (ADR-0002): master key file on first run, encrypt/decrypt wrappers in pkg/db, migrate BMC password fields, `clonr migrate encrypt-secrets` | Dinesh | M | P1, B3 | Standard library only (crypto/aes, crypto/cipher). No CGO. |
| S1-10 | Structured deploy event enrichment: add `node_hardware_snapshot` (JSON) and `image_checksum_at_deploy` to deploy_events migration | Dinesh | XS | P1 | Migration + handler update. Additive schema change. |
| S1-11 | Config validation on startup: fail fast with clear error if ImageDir unwritable, TFTP dir inaccessible, SQLite unopenable, master key file unreadable | Dinesh | XS | P1 | One-time check in main() before serving. |
| S1-12 | API versioning: `Accept: application/vnd.clonr.v1+json` + `API-Version` response header on all endpoints | Dinesh | XS | P1 | Cheap now, expensive to retrofit later. |

**Sprint 1 total:** 5 M items, 4 S items, 3 XS items. Approximately 12-15 engineering days. Tight but achievable.

**Sprint 1 exit criteria:**
- All `_ = err` audit items resolved (grep-verifiable)
- API key scope enforcement: integration test proves node-scoped key gets 403 on admin endpoints
- AES-256-GCM: database contains no plaintext BMC credentials (verifiable via sqlite3 inspection)
- TLS: curl to server port returns valid cert response
- Disk selection: multi-disk test node deploys to the correct disk (verified in Proxmox lab)

---

## Sprint 2 — Turn-Key Surface (2 weeks)

**Goal:** A 50-node cluster can be stood up end-to-end from bare metal using clonr alone. First-boot network, storage, identity, scheduler, and monitoring are configured automatically by the deploy pipeline.

Items are ordered by dependency. First-boot hooks depend on the Finalize phase being correct (Sprint 1). Group operations depend on groups existing.

| # | Item | Owner | Effort | Source | Notes |
|---|------|-------|--------|--------|-------|
| S2-01 | Node groups: `node_groups` + `node_group_members` migration, group CRUD API, group-targeted deploy/reboot/assign-image endpoints | Dinesh | M | P1, B4 | Foundation for all fleet operations. |
| S2-02 | Bulk node import from CSV: `POST /api/v1/nodes/bulk`, CSV format `hostname,mac,ip,bmc_ip,group`, single SQLite transaction (all-or-nothing) | Dinesh | S | P1, B5 | Moved from nice-to-have to MVP for turn-key. |
| S2-03 | Node roles (built-in set): `node_roles` + `node_role_assignments` migration; built-in role definitions (head-node, compute, gpu-compute, storage, login); role assignment API; role spec drives service enable/disable list | Dinesh | L | P1 | Head-node/compute distinction is required for first-boot hooks to know what to configure where. |
| S2-04 | First-boot network config: DNS registration (update /etc/hosts + optional DNS API call), VLAN tagging, NIC bonding config, static IP written to NetworkManager keyfiles in deployed OS during Finalize | Dinesh | M | P1, B6 | Config-driven. NodeConfig.Interfaces already has the data. |
| S2-05 | First-boot NTP/chrony: write chrony.conf to deployed OS during Finalize. Config-driven (NTP server list from ServerConfig). | Dinesh | XS | P1, B7 | Simple file injection via ChrootSession.InjectFile. |
| S2-06 | First-boot NFS/autofs: write /etc/auto.master and automount map entries to deployed OS during Finalize. Config-driven (NFS mounts from role spec). | Dinesh | S | P1, B8 | Pulls mount list from role spec. Depends on S2-03. |
| S2-07 | LDAP/SSSD client join hook: during Finalize, if NodeConfig has an LDAP server configured, write sssd.conf + add ldap to nsswitch.conf + enable sssd. Config-driven. Does NOT stand up an LDAP server (that is v1.1). | Dinesh | S | P1, B9 | Connects to existing LDAP. Standing up the server is v1.1. |
| S2-08 | Scheduler join hook: SLURM backend — `scontrol update State=DRAIN` before deploy, `State=RESUME` after success; PBS backend — `pbsnodes -o/-c`. Config-driven `ClusterIntegration` interface. Not required; gracefully no-ops if unconfigured. | Dinesh | M | P1, B10 | Expand the existing post-deploy validation hook to do the actual drain/resume. |
| S2-09 | Monitoring agent deployment: node_exporter binary (or configurable agent path) dropped to deployed OS and systemd unit written during Finalize. Role-driven: compute and gpu-compute roles enable it by default. | Dinesh | S | P1 | Moved from nice-to-have to MVP for turn-key observability. |
| S2-10 | Rolling deploy job: `deploy_jobs` + `deploy_job_nodes` migration; server queues nodes, respects concurrency limit (default 10% of group, min 1); IPMI PXE signals nodes; per-node status tracked; UI shows progress | Dinesh | L | P1, B11 | The mechanism for reimaging a compute partition without doing it 200 times by hand. |
| S2-11 | Prometheus metrics endpoint: `GET /metrics` in Prometheus text format. Metrics: node count by status, image count by status, deploy duration histogram, blob stream throughput gauge, active deploy jobs gauge. | Dinesh | S | P2, C1 | Required for the v1.0 validation gate (throughput SLA). |
| S2-12 | Integration test suite on Proxmox lab: CI job that boots test-node-01 via PXE, deploys Rocky 9, verifies SSH, re-deploys second image, verifies again. Gate on v1.0 release tag. | Gilfoyle | L | P0, A4 | Gilfoyle owns the lab harness. Depends on S1 correctness items. |

**Sprint 2 total:** 2 L items, 4 M items, 4 S items, 1 XS item. Approximately 14-16 engineering days. This is the heavier sprint — S2-03 (roles) and S2-10 (rolling deploy) are the two biggest items.

**Sprint 2 exit criteria:**
- Bulk CSV import: 50 nodes imported, all reachable via MAC lookup API
- Rolling deploy: 50-node group deployed at concurrency=10, all nodes report success, deploy_job_nodes shows 100% completion
- First-boot: a freshly deployed node has correct /etc/hosts, chrony.conf, autofs maps, and sssd.conf (verified by SSH inspection in lab)
- Scheduler hook: SLURM drain fires before reimage, RESUME fires after (verified with real slurmctld in Proxmox lab)
- node_exporter: running and reachable on all deployed nodes
- Prometheus endpoint: `GET /metrics` returns correct node count gauge
- All v1.0 MVP checklist items green (see ROADMAP.md)

---

## Items Explicitly Cut from Sprint 1/2

These were considered and deferred. They are not forgotten — they are sequenced into v1.1.

| Item | Reason for deferral |
|------|---------------------|
| LDAP server deployment on head node | Connecting to existing LDAP (S2-07) covers the v1.0 use case. Standing up the server adds significant complexity and is a v1.1 cluster recipe concern. |
| SLURM full deployment (slurm.conf generation, munge distribution, slurmdbd) | The scheduler join hook (S2-08) is the v1.0 MVP. Full SLURM deployment from scratch belongs in the cluster recipe (v1.1). |
| Cluster recipe (declarative `ClusterRecipe` YAML) | Roles and groups (S2-01, S2-03) are the foundation. The recipe orchestrator is v1.1 after the foundation is validated. |
| udpcast multicast distribution | HTTP range (existing) is sufficient for 50-node v1.0. Multicast is v1.1 (ADR-0003). |
| OIDC authentication | API key scopes (S1-08) cover v1.0. OIDC is v1.1 (ADR-0001). |
| PostgreSQL backend | SQLite is correct through ~500 nodes (ADR-0004). No immediate requirement. |
| NodeConfig templating | CSV import (S2-02) covers the v1.0 bulk-enrollment use case. Templating with variable substitution is v1.1. |
| Hardware inventory aggregation API | Individual hardware profiles are captured. Aggregate query API is v1.1. |
| Image versioning / parent lineage | The image pipeline is correct at v1.0. Lineage graph is v1.1. |
| Parallel SSH execution (`clonr exec`) | Not required for the turn-key deploy path. v1.1. |

---

## Confidence Assessment

Sprint 1: **9/10.** These are correctness and security fixes on a codebase that is mostly working. The risk is scope creep during the `_ = err` audit discovering more issues than expected — budget one extra day for surprises.

Sprint 2: **7/10.** The first-boot hooks (S2-04 through S2-09) are individually simple but their integration surface is wide — a misconfigured sssd.conf or a bonded NIC that does not come up is an invisible failure until boot. The Proxmox integration test (S2-12) is the mitigation. The rolling deploy job (S2-10) is the highest-risk item: it has a non-trivial state machine (queueing, IPMI signaling, per-node status tracking, UI updates). If it runs long, move it to v1.1 and accept that v1.0 rolling deploys are manually orchestrated — the underlying single-node deploy is correct.

**Overall v1.0 confidence: 8/10.** The architecture is sound, the lab exists, and the scope is honest. The failure mode is not "it does not work" — it is "Sprint 2 takes three weeks instead of two." Shipping v1.0 without the rolling deploy job (S2-10) is a valid fallback; everything else in Sprint 2 is achievable in the window.
