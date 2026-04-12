-- Tracks whether a node is awaiting a reimage deployment.
-- When 1, the register endpoint always returns action=deploy regardless of
-- whether the base_image_id changed, forcing the deploy client to proceed.
-- Cleared to 0 by the server once the deploy finalizes successfully.
ALTER TABLE node_configs ADD COLUMN reimage_pending INTEGER NOT NULL DEFAULT 0;
