-- migration 099: SELF-MON hosts table + nodes.host_id FK (#243)
--
-- Introduces a `hosts` table that tracks physical/virtual hosts known to
-- clustr-serverd.  Two roles are recognised:
--
--   control_plane — the host running clustr-serverd itself. Exactly one row
--                   per installation; inserted at startup via BootstrapControlPlaneHost().
--                   Does NOT appear in the datacenter node enumeration.
--
--   cluster_node  — a host that also has a row in node_configs. The cluster
--                   node row provides a stable identifier cross-referenced by
--                   the host row so metrics and alerts can distinguish "is this
--                   my own host?" from "is this a managed cluster node?".
--
-- The partial-unique semantics (at most one control_plane row) are enforced at
-- the application layer in db.BootstrapControlPlaneHost().  SQLite partial
-- unique index on expression is not reliably portable across all versions we
-- target (EL8 ships SQLite 3.26), so we use a simple UNIQUE on (hostname, role)
-- plus the application guard.
--
-- nodes.host_id is nullable for now.  Existing node rows are not back-filled
-- automatically; the link is established by the client-registration path when a
-- cluster_node host row is created.

CREATE TABLE IF NOT EXISTS hosts (
    id          TEXT PRIMARY KEY,               -- UUID v4
    hostname    TEXT NOT NULL,                  -- canonical hostname (os.Hostname() for cp)
    role        TEXT NOT NULL
                    CHECK (role IN ('control_plane', 'cluster_node')),
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- At most one (hostname, role) pair — prevents accidental duplicate inserts.
CREATE UNIQUE INDEX IF NOT EXISTS hosts_hostname_role_uniq ON hosts (hostname, role);

-- nodes.host_id FK — nullable; populated by the client-registration path.
ALTER TABLE node_configs ADD COLUMN host_id TEXT REFERENCES hosts(id);

CREATE INDEX IF NOT EXISTS nodes_host_id ON node_configs (host_id);
