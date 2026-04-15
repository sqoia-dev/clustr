# clonr Docs Index

**Generated:** 2026-04-13 — applies to clonr main @ 5ce769c

This is the single-page map of every document in `docs/`. Open this first. Each entry tells you whether to trust the file for current decisions.

**Trust hierarchy:** CURRENT > IN PROGRESS (check back) > DRAFT > STALE/SUPERSEDED (do not use for decisions)

---

## Section 1: Architecture Decisions (ADRs)

All ADRs live in `docs/adr/`. An ADR is Accepted unless noted otherwise. Accepted means the decision is implemented or actively being implemented — it is not a proposal.

| # | Title | Status | Amends | One-Line Summary | Last Commit SHA |
|---|-------|--------|--------|-----------------|-----------------|
| [0001](adr/0001-auth-model.md) | Authentication and Authorization Model | CURRENT / Accepted | — | Two-scope API keys (admin + node); OIDC optional overlay in v1.1 | fbccab9 |
| [0002](adr/0002-secrets-storage.md) | Secrets Storage | CURRENT / Accepted | — | AES-256-GCM envelope encryption with file-resident master key; no CGO | fbccab9 |
| [0003](adr/0003-image-distribution.md) | Image Distribution at Scale | CURRENT / Accepted | Amended by 0009 | HTTP range requests in v1.0; udpcast multicast in v1.1; BitTorrent deferred | fbccab9 |
| [0004](adr/0004-persistence-strategy.md) | Persistence Strategy | CURRENT / Accepted | Amended by 0008, 0009 | SQLite (pure-Go, WAL) is permanent default; PostgreSQL optional backend in v1.1 | fbccab9 |
| [0005](adr/0005-turnkey-cluster-charter.md) | Turn-Key Cluster Charter | CURRENT / Accepted | — | Hard scope boundary: clonr owns provisioning-time only; day-2 ops are out of scope | fbccab9 |
| [0006](adr/0006-browser-session-layer.md) | Browser Session Layer | CURRENT / Accepted | 0001 (additive) | Stateless HMAC-signed session cookie for web UI; localStorage admin-key flow is temp | f34323e |
| [0007](adr/0007-user-accounts.md) | User Accounts and First-Run Bootstrap | CURRENT / Accepted | 0001, 0006 (additive) | Users table + role model + predictable first-run default credential; user-scoped keys | 722c8f3 |
| [0008](adr/0008-post-reboot-verification.md) | Post-Reboot Verification | CURRENT / Accepted | 0001, 0004 | Two-phase deploy success: pre-reboot callback + post-reboot heartbeat required | 6038544 |
| [0009](adr/0009-content-only-images.md) | Content-Only Images | IN PROGRESS (Gilfoyle editing) | 0003, 0004 | Images are rootfs tarballs only; all bootloader/fstab artifacts generated fresh at deploy | 9f72232 |

### Known Contradictions Between ADRs

These are surfaced for the founder's awareness. Resolution requires a new ADR or amendment — do not resolve in docs.

1. **ADR-0003 vs ADR-0009 — image format:** ADR-0003 defines the blob distribution model assuming a single opaque blob (filesystem or block capture). ADR-0009 changes the blob to a two-file bundle (`rootfs.tar.zst` + `image.json`). ADR-0003's v1.0 HTTP range request path remains valid for transport, but the "what is being served" has changed. ADR-0003 has not been formally re-stated to reflect the new blob structure. Richard should amend ADR-0003 to acknowledge the format change.

2. **docs/architecture.md vs ADR-0009 — BaseImage struct:** `docs/architecture.md` defines `BaseImage.Format: filesystem|block` and embeds `DiskLayout` (including `Bootloader.Type: grub2|systemd-boot`) inside `BaseImage`. ADR-0009 eliminates format variants and moves all bootloader logic to deploy time. The architecture doc is marked STALE but the contradiction will confuse anyone who opens it. The authoritative struct is in `pkg/api/types.go`.

3. **admin-image-customization-requirements.md header vs ADR-0009 header:** The requirements doc still carried "Draft — for Richard's review" until this cleanup pass. ADR-0009 explicitly states the doc was incorporated (commit `034b7d9`). The status mismatch is now resolved in the requirements doc header, but reviewers comparing the two files at earlier commits will see the discrepancy in git history.

---

## Section 2: Sprint Plans

Current sprint is Sprint 3. Sprint 1 and Sprint 2 plans are preserved for exit criteria review but are SUPERSEDED for planning purposes.

| Sprint | File | Status | Goal | Exit SHA |
|--------|------|--------|------|----------|
| Sprint 1 | [sprint-plan.md](sprint-plan.md) | SUPERSEDED by Sprint 2 | Stop the bleeding: correctness + security hardening | v0.1.0 @ e8c86fa |
| Sprint 2 | [sprint-2-plan.md](sprint-2-plan.md) | SUPERSEDED by Sprint 3 | Turn-key cluster: groups, CSV import, first-boot net config, rolling deploy | a15d762 |
| Sprint 3 | [sprint-3-plan.md](sprint-3-plan.md) | IN PROGRESS (Richard editing) | v1.0 content-only image refactor post-ADR-0009 | 5ce769c (unfinished) |

Sprint 2 exit criteria: not formally documented in the plan file. The ADR-0008 and ADR-0009 creation during Sprint 2 is the de facto record of what changed scope. Sprint 2 did not ship a clean v1.0 — the content-only pivot is Sprint 3's core work.

---

## Section 3: Operational Runbooks

All ops runbooks live in `docs/ops/`. These are procedure docs for the operator managing the clonr provisioning server itself (not the HPC cluster).

| File | Status | One-Line Summary | Last Commit SHA |
|------|--------|-----------------|-----------------|
| [ops/backup-restore.md](ops/backup-restore.md) | CURRENT | SQLite hot backup via systemd timer; restore playbook with WAL safety | cf7e367 |
| [ops/deploy-server-update.md](ops/deploy-server-update.md) | CURRENT | SSH hardening drop-in + binary update procedure + auth key bootstrap | 92644c9 |
| [ops/disaster-recovery.md](ops/disaster-recovery.md) | CURRENT | Kill-and-recover smoke test results; recovery from process death and power loss | 4d1f66f |
| ops/secrets-master-key.md | IN PROGRESS (Gilfoyle) | Master key ops: rotation, backup, container mount pattern | not yet committed |
| ops/boot-matrix-coverage.md | IN PROGRESS (Gilfoyle) | Boot path coverage matrix: BIOS/UEFI x SATA/NVMe/RAID | not yet committed |

---

## Section 4: Requirements and Design

| File | Status | One-Line Summary | Last Commit SHA |
|------|--------|-----------------|-----------------|
| [admin-image-customization-requirements.md](admin-image-customization-requirements.md) | CURRENT | Enumerates every HPC admin customization class (packages, DKMS, fstab, secrets) backing ADR-0009 | 034b7d9 |
| [validation-strategy.md](validation-strategy.md) | CURRENT | Serial console gate as mandatory pass criterion; boot matrix; ADR-0008 alignment | e439b0a |
| [ui-design-personas.md](ui-design-personas.md) | CURRENT | UX journey maps for Junior sysadmin, Senior HPC admin, and Research engineer personas | 9b56b4f |
| [test-lab-design.md](test-lab-design.md) | CURRENT | Complete Proxmox lab spec: bridge config, VM layout, PXE setup, copy-paste runnable commands | 21f82f1 |
| [exit-codes.md](exit-codes.md) | CURRENT | Classified exit codes for `clonr deploy --auto`; codes appear in DB and node detail UI | ebb84b0 |
| [ROADMAP.md](ROADMAP.md) | CURRENT | Phase-by-phase product vision (v0.x through v2.x); persona definitions; locked decisions | a189a07 |
| [architecture.md](architecture.md) | STALE | Pre-ADR-0009 design doc; struct definitions and package layout do not match current code | 21f82f1 |

---

## Section 5: Historical / Superseded

These files are kept for reference and git archaeology. Do not use them for current planning, feature decisions, or operator procedures. They reflect a prior state of the project.

| File | Superseded By | Reason |
|------|--------------|--------|
| [sprint-plan.md](sprint-plan.md) | sprint-2-plan.md, sprint-3-plan.md | Sprint 1 scope only; project has advanced two sprints |
| [sprint-2-plan.md](sprint-2-plan.md) | sprint-3-plan.md | Sprint 2 complete; content-only pivot changed the roadmap mid-sprint |
| [architecture.md](architecture.md) | ADR-0009, pkg/api/types.go | Written before content-only pivot; BaseImage struct, ImageFormat, DiskLayout, and deploy pipeline described here are all superseded by ADR-0009 |

---

## Quick Reference: The 8 Files to Trust Right Now

If you only open eight files, open these:

1. `docs/adr/0009-content-only-images.md` — the architectural pivot that defines Sprint 3
2. `docs/sprint-3-plan.md` — active sprint scope (IN PROGRESS, Richard)
3. `docs/ROADMAP.md` — what we're building toward and why
4. `docs/adr/0001-auth-model.md` — auth model (scopes, key types, OIDC plan)
5. `docs/validation-strategy.md` — what "done" means for any deploy path
6. `docs/admin-image-customization-requirements.md` — what HPC admins actually need images to support
7. `docs/adr/0008-post-reboot-verification.md` — why we require serial console confirmation
8. `docs/ops/backup-restore.md` — keeping the provisioning server recoverable
