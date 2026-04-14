-- 019_initramfs_builds: track initramfs rebuild history.
--
-- Keeps the last N initramfs builds so operators can audit when the initramfs
-- was last rebuilt, by whom, and verify hash continuity.
-- The application trims to 5 rows after each insert.

CREATE TABLE IF NOT EXISTS initramfs_builds (
    id                  TEXT PRIMARY KEY,
    started_at          INTEGER NOT NULL,
    finished_at         INTEGER,
    sha256              TEXT NOT NULL DEFAULT '',
    size_bytes          INTEGER NOT NULL DEFAULT 0,
    kernel_version      TEXT NOT NULL DEFAULT '',
    triggered_by_prefix TEXT NOT NULL DEFAULT '',
    outcome             TEXT NOT NULL DEFAULT 'pending'
);

CREATE INDEX IF NOT EXISTS idx_initramfs_builds_started ON initramfs_builds(started_at DESC);
