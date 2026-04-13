#!/usr/bin/env bash
# clonr-backup.sh — daily backup for clonr-serverd data
#
# Backs up:
#   1. SQLite database via the SQLite backup API (safe under WAL, unlike cp/rsync)
#   2. ISO cache via rsync to a sibling directory (30-day retention)
#   3. Images inventory (names + sizes only, not blobs — blobs rebuild from ISO cache)
#
# Retention:
#   DB backups   — 14 daily snapshots, older purged via find -mtime
#   ISO cache    — 30-day retention via find -mtime
#
# Logs to journal via logger -t clonr-backup.
# Run as root (systemd unit runs as root).

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration — edit here or override via environment
# ---------------------------------------------------------------------------
DB_PATH="${CLONR_DB_PATH:-/var/lib/clonr/db/clonr.db}"
BACKUP_DIR="${CLONR_BACKUP_DIR:-/var/lib/clonr/backups}"
ISO_CACHE_DIR="${CLONR_ISO_CACHE_DIR:-/var/lib/clonr/iso-cache}"
ISO_BACKUP_DIR="${CLONR_ISO_BACKUP_DIR:-/var/lib/clonr/iso-cache-backup}"
IMAGES_DIR="${CLONR_IMAGE_DIR:-/var/lib/clonr/images}"
IMAGES_INVENTORY_DIR="${BACKUP_DIR}/images-inventory"

DB_RETAIN_DAYS=14
ISO_RETAIN_DAYS=30

TAG="clonr-backup"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() {
    logger -t "${TAG}" -- "$*"
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

die() {
    log "ERROR: $*"
    exit 1
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------
log "Starting backup run"

[[ -f "${DB_PATH}" ]] || die "Database not found at ${DB_PATH}"
command -v sqlite3 >/dev/null || die "sqlite3 not in PATH"
command -v rsync   >/dev/null || die "rsync not in PATH"

mkdir -p "${BACKUP_DIR}" "${ISO_BACKUP_DIR}" "${IMAGES_INVENTORY_DIR}"

# ---------------------------------------------------------------------------
# 1. SQLite hot backup via the backup API
# ---------------------------------------------------------------------------
TIMESTAMP="$(date '+%Y%m%d-%H%M%S')"
DB_BACKUP="${BACKUP_DIR}/clonr-${TIMESTAMP}.db"

log "SQLite backup: ${DB_PATH} -> ${DB_BACKUP}"
sqlite3 "${DB_PATH}" ".backup '${DB_BACKUP}'"

# Verify the backup is a valid SQLite file
TABLES="$(sqlite3 "${DB_BACKUP}" '.tables' 2>&1)" || die "Backup integrity check failed: sqlite3 .tables returned non-zero"
log "Backup verified — tables: ${TABLES}"

# ---------------------------------------------------------------------------
# 2. Retention: purge DB backups older than 14 days
# ---------------------------------------------------------------------------
PURGED_DB=$(find "${BACKUP_DIR}" -maxdepth 1 -name 'clonr-*.db' -mtime "+${DB_RETAIN_DAYS}" -print -delete 2>&1 | wc -l)
log "Purged ${PURGED_DB} DB backup(s) older than ${DB_RETAIN_DAYS} days"

# ---------------------------------------------------------------------------
# 3. ISO cache backup via rsync
#    rsync --archive --delete keeps iso-cache-backup as a mirror.
#    A separate find pass removes files in the backup that haven't been
#    touched in 30 days (i.e., ISOs removed from the live cache long ago).
# ---------------------------------------------------------------------------
if [[ -d "${ISO_CACHE_DIR}" ]]; then
    log "rsync ISO cache: ${ISO_CACHE_DIR}/ -> ${ISO_BACKUP_DIR}/"
    rsync --archive --delete --quiet "${ISO_CACHE_DIR}/" "${ISO_BACKUP_DIR}/"
    PURGED_ISO=$(find "${ISO_BACKUP_DIR}" -maxdepth 1 -type f -mtime "+${ISO_RETAIN_DAYS}" -print -delete 2>&1 | wc -l)
    log "ISO cache sync complete. Purged ${PURGED_ISO} ISO(s) older than ${ISO_RETAIN_DAYS} days from backup"
else
    log "WARNING: ISO cache dir ${ISO_CACHE_DIR} not found — skipping ISO backup"
fi

# ---------------------------------------------------------------------------
# 4. Images inventory (names + sizes, no blobs)
#    Blobs are large and rebuild from ISO cache. Recording inventory allows
#    detecting drift without storing gigabytes.
# ---------------------------------------------------------------------------
INVENTORY_FILE="${IMAGES_INVENTORY_DIR}/images-inventory-${TIMESTAMP}.txt"
if [[ -d "${IMAGES_DIR}" ]]; then
    log "Capturing image inventory to ${INVENTORY_FILE}"
    find "${IMAGES_DIR}" -maxdepth 2 \( -type f -o -type d \) -printf '%s\t%P\n' | sort -k2 > "${INVENTORY_FILE}"
    log "Image inventory: $(wc -l < "${INVENTORY_FILE}") entries recorded"
    # Purge inventory files older than 30 days
    find "${IMAGES_INVENTORY_DIR}" -name 'images-inventory-*.txt' -mtime "+${ISO_RETAIN_DAYS}" -delete
else
    log "WARNING: Images dir ${IMAGES_DIR} not found — skipping inventory"
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
log "Backup complete. DB backup: ${DB_BACKUP} ($(du -h "${DB_BACKUP}" | cut -f1))"
