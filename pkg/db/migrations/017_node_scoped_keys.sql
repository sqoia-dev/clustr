-- 017_node_scoped_keys: add node_id binding and TTL to api_keys.
--
-- node_id: when set, this key is scoped to a specific node. The deploy agent
--   running in initramfs receives exactly this key and may only access resources
--   associated with its own node.
--
-- expires_at: unix timestamp after which the key is no longer valid. NULL means
--   no expiry (used for long-lived admin keys). Node-scoped keys are given a
--   short TTL (1h) at mint time and are revoked when a new one is issued for
--   the same node.

ALTER TABLE api_keys ADD COLUMN node_id TEXT;
ALTER TABLE api_keys ADD COLUMN expires_at INTEGER;

CREATE INDEX IF NOT EXISTS idx_api_keys_node_id ON api_keys(node_id);
