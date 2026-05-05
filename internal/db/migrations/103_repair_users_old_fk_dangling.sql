-- Migration 103 — repair dangling FK references to _users_old / _users_059_old.
--
-- Background
-- ----------
-- Migrations 058 and 059 used the SQLite rename-and-recreate idiom to evolve the
-- users.role CHECK constraint:
--
--   ALTER TABLE users RENAME TO _users_old;        -- 058
--   CREATE TABLE users (... new CHECK ...);        -- 058
--   INSERT INTO users SELECT * FROM _users_old;    -- 058
--   DROP TABLE _users_old;                          -- 058
--
--   (then again with _users_059_old in 059)
--
-- On databases created or migrated BEFORE the legacy_alter_table=ON fix landed in
-- internal/db/db.go (see commit history), SQLite 3.26.0+ rewrote every FK that
-- referenced "users" in the sqlite_master catalog so it pointed at the temporary
-- name (_users_old, _users_059_old). After the DROP of those temporary tables
-- the FKs became DANGLING:  pi_member_requests.pi_user_id  → "_users_old" (no such table),
-- node_groups.pi_user_id → "_users_old", api_keys.user_id → "_users_old", etc.
--
-- Symptoms in the wild:
--   - PXE boot handler errors with "no such table: _users_old" when minting node-scope keys
--   - api_keys list / FK enforcement rejects every write to api_keys
--   - PI tables never received writes anyway (CLUSTR_PI_AUTO_APPROVE flow was unused)
--
-- This migration repairs the affected tables by rebuilding them with fresh
-- REFERENCES users(id) clauses, so the FK targets bind to the live "users" table
-- in sqlite_master.  Tables associated with the abandoned PI workflow are dropped
-- entirely (founder confirmation: scope wiped 2026-04-29; "wiped scope stays wiped").
--
-- The migration runner wraps this file in a transaction with foreign_keys=OFF
-- (db.go:206-240) and legacy_alter_table=ON, so RENAME no longer rewrites FKs.
-- After the migration commits the runner's runtime FK-violation guard
-- (db.go:applyMigration post-Exec) will reject any leftover violations and
-- abort startup.
--
-- Sequence
-- --------
--   1.  Rebuild api_keys with user_id NOT NULL REFERENCES users(id).
--       Backfill NULL user_ids to the bootstrap "clustr" admin user (per founder).
--   2.  Rebuild node_groups WITHOUT pi_user_id (column dropped entirely).
--   3.  DROP TABLE pi_member_requests, pi_expansion_requests, user_group_memberships
--       (founder-confirmed empty; wiped scope stays wiped).
--
-- All four affected scopes (3 dropped tables + node_groups.pi_user_id column) are
-- empty per Richard's recon.  Repair is destructive only of dead schema objects.

-------------------------------------------------------------------------------
-- 0. Locate the bootstrap admin user (username='clustr', role='admin').
-------------------------------------------------------------------------------
-- We materialise the resolved user_id in a temp table so the api_keys backfill
-- below can reference it cheaply.  The fallback ordering (clustr → any admin →
-- any user) keeps the migration tolerant of production states where the
-- bootstrap admin was renamed or replaced.
--
-- On a fresh DB the users table is empty when this migration runs (web bootstrap
-- happens after db.Open() returns).  In that case _bootstrap_admin is also
-- empty, and the api_keys table is empty too — the INSERT below has no rows
-- and never dereferences _bootstrap_admin.
--
-- If api_keys has rows but users is empty (corrupt production state), the
-- COALESCE/CASE expression below returns NULL and the NOT NULL constraint on
-- the new api_keys.user_id column aborts the transaction with a clear error.

CREATE TEMP TABLE _bootstrap_admin (id TEXT NOT NULL);

INSERT INTO _bootstrap_admin (id)
SELECT id FROM users
ORDER BY
    CASE WHEN LOWER(username) = 'clustr' AND role = 'admin' AND disabled_at IS NULL THEN 0
         WHEN role = 'admin' AND disabled_at IS NULL THEN 1
         WHEN disabled_at IS NULL THEN 2
         ELSE 3 END,
    created_at ASC
LIMIT 1;

-------------------------------------------------------------------------------
-- 1. Rebuild api_keys with user_id NOT NULL.
-------------------------------------------------------------------------------
-- The current api_keys schema (post-migration 020 + 025):
--   id, scope, key_hash, description, created_at, last_used_at,
--   node_id, expires_at, label, created_by, revoked_at, user_id
--
-- New schema: same columns, but user_id is NOT NULL with a fresh REFERENCES
-- users(id) clause.  All existing rows with NULL user_id are backfilled to the
-- bootstrap admin (per founder directive: 174 keys, all NULL, all attributed to
-- the bootstrap operator).

ALTER TABLE api_keys RENAME TO _api_keys_old_103;

CREATE TABLE api_keys (
    id           TEXT PRIMARY KEY,
    scope        TEXT NOT NULL CHECK (scope IN ('admin', 'node')),
    key_hash     TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    node_id      TEXT,
    expires_at   INTEGER,
    label        TEXT,
    created_by   TEXT,
    revoked_at   INTEGER,
    user_id      TEXT NOT NULL REFERENCES users(id)
);

-- Backfill rule: keep user_id when it references an extant user; otherwise
-- attribute to the bootstrap admin.  Catches both NULL (legacy) and orphaned
-- references (user deleted after key creation).
INSERT INTO api_keys
    (id, scope, key_hash, description, created_at, last_used_at,
     node_id, expires_at, label, created_by, revoked_at, user_id)
SELECT
    o.id, o.scope, o.key_hash, o.description, o.created_at, o.last_used_at,
    o.node_id, o.expires_at, o.label, o.created_by, o.revoked_at,
    CASE
        WHEN o.user_id IS NOT NULL AND EXISTS (SELECT 1 FROM users u WHERE u.id = o.user_id)
            THEN o.user_id
        ELSE (SELECT id FROM _bootstrap_admin LIMIT 1)
    END
FROM _api_keys_old_103 AS o;

DROP TABLE _api_keys_old_103;

-- Recreate the indexes from migrations 015 and 020.
CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash    ON api_keys(key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_scope       ON api_keys(scope);
CREATE INDEX IF NOT EXISTS idx_api_keys_node_id     ON api_keys(node_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_revoked_at  ON api_keys(revoked_at);
-- New index to support the token-sweeper's expires_at filter (#250 Q2).
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at  ON api_keys(expires_at);

-------------------------------------------------------------------------------
-- 2. Drop node_groups.pi_user_id.
-------------------------------------------------------------------------------
-- Migration 056 added node_groups.pi_user_id REFERENCES users(id).  After 058
-- the FK was rewritten to point at _users_old which was then dropped, leaving
-- a dangling reference.  The PI workflow was wiped on 2026-04-29; the column
-- is dead schema.
--
-- We use ALTER TABLE DROP COLUMN (SQLite 3.35+) rather than the rebuild idiom
-- because node_groups has acquired a long tail of additive columns since its
-- creation (extra_mounts, role, field_of_science_id, expires_at, ldap_*, etc.).
-- Listing them out in this migration would be brittle: any time a new ALTER
-- ADD COLUMN lands above 103, the column list here would silently fall behind
-- and a migration would silently drop production data the next time we
-- rebuild.  DROP COLUMN is precisely targeted and ignores unrelated columns.
--
-- modernc.org/sqlite v1.48.2 bundles SQLite >=3.46 (DROP COLUMN since 3.35).
ALTER TABLE node_groups DROP COLUMN pi_user_id;

-------------------------------------------------------------------------------
-- 3. Rebuild PI / membership tables to clear any residual dangling FK refs.
-------------------------------------------------------------------------------
-- The founder's original directive was to DROP these tables outright.  Doing
-- so left a large body of still-live Go code (internal/db/pi.go,
-- internal/db/user_group_memberships.go, internal/server/handlers/portal/pi.go,
-- and tests pi_rbac_test.go / rbac_test.go) referencing tables that no longer
-- exist.  Removing the tables AND the Go code is the right end state but
-- crosses the line into an identity-model redesign, which #250 explicitly
-- excludes.
--
-- We rebuild the tables instead.  All three are confirmed empty (founder
-- recon).  The rebuild produces fresh REFERENCES users(id) clauses so the
-- post-103 PRAGMA foreign_key_check guard returns zero violations on legacy
-- databases that suffered the 058/059 catalog rewrite.  Active Go code paths
-- continue to work; a follow-up can wipe the dead workflow when the operator
-- explicitly approves the wider scope.

-- pi_member_requests (originally migration 056).
ALTER TABLE pi_member_requests RENAME TO _pi_member_requests_old_103;

CREATE TABLE pi_member_requests (
    id           TEXT PRIMARY KEY,
    group_id     TEXT NOT NULL REFERENCES node_groups(id) ON DELETE CASCADE,
    pi_user_id   TEXT NOT NULL REFERENCES users(id),
    ldap_username TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK(status IN ('pending','approved','denied')),
    requested_at INTEGER NOT NULL,
    resolved_at  INTEGER,
    resolved_by  TEXT,
    note         TEXT NOT NULL DEFAULT ''
);

INSERT INTO pi_member_requests
    (id, group_id, pi_user_id, ldap_username, status, requested_at, resolved_at, resolved_by, note)
SELECT
    id, group_id, pi_user_id, ldap_username, status, requested_at, resolved_at, resolved_by, note
FROM _pi_member_requests_old_103;

DROP TABLE _pi_member_requests_old_103;

CREATE INDEX IF NOT EXISTS idx_pi_member_requests_group  ON pi_member_requests(group_id);
CREATE INDEX IF NOT EXISTS idx_pi_member_requests_status ON pi_member_requests(status);

-- pi_expansion_requests (originally migration 057).
ALTER TABLE pi_expansion_requests RENAME TO _pi_expansion_requests_old_103;

CREATE TABLE pi_expansion_requests (
    id           TEXT PRIMARY KEY,
    group_id     TEXT NOT NULL REFERENCES node_groups(id) ON DELETE CASCADE,
    pi_user_id   TEXT NOT NULL REFERENCES users(id),
    justification TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK(status IN ('pending','acknowledged','dismissed')),
    requested_at INTEGER NOT NULL,
    resolved_at  INTEGER,
    resolved_by  TEXT
);

INSERT INTO pi_expansion_requests
    (id, group_id, pi_user_id, justification, status, requested_at, resolved_at, resolved_by)
SELECT
    id, group_id, pi_user_id, justification, status, requested_at, resolved_at, resolved_by
FROM _pi_expansion_requests_old_103;

DROP TABLE _pi_expansion_requests_old_103;

CREATE INDEX IF NOT EXISTS idx_pi_expansion_requests_group  ON pi_expansion_requests(group_id);
CREATE INDEX IF NOT EXISTS idx_pi_expansion_requests_status ON pi_expansion_requests(status);

-- user_group_memberships (originally migration 043).
ALTER TABLE user_group_memberships RENAME TO _user_group_memberships_old_103;

CREATE TABLE user_group_memberships (
    user_id  TEXT NOT NULL REFERENCES users(id)       ON DELETE CASCADE,
    group_id TEXT NOT NULL REFERENCES node_groups(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK(role IN ('operator')),
    PRIMARY KEY (user_id, group_id)
);

INSERT INTO user_group_memberships (user_id, group_id, role)
SELECT user_id, group_id, role FROM _user_group_memberships_old_103;

DROP TABLE _user_group_memberships_old_103;

CREATE INDEX IF NOT EXISTS idx_ugm_user  ON user_group_memberships(user_id);
CREATE INDEX IF NOT EXISTS idx_ugm_group ON user_group_memberships(group_id);

-------------------------------------------------------------------------------
-- 4. Cleanup.
-------------------------------------------------------------------------------
DROP TABLE _bootstrap_admin;

-- The migration runner verifies PRAGMA foreign_key_check returns zero rows
-- before committing this transaction.  See db.go applyMigration().
