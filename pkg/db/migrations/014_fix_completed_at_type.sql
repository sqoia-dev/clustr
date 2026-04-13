-- Migration 014: coerce any legacy string values in reimage_requests timestamp
-- columns to integer Unix timestamps. SQLite does not enforce column types, so
-- older code paths may have written RFC3339 strings into INTEGER columns. The
-- Go scanner now handles both, but this migration normalises the data at rest
-- so future readers always see integers.

UPDATE reimage_requests
SET scheduled_at = CAST(scheduled_at AS INTEGER)
WHERE scheduled_at IS NOT NULL AND typeof(scheduled_at) = 'text';

UPDATE reimage_requests
SET triggered_at = CAST(triggered_at AS INTEGER)
WHERE triggered_at IS NOT NULL AND typeof(triggered_at) = 'text';

UPDATE reimage_requests
SET started_at = CAST(started_at AS INTEGER)
WHERE started_at IS NOT NULL AND typeof(started_at) = 'text';

UPDATE reimage_requests
SET completed_at = CAST(completed_at AS INTEGER)
WHERE completed_at IS NOT NULL AND typeof(completed_at) = 'text';
