#!/usr/bin/env bash
# clonr-restore.sh — restore clonr SQLite database from a named backup file
#
# Usage:
#   sudo ./clonr-restore.sh <backup-file>
#
# Examples:
#   sudo ./clonr-restore.sh /var/lib/clonr/backups/clonr-20260413-020001.db
#   sudo ./clonr-restore.sh clonr-20260413-020001.db   # looked up in BACKUP_DIR
#
# The script stops clonr-serverd, validates the backup, replaces the live DB,
# then restarts clonr-serverd. It leaves the old DB as .pre-restore.<timestamp>
# so you can roll back with one mv if the restore turns out to be wrong.
#
# Run as root.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
DB_PATH="${CLONR_DB_PATH:-/var/lib/clonr/db/clonr.db}"
BACKUP_DIR="${CLONR_BACKUP_DIR:-/var/lib/clonr/backups}"
SERVICE="clonr-serverd"

TAG="clonr-restore"

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
[[ "${EUID}" -eq 0 ]] || die "Must run as root (sudo ${0} <backup-file>)"
[[ $# -ge 1 ]]        || { echo "Usage: sudo ${0} <backup-file>"; exit 1; }

BACKUP_ARG="${1}"

# Resolve backup path — accept full path or just filename
if [[ -f "${BACKUP_ARG}" ]]; then
    BACKUP_FILE="${BACKUP_ARG}"
elif [[ -f "${BACKUP_DIR}/${BACKUP_ARG}" ]]; then
    BACKUP_FILE="${BACKUP_DIR}/${BACKUP_ARG}"
else
    die "Backup file not found: ${BACKUP_ARG} (also checked ${BACKUP_DIR}/${BACKUP_ARG})"
fi

log "Restore requested from: ${BACKUP_FILE}"

# Validate the backup file before touching anything
command -v sqlite3 >/dev/null || die "sqlite3 not in PATH"
TABLES="$(sqlite3 "${BACKUP_FILE}" '.tables' 2>&1)" || die "Backup file failed integrity check: ${TABLES}"
log "Backup validated — tables: ${TABLES}"

# ---------------------------------------------------------------------------
# Confirmation prompt (skipped when CLONR_RESTORE_YES=1)
# ---------------------------------------------------------------------------
if [[ "${CLONR_RESTORE_YES:-0}" != "1" ]]; then
    echo ""
    echo "  WARNING: This will REPLACE the live database at ${DB_PATH}"
    echo "  with: ${BACKUP_FILE}"
    echo ""
    echo "  A .pre-restore copy will be kept at ${DB_PATH}.pre-restore.<timestamp>"
    echo "  clonr-serverd will be stopped and restarted."
    echo ""
    read -r -p "  Type YES to continue: " CONFIRM
    [[ "${CONFIRM}" == "YES" ]] || { log "Restore aborted by operator."; exit 0; }
fi

# ---------------------------------------------------------------------------
# Stop service
# ---------------------------------------------------------------------------
log "Stopping ${SERVICE}"
systemctl stop "${SERVICE}"

# Give it a moment to flush WAL checkpoints
sleep 2

# ---------------------------------------------------------------------------
# Preserve the current live DB
# ---------------------------------------------------------------------------
TIMESTAMP="$(date '+%Y%m%d-%H%M%S')"
ROLLBACK_PATH="${DB_PATH}.pre-restore.${TIMESTAMP}"
if [[ -f "${DB_PATH}" ]]; then
    cp -a "${DB_PATH}" "${ROLLBACK_PATH}"
    log "Live DB preserved at: ${ROLLBACK_PATH}"
fi

# Also preserve any WAL/SHM sidecar files if present
for SIDECAR in "${DB_PATH}-wal" "${DB_PATH}-shm"; do
    [[ -f "${SIDECAR}" ]] && mv "${SIDECAR}" "${SIDECAR}.pre-restore.${TIMESTAMP}" && log "Preserved sidecar: ${SIDECAR}"
done

# ---------------------------------------------------------------------------
# Restore
# ---------------------------------------------------------------------------
log "Copying backup to ${DB_PATH}"
cp -a "${BACKUP_FILE}" "${DB_PATH}"
chmod 644 "${DB_PATH}"

# Final sanity check on the restored file
sqlite3 "${DB_PATH}" '.tables' >/dev/null || {
    log "Restored file failed integrity check — rolling back"
    mv "${ROLLBACK_PATH}" "${DB_PATH}"
    systemctl start "${SERVICE}"
    die "Restore failed integrity check; rolled back to pre-restore DB and restarted service"
}

# ---------------------------------------------------------------------------
# Restart service
# ---------------------------------------------------------------------------
log "Starting ${SERVICE}"
systemctl start "${SERVICE}"
sleep 3

STATUS="$(systemctl is-active "${SERVICE}" 2>&1)"
if [[ "${STATUS}" == "active" ]]; then
    log "Restore complete. ${SERVICE} is active. Rollback copy at: ${ROLLBACK_PATH}"
else
    log "WARNING: ${SERVICE} is not active after restore (status: ${STATUS}). Check: journalctl -u ${SERVICE} -n 50"
    log "Rollback copy available at: ${ROLLBACK_PATH}"
    exit 1
fi
