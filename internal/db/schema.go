package db

// schema contains the canonical DDL for all clonr tables.
// Table names use snake_case. All timestamps are Unix epoch seconds (INTEGER).
const schema = `
CREATE TABLE IF NOT EXISTS images (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'pending',
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS image_tags (
    image_id TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
    key      TEXT NOT NULL,
    value    TEXT NOT NULL,
    PRIMARY KEY (image_id, key)
);

CREATE TABLE IF NOT EXISTS nodes (
    id           TEXT PRIMARY KEY,
    hostname     TEXT NOT NULL,
    mac_address  TEXT NOT NULL UNIQUE,
    last_seen_at INTEGER NOT NULL
);
`
