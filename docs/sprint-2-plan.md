> **SUPERSEDED — Sprint 2 is complete. See [sprint-3-plan.md](sprint-3-plan.md) for the current active sprint. This file is kept for historical record and exit criteria review.**
>
> Last Verified: 2026-04-13 — applies to clonr main @ a15d762

# clonr Sprint 2 Plan — v1.0 "Ready" Path

**Last updated:** 2026-04-13  
**Sprint duration:** 2 weeks  
**Sprint 1 exit SHA (tag v0.1.0):** e8c86fa  
**Owner legend:** D = Dinesh (code), G = Gilfoyle (infra/ops/lab), R = Richard (architecture review)

---

## 1. Sprint Goal

Sprint 2 converts a correct, secure single-node provisioner (the Sprint 1 outcome) into a turn-key cluster tool. By the end of this sprint, a senior HPC admin can import 50 nodes from a CSV, trigger a rolling group deploy, and reach a cluster where every node has a correct hostname, static IP, NTP config, NFS mounts, SLURM drain/resume integration, and a running node_exporter — without writing a single config file by hand. The build pipeline (F1/F2/F3) ships in parallel to eliminate the ops friction that blocks the Senior HPC Admin persona during day-to-day image management.

---

## 2. Scope

Items are ordered by dependency. F-items are already in flight with Dinesh. S2-items are newly selected from the Sprint 2 roadmap.

| ID | Title | Owner | Effort | Dependency | Notes |
|----|-------|-------|--------|------------|-------|
| **F1** | Initramfs rebuild button: admin UI + progress stream + history | D | M | — | Queued. Eliminates manual CLI step that blocks image iteration. |
| **F2** | Resume button for failed/interrupted image builds | D | S | F1 (shares progress stream infra) | Queued. Correctness: a failed build should never require a full restart. |
| **F3** | Reconcile-on-restart: graceful drain + resume instead of tombstone | D | S | — | Queued. Server restart must not orphan in-flight builds as permanent failures. |
| **S2-01** | Node groups: `node_groups` + `node_group_members` migration, group CRUD API, group-targeted deploy/reimage/assign-image endpoints | D | M | Sprint 1 complete | Foundation for all fleet operations. Every subsequent item depends on groups existing. |
| **S2-02** | Bulk node import from CSV: `POST /api/v1/nodes/bulk`, format `hostname,mac,ip,bmc_ip,group`, single SQLite transaction | D | S | S2-01 | Enables the "unbox and import procurement CSV" day-one flow. Required for 50-node v1.0 checklist gate. |
| **S2-D1** | DiskRole annotation: add `role: os \| data \| scratch \| skip` to NodeConfig per-disk; recommender skips non-OS disks; deploy aborts if no OS disk selected | D | S | — | ROADMAP correctness ticket. Nodes with existing data volumes will be destroyed by the recommender today. Fixes a silent data-loss risk before real hardware validation. |
| **S2-04** | First-boot network config: static IP, hostname, /etc/hosts, NIC bonding written to NetworkManager keyfiles in deployed OS during Finalize | D | M | Sprint 1 Finalize correctness | No cluster runs without proper network identity on every node. NodeConfig.Interfaces already carries the data. |
| **S2-08** | Scheduler join hook: SLURM `scontrol State=DRAIN` before deploy, `State=RESUME` after success; PBS as second backend; config-driven ClusterIntegration interface; no-ops if unconfigured | D | M | S2-04 (node identity must be correct before SLURM registration) | On the v1.0 MVP checklist. Senior HPC Admin will test this in the first lab session. |
| **S2-10** | Rolling deploy job: `deploy_jobs` + `deploy_job_nodes` migration; server queues nodes with configurable concurrency (default 10%, min 1); IPMI PXE signals; per-node status tracked; UI progress view | D | L | S2-01 (groups), S2-08 (drain/resume must fire per node) | The mechanism that makes reimaging a compute partition not a manual 50-step process. |
| **S2-11** | Prometheus metrics endpoint: `GET /metrics` in text format; node counts by status, deploy duration histogram, blob stream throughput gauge, active deploy jobs gauge | D + G | S | S2-10 (deploy job metrics need the job concept) | Required for the v1.0 throughput SLA gate (>=200 MB/s aggregate). Gilfoyle wires the scrape target in lab. |
| **S2-12** | Integration test suite on Proxmox lab: CI job boots test-node-01 via PXE, deploys Rocky 9, verifies SSH, redeploys second image, verifies again; gates v1.0 release tag | G | L | Sprint 1 correctness items, S2-10 | Gilfoyle owns the lab harness. This is the CI gate — v1.0 does not ship without it passing. |

**Sprint 2 total:** 1 L (S2-10), 1 L (S2-12), 4 M (F1, S2-01, S2-04, S2-08), 4 S (F2, F3, S2-02, S2-D1, S2-11). Approximately 15–17 engineering days across D + G. S2-10 is the highest-risk item; see risks below.

### Owner allocation

- **Dinesh:** F1, F2, F3, S2-01, S2-02, S2-D1, S2-04, S2-08, S2-10, S2-11 (backend)
- **Gilfoyle:** S2-11 (lab scrape config), S2-12 (lab harness + CI gate)
- **Richard:** architecture review on S2-10 state machine before implementation begins; DiskRole annotation API shape review before S2-D1 lands

---

## 3. Out of Scope (Explicit Non-Goals)

These were considered and cut. They are not forgotten — they are queued for v1.1.

| Item | Reason |
|------|--------|
| First-boot NTP/chrony config (S2-05) | High-value but XS effort — Dinesh picks it up if S2-04 lands early. Not in the primary scope commitment. |
| First-boot NFS/autofs config (S2-06) | Depends on node roles (S2-03, not in this sprint). Deferred. |
| LDAP/SSSD client join hook (S2-07) | Depends on node roles. Deferred with S2-06. |
| Node roles built-in set (S2-03) | Large item. The SLURM hook (S2-08) can infer role from NodeConfig flag without a full role system. Roles ship in v1.1 with cluster recipes. |
| Monitoring agent deployment (S2-09) | Depends on node roles for per-role enable/disable. Deferred. node_exporter is wired manually in lab for v1.0 validation. |
| RAID5/RAID6/RAID10 recommender heuristics | v1.1. RAID1 is complete. N-disk identicalSize helper is a supporting ticket, also v1.1. |
| SCSI transport detection (SAS vs. SATA vs. NVMe-over-Fabric) | v1.1. Benign in Proxmox lab. |
| Cluster recipe (declarative ClusterRecipe YAML) | v1.1. Groups and roles are the foundation. |
| udpcast multicast distribution | v1.1 (ADR-0003). HTTP range is sufficient for 50-node v1.0. |
| OIDC authentication | v1.1 (ADR-0001). API key scopes cover v1.0. |
| Gilfoyle UX polish findings | Gilfoyle's Senior HPC Admin walkthrough findings feed Sprint 3 / v1.1 UX backlog, not this sprint. |

---

## 4. Definition of Done Per Item

| ID | Done when |
|----|-----------|
| F1 | Rebuild button visible on Images page; progress events stream to UI in real time; history of last N rebuilds stored and rendered; rebuild triggered via admin-scoped API key |
| F2 | A build in `failed` or `interrupted` state has a Resume button; clicking it re-enters the build pipeline from the last safe checkpoint; no duplicate build record created |
| F3 | Server restart with in-flight builds drains gracefully (waits for safe checkpoint or timeout); on restart, builds resume from last checkpoint; no build is tombstoned as permanent failure without at least one resume attempt |
| S2-01 | `GET/POST/DELETE /api/v1/groups` and `POST /api/v1/groups/:id/deploy` all return correct responses; group membership is transactionally consistent; group-targeted deploy enqueues all member nodes |
| S2-02 | `POST /api/v1/nodes/bulk` with valid 50-row CSV inserts all 50 nodes atomically; a row with a duplicate MAC returns 409 and rolls back the entire import; all imported nodes reachable via `GET /api/v1/nodes?mac=` |
| S2-D1 | NodeConfig accepts per-disk `role` field; recommender skips disks with `role: skip`, `role: data`, `role: scratch`; deploy aborts with a structured error if no disk has `role: os` after hint resolution; existing NodeConfigs without the field default to current behavior |
| S2-04 | A deployed Rocky 9 node has correct `/etc/NetworkManager/system-connections/` keyfile matching NodeConfig.Interfaces; hostname is set; /etc/hosts has all cluster nodes (from group membership); verified by SSH inspection in Proxmox lab |
| S2-08 | SLURM drain fires before IPMI PXE signal; RESUME fires on success; drain is skipped and logged if slurmctld unreachable; PBS backend passes unit tests; ClusterIntegration config absent = no-op with no error |
| S2-10 | `POST /api/v1/groups/:id/deploy-job` creates a deploy job; server respects concurrency limit; each node transitions through queued → deploying → success/failed; UI progress view updates in real time; tested at concurrency 5, 10, 20 against 50-node group in lab |
| S2-11 | `GET /metrics` returns Prometheus text format; node count gauge, deploy duration histogram, throughput gauge all present; Gilfoyle's lab Prometheus scrapes it successfully; throughput gauge reflects >=200 MB/s aggregate at 20 concurrent clients |
| S2-12 | CI job runs on every push to main after Sprint 1 merge; boots test-node-01 via PXE; deploys image A, verifies SSH and hostname; redeploys image B, verifies SSH and hostname; job fails and blocks release if any step fails; v1.0 tag cannot push without this green |

---

## 5. Sprint Exit Criteria

The sprint is done when all of the following are true:

- [ ] F1/F2/F3: Image build pipeline UX complete; no build can be permanently orphaned by a server restart
- [ ] Bulk CSV import: 50 nodes imported in one transaction, all reachable via MAC lookup
- [ ] Node groups: group CRUD and group-targeted deploy endpoints pass integration tests
- [ ] DiskRole annotation: deploy aborts cleanly on nodes with no `os`-role disk; existing single-disk nodes unaffected
- [ ] First-boot network: deployed node has correct NM keyfile, hostname, and /etc/hosts (SSH-verified in lab)
- [ ] Scheduler hook: SLURM drain fires before reimage, RESUME fires after; no-op path tested with slurmctld absent
- [ ] Rolling deploy job: 50-node group deployed at concurrency=10; deploy_job_nodes shows 100% completion; UI reflects live status
- [ ] Prometheus endpoint: `GET /metrics` returns correct gauges; scrape succeeds in lab
- [ ] Integration test suite: CI gate green on Proxmox lab; v1.0 release tag blocked until green
- [ ] All v1.0 MVP checklist items in ROADMAP.md marked green

---

## 6. Top Risks

**Risk 1 — Rolling deploy job (S2-10) state machine complexity.** The node queueing, IPMI signaling, per-node status tracking, and UI event stream form a non-trivial state machine. If this runs long, the fallback is: cut S2-10 from the sprint commitment, ship v1.0 with single-node deploy only, and document rolling deploys as a manual loop. The 50-node checklist gate is still achievable with scripted single-node deploys. Richard reviews the state machine design before implementation begins.

**Risk 2 — First-boot network config silent failures (S2-04).** A misconfigured NetworkManager keyfile or bonded NIC that does not come up is invisible until boot on real hardware. The Proxmox integration test (S2-12) is the mitigation — but Proxmox uses virtio NICs, not real bonded hardware. The v1.0 checklist accepts Proxmox-verified as sufficient; bare-metal bonding validation moves to v1.1 pre-release lab work.

**Risk 3 — Dinesh bandwidth.** 10 code items across 2 weeks is aggressive. Triage order if scope must cut: S2-D1 (DiskRole) is highest-safety-value-per-effort and should not cut. S2-08 (scheduler hook) and S2-10 (rolling deploy) are the candidates to slip to v1.1 if time runs out — the turn-key path still works without them for the lab demo; the v1.0 MVP checklist has explicit scheduler and rolling deploy requirements, so slipping them slips the checklist not the sprint.
