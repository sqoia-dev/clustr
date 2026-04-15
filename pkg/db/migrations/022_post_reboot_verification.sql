-- ADR-0008: Post-Reboot Verification — Two-Phase Deploy Success
--
-- Extends node_configs with timestamps for the two-phase deploy lifecycle:
--
--   deploy_completed_preboot_at  — replaces last_deploy_succeeded_at semantically.
--     Set when clonr-static POSTs deploy-complete from inside the PXE initramfs.
--     Proves the rootfs was written; does NOT prove the OS boots.
--
--   deploy_verified_booted_at   — new. Set when the deployed OS phones home via
--     POST /api/v1/nodes/{id}/verify-boot after first boot. Proves the bootloader,
--     kernel, and systemd all started successfully.
--
--   deploy_verify_timeout_at    — set by the server scanner when verify-boot is
--     not received within CLONR_VERIFY_TIMEOUT after deploy_completed_preboot_at.
--
--   last_seen_at                — updated on every verify-boot call (heartbeat).
--
-- Back-compat: last_deploy_succeeded_at is retained (dual-write) for one release.
-- It will be removed in v1.0 once all callers migrate to deploy_completed_preboot_at.

ALTER TABLE node_configs ADD COLUMN deploy_completed_preboot_at INTEGER;
ALTER TABLE node_configs ADD COLUMN deploy_verified_booted_at   INTEGER;
ALTER TABLE node_configs ADD COLUMN deploy_verify_timeout_at    INTEGER;
ALTER TABLE node_configs ADD COLUMN last_seen_at                INTEGER;

-- Backfill: carry forward existing deploy success timestamps into the new column.
-- This means nodes that were "deployed" before this migration retain their state.
UPDATE node_configs
   SET deploy_completed_preboot_at = last_deploy_succeeded_at
 WHERE last_deploy_succeeded_at IS NOT NULL;
