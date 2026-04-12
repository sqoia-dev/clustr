CREATE TABLE IF NOT EXISTS reimage_requests (
    id            TEXT PRIMARY KEY,
    node_id       TEXT NOT NULL REFERENCES node_configs(id),
    image_id      TEXT NOT NULL REFERENCES base_images(id),
    status        TEXT NOT NULL DEFAULT 'pending',
    scheduled_at  INTEGER,
    triggered_at  INTEGER,
    started_at    INTEGER,
    completed_at  INTEGER,
    error_message TEXT NOT NULL DEFAULT '',
    requested_by  TEXT NOT NULL DEFAULT 'api',
    dry_run       INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reimage_requests_node      ON reimage_requests(node_id);
CREATE INDEX IF NOT EXISTS idx_reimage_requests_status    ON reimage_requests(status);
CREATE INDEX IF NOT EXISTS idx_reimage_requests_scheduled ON reimage_requests(scheduled_at)
    WHERE scheduled_at IS NOT NULL;
