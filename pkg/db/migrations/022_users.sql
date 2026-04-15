-- 022_users.sql — ADR-0007: user accounts and first-run bootstrap.
--
-- Adds the users table for username/password auth with role-based access control.
-- Also extends api_keys with an optional user_id foreign key for personal API keys.

CREATE TABLE users (
    id                   TEXT PRIMARY KEY,
    username             TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash        TEXT NOT NULL,
    role                 TEXT NOT NULL CHECK(role IN ('admin', 'operator', 'readonly')),
    must_change_password INTEGER NOT NULL DEFAULT 0,
    created_at           INTEGER NOT NULL,
    last_login_at        INTEGER,
    disabled_at          INTEGER
);

-- Extend api_keys to optionally associate a key with its creating user.
-- Nullable — existing keys created before this migration have no owner.
ALTER TABLE api_keys ADD COLUMN user_id TEXT REFERENCES users(id);
