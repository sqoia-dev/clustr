# clonr Sprint 3 Plan — v1.0 "Ready" Path

**Last updated:** 2026-04-13
**Sprint duration:** 2 weeks
**Sprint 2 exit SHA (tag v0.1.2):** aa06d86
**Owner legend:** D = Dinesh (code), G = Gilfoyle (infra/ops/lab), R = Richard (architecture review)

---

## 1. Sprint Goal

Ship v1.0 Ready: the content-only image refactor lands end-to-end (ADR-0009 + ADR-0009 amendments), UEFI deploy works reliably on all four lab VMs, user accounts and post-reboot phone-home are implemented (ADR-0007 and ADR-0008 leave draft status), and the HPC admin customization surface — package manifest, config overlay, kernel pinning, DKMS pipeline, service masking, post-install scripts, and secrets delivery — is operational. The sprint exits when all four lab VMs (VM201, VM202, VM206, VM207) boot to login on a fresh reimage under the new architecture, verified via serial console per the ADR-0006 validation matrix.

---

## 2. Completed in Sprint 2

| SHA(s) | Item |
|--------|------|
| caa4001, 248da57, 5a567cf, 6d12c85 | F1 Initramfs rebuild button: UI, progress stream, history |
| 248da57, 5a567cf | F2 Resume button for failed/interrupted builds |
| 5a567cf | F3 Reconcile-on-restart: graceful drain instead of tombstone |
| 5fa7db4, bc89254, 1d19dfb, ad61a419 | S2-01 Node groups + group-targeted deploy |
| 86a0746, 5607676, 3f8060c, ebb84b0 | Exit codes + reimage terminal state |
| f9e3518, 6021877, ca29c41, c9a168e, 48f9ec9, 90ca5ae | API key rotation + Settings page + session expiry banner |
| aa06d86 | BIOS deploy path fully verified (v0.1.2 tag) |
| 82132a8 | ADR-0009 Content-only images |
| 034b7d9 | Admin image customization requirements (Gilfoyle) |

---

## 3. Carrying Over from Sprint 2

Items that were not done in Sprint 2 and survive the content-only pivot. Each entry notes how the pivot changes or simplifies the item.

**S2-02 — Bulk CSV node import. KEEP.**
No architectural impact. Node records are topology-agnostic; the image-format pivot does not change how nodes are imported. Still S (small) effort. Ships early in Sprint 3 to unblock the 50-node lab validation.

**S2-D1 — DiskRole annotation. KEEP, SIMPLIFIED.**
The content-only model makes disk role even more important: if the deploy engine partition-wipes a data volume, there is no content to recover from. The annotation remains S effort and ships before any lab hardware validation.

**S2-04 — First-boot network config. KEEP, RESHAPED.**
Previously this was NetworkManager keyfiles written in the Finalize phase. Under the content-only model, per-node IP/hostname injection is still a Finalize-phase operation but now cleanly separated from the image overlay (which carries NM/networkd templates for bond config, MTU, VLAN) and from `node_config` (which carries per-node IP and hostname). The effort is M and the implementation is now cleaner — Finalize writes exactly the per-node fields into the extracted rootfs, not a full NM config from scratch.

**S2-08 — Scheduler join hook (SLURM drain/resume + PBS backend). KEEP.**
No change from the pivot. Scheduler integration fires before and after the deploy, independent of image format. Still M effort. Dependency on S2-04 (node identity correct before SLURM registration) remains.

**S2-10 — Rolling deploy job. KEEP, RESHAPED.**
The node groups orchestrator (S2-01) partially covers the queuing model. Under content-only, each node's deploy is a well-defined transaction (partition → extract → generate → inject → bootloader → reboot), which makes the per-node state machine easier to reason about. Still L effort but the state machine surface is reduced because there is no patching phase that can produce partial-success states. Concurrency limit defaults to 10% of group size, min 1.

**S2-11 — Prometheus metrics endpoint. KEEP.**
No architectural impact. Still S effort. Blocked on S2-10 (deploy job metrics) as before.

**S2-12 — Proxmox CI integration gate. KEEP, RESHAPED.**
The CI gate must now validate the content-only deploy path: boot VM, deploy rocky10-base under the new architecture, verify SSH + hostname + kernel version + required_secrets delivered, redeploy image B, verify again. The test matrix expands to cover BIOS (VM206/VM207) and UEFI (VM201/VM202). Still L effort (Gilfoyle owns the harness).

**ADR-0007 — User accounts first-run bootstrap. KEEP, NOW IMPLEMENTING.**
Was committed as ADR, never implemented. Sprint 3 implements it: `first_run` DB flag, `POST /api/v1/bootstrap` endpoint for initial admin account creation, UI first-run flow. Small scope; the ADR is the spec.

**ADR-0008 — Post-reboot phone-home verification. KEEP, NOW IMPLEMENTING.**
Was committed as ADR, never implemented. The `clonr-verify-boot.service` oneshot unit and the `/api/v1/nodes/:id/verify-boot` endpoint are straightforward. The node token written at finalize time (§3g) is already architectured; this completes the consumer side. M effort.

---

## 4. Obsolete from Sprint 2

None of the Sprint 2 carryovers are made obsolete by the content-only pivot. The pivot replaces the internal deploy mechanics without eliminating any user-facing feature requirement. The only item that becomes structurally different (not obsolete) is S2-04, reshaped above.

---

## 5. New Sprint 3 Scope

Items are dependency-ordered. Items marked (blocker) must complete before items that depend on them can start.

| ID | Title | Owner | Effort | Dependency | Notes |
|----|-------|-------|--------|------------|-------|
| **N3-01** | Content-only image refactor: rewrite `pkg/images/builder.go` with tarball exclude validation; reject any tarball containing `/etc/fstab` content | D | M | — | (blocker for N3-02, N3-03) |
| **N3-02** | Finalize rewrite: fstab ownership split (§5A), BLS entry generation, dracut chroot, firmware-branched bootloader install; remove all UUID-patch and sed code | D | L | N3-01 | The core of the refactor. Must work BIOS and UEFI before anything else ships. |
| **N3-03** | image.json sidecar: schema definition, DB storage, distribution layer integration (ADR-0003 model), UI display of kernel_version, firmware_hint, content_sha256 | D | M | N3-01 | |
| **N3-04** | Shell session overlayfs + commit-on-exit: overlayfs lowerdir/upperdir per session, commit → new image version with recomputed content_sha256, discard → upperdir deleted; fix latent bug: close handler must invalidate tar-sha256 sidecar | D | M | N3-01 | The rocky101 desync bug. Commit path creates a new version record; original version is immutable. |
| **N3-05** | Image customization surface: package manifest, config file overlay, user/UID injection, kernel cmdline extras, service enable/disable/mask, post-install scripts — all applied at image-build time inside nspawn chroot | D | L | N3-01 | Sprint 3 bakes these into the build pipeline. Deploy-time application of overlays (for per-node config) is a separate step in N3-02. |
| **N3-06** | DKMS build pipeline: nspawn chroot with /proc /sys /dev bound, kernel pinning via dnf versionlock, DKMS autoinstall, post-build .ko verification gate against dkms_modules manifest | G + D | M | N3-05 | Gilfoyle owns the nspawn environment and dkms_modules manifest definition; Dinesh owns build pipeline integration. |
| **N3-07** | Secrets storage + delivery: envelope encryption in server (master key file, per-secret DEK, AES-256-GCM), required_secrets API endpoint, clonr-static finalize injection with mode+owner; munge.key and root authorized_keys minimum for v1.0 | D | M | N3-02, N3-03 | Follows ADR-0002 pattern. Master key at /etc/clonr/secret-master.key, mode 0400. |
| **N3-08** | UEFI deploy path rework: clean grub2-install --target=x86_64-efi + fresh efibootmgr at finalize under content-only model; validate on VM201 and VM202 to login via serial console | G | M | N3-02 | VM201/VM202 currently don't reach login. This is the explicit UEFI gate. |
| **N3-09** | ADR-0007 implementation: first_run bootstrap endpoint, initial admin account creation flow, UI first-run page | D | S | — | |
| **N3-10** | ADR-0008 implementation: clonr-verify-boot.service oneshot unit written at finalize, verify-boot API endpoint, UI node status update on phone-home | D | M | N3-02, N3-07 | Node token (written at finalize) is the auth credential for the phone-home call. |
| **N3-11** | Rocky10-base image rebuild under new architecture: ISO builder run with new exclude list, image.json generated, BIOS validated on VM206/VM207, UEFI validated on VM201/VM202 | G | M | N3-02, N3-05, N3-08 | Blocks sprint exit criteria. Cannot exit without four green VMs. |
| **N3-12** | Sanjay polish items: global deployments page, CLI write-parity commands (node add/set-image/reimage), node list filter/search bar, sortable image table, session expiry warning banner (verify 90ca5ae covers this or complete it) | D | M | N3-03 | Rank 3-4 SME walkthrough items from Sprint 2 session. |
| **S2-02** | Bulk CSV node import | D | S | — | Carried from Sprint 2. |
| **S2-D1** | DiskRole annotation | D | S | — | Carried from Sprint 2. |
| **S2-04** | First-boot network config (reshaped) | D | M | N3-02 | Per-node IP/hostname injection in Finalize; bond/MTU templates from image overlay. |
| **S2-08** | Scheduler join hook | D | M | S2-04 | |
| **S2-10** | Rolling deploy job | D | L | S2-01 (done), S2-08 | |
| **S2-11** | Prometheus metrics endpoint | D + G | S | S2-10 | |
| **S2-12** | Proxmox CI integration gate | G | L | N3-11 | Must cover BIOS + UEFI matrix. |

---

## 6. Definition of Done Per Item

| ID | Done when |
|----|-----------|
| N3-01 | `pkg/images/builder.go` rejects any tarball containing a non-empty `/etc/fstab`; unit tests cover accept and reject paths |
| N3-02 | A fresh deploy on VM206 (BIOS, RAID) produces a booted node with correct fstab (OS-disk only), correct BLS entry, correct kernel cmdline; no UUID-patch or sed code remains in finalize.go; overlay fstab entries survive untouched |
| N3-03 | Every image record has a valid image.json sidecar in the DB; images list UI shows kernel_version, firmware_hint, content_sha256 (first 12 chars); image detail page renders all customization tabs |
| N3-04 | Opening a shell on an image mounts overlayfs; commit creates a new version record with recomputed content_sha256; discard leaves original unchanged; close handler invalidates sidecar; the rocky101 desync scenario is a unit test that passes |
| N3-05 | An image built with a declarative package list, config overlay, user injection, service mask list, and post-install script produces the expected rootfs; all operations run at build time inside the nspawn chroot |
| N3-06 | A GPU image definition with nvidia-dkms in the package list produces a sealed tarball containing `/lib/modules/<kver>/extra/nvidia.ko`; a build where the .ko is absent fails the verification gate and the image is set to build_failed state |
| N3-07 | `clonr secret set munge.key --cluster <id> --file ...` stores encrypted; deploy of a node with an image declaring munge.key in required_secrets writes /etc/munge/munge.key (mode 0400, owner munge) in the target rootfs; a missing secret causes a hard finalize failure |
| N3-08 | VM201 and VM202 boot to login prompt on fresh reimage; serial console shows grub menu, kernel boot, systemd reaching multi-user.target; efibootmgr shows clonr entry |
| N3-09 | Fresh server with empty DB redirects to /bootstrap; admin account created; subsequent restart does not re-show bootstrap page |
| N3-10 | clonr-verify-boot.service is present in deployed rootfs; on VM reboot, the service calls the server and the node transitions to verified state in the UI |
| N3-11 | rocky10-base image exists in DB under content-only schema; all four VMs (VM201, VM202, VM206, VM207) boot to login from this image; serial console gate passes per ADR-0006 matrix |
| N3-12 | Global deployments page lists all in-progress and recent deploys across all nodes; CLI can add a node, set its image, and trigger reimage; node list supports filter by status; image table is sortable by created_at and name |
| S2-02 | 50-row CSV import is atomic; duplicate MAC returns 409 and rolls back |
| S2-D1 | Deploy aborts with structured error if no disk has role:os; existing single-disk nodes unaffected |
| S2-04 | Deployed node has correct NM keyfile, hostname, /etc/hosts; verified by SSH inspection |
| S2-08 | SLURM drain fires before IPMI PXE signal; RESUME fires on success; no-op if slurmctld absent |
| S2-10 | 50-node group deployed at concurrency=10; UI reflects live per-node status |
| S2-11 | GET /metrics returns correct gauges; Gilfoyle's lab Prometheus scrapes successfully |
| S2-12 | CI job boots test-node-01 via PXE on BIOS and UEFI paths; deploys image A, verifies SSH + hostname; redeploys image B, verifies; blocks release tag on failure |

---

## 7. Sprint Exit Criteria

The sprint is done when all of the following are true:

- [ ] Content-only refactor: all four lab VMs boot to login on fresh reimage under the new architecture
- [ ] UEFI path: VM201 and VM202 reach login via serial console; efibootmgr entry confirmed
- [ ] BIOS path: VM206 and VM207 reach login via serial console; grub menu confirmed
- [ ] Fstab ownership: image-provided fstab.d entries survive a deploy cycle untouched
- [ ] DKMS gate: a test image with a declarative dkms_modules entry passes verification or fails build_failed (not silently missing .ko files on deploy)
- [ ] Secrets delivery: munge.key and root authorized_keys delivered at finalize; not present in image tarball
- [ ] ADR-0007: first-run bootstrap flow works on a fresh server
- [ ] ADR-0008: clonr-verify-boot.service fires on all four VMs after reimage and posts phone-home
- [ ] CI gate (S2-12): green on last SHA, covering BIOS + UEFI matrix
- [ ] No Sprint 2 carryover items older than one sprint remain open (S2-10 and S2-12 are the stretch items; if they slip, document explicitly with a new target sprint)

---

## 8. Top 3 Risks

**Risk 1 — UEFI path fragility (N3-08).** The current UEFI failure (VM201/VM202 don't reach login) is undiagnosed at the architectural level — it may be a grub2-install target issue, an efibootmgr boot order issue, or a firmware-specific ESP layout issue. Under the content-only model the path is simpler (no pre-installed EFI binaries in the tarball, fresh grub2-install + efibootmgr at finalize), but "simpler" does not mean "debugged." If UEFI remains broken after N3-02 lands, Gilfoyle drops to UEFI debugging full-time before N3-11 blocks the sprint exit. Mitigation: Gilfoyle starts UEFI investigation in parallel with N3-02 implementation, not after.

**Risk 2 — Sprint 3 scope is wider than Sprint 2 was.** Sprint 2 had ~10 code items across 2 weeks and ran long. Sprint 3 carries all of Sprint 2's unfinished items plus the entire content-only refactor (the largest single item in the project's history). The triage order if scope must cut: N3-01 through N3-08 and N3-11 are non-negotiable for the sprint exit criteria. N3-09 (ADR-0007 bootstrap), N3-10 (ADR-0008 phone-home), N3-12 (Sanjay polish), S2-08, S2-10, S2-11 are the candidates to slip to Sprint 4 if the refactor runs long. The sprint exit criteria can still be met without those items.

**Risk 3 — DKMS build environment portability (N3-06).** DKMS builds inside a systemd-nspawn chroot are sensitive to the host kernel version (the nspawn host must be running a kernel compatible with the kernel-devel headers being used for the DKMS build) and to the availability of gcc and make inside the chroot. On a Rocky 10 build host building a Rocky 9 image, the nspawn environment may have kernel ABI mismatches that cause DKMS to produce silently incorrect .ko files. Mitigation: Gilfoyle validates the nspawn environment against at least one NVIDIA DKMS build before N3-06 is marked done; if the host/guest kernel compatibility is a blocker, the DKMS step is isolated to a QEMU VM matching the target distro.
