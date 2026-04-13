# Backup and Restore — clonr

## What gets backed up

| Asset | Method | Frequency | Retention |
|---|---|---|---|
| SQLite database (`clonr.db`) | SQLite backup API (hot, WAL-safe) | Daily at 02:00 | 14 days |
| ISO cache (`/var/lib/clonr/iso-cache/`) | rsync mirror | Daily at 02:00 | 30 days |
| Image inventory (names + sizes) | `find` text listing | Daily at 02:00 | 30 days |

Image blobs (`/var/lib/clonr/images/*.blob`) are **not** copied. They are large and fully reproducible from the ISO cache. The inventory snapshot lets you verify which blobs should exist after a rebuild.

## Directory layout on clonr-server

```
/var/lib/clonr/
  db/clonr.db                        # live database
  backups/
    clonr-YYYYMMDD-HHMMSS.db         # DB snapshots (14 kept)
    images-inventory/
      images-inventory-YYYYMMDD-*.txt  # inventory snapshots (30 kept)
  iso-cache/                         # live ISOs
  iso-cache-backup/                  # rsync mirror of iso-cache (30-day purge)
```

## Scripts

| Script | Purpose |
|---|---|
| `/opt/clonr/scripts/clonr-backup.sh` | Backup script (runs via systemd timer) |
| `/opt/clonr/scripts/clonr-restore.sh` | Interactive restore from a named backup file |

Source of record: `scripts/ops/clonr-backup.sh` and `scripts/ops/clonr-restore.sh` in this repo.

## Systemd units

The timer fires at 02:00 every day. If the host was powered off at fire time, `Persistent=true` causes the job to run immediately on next boot.

```bash
# Install and enable (run once on initial deploy or after unit file changes)
sudo cp deploy/systemd/clonr-backup.service /etc/systemd/system/
sudo cp deploy/systemd/clonr-backup.timer   /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now clonr-backup.timer

# Verify timer is armed
sudo systemctl list-timers clonr-backup.timer

# Run a backup immediately (one-shot, does not affect the timer schedule)
sudo systemctl start clonr-backup.service

# Watch live output
sudo journalctl -u clonr-backup -f
```

## Manual backup (without systemd)

```bash
sudo sqlite3 /var/lib/clonr/db/clonr.db \
  ".backup '/var/lib/clonr/backups/clonr-$(date +%Y%m%d-%H%M%S).db'"
```

Verify the backup is valid:

```bash
sqlite3 /var/lib/clonr/backups/clonr-<timestamp>.db ".tables"
```

## Restore procedure

**Prerequisites:** Run as root on clonr-server. Identify the backup file to restore from:

```bash
ls -lh /var/lib/clonr/backups/clonr-*.db
```

**Run the restore script:**

```bash
sudo /opt/clonr/scripts/clonr-restore.sh /var/lib/clonr/backups/clonr-<timestamp>.db
```

The script:
1. Validates the backup file with `sqlite3 .tables` before touching anything.
2. Stops `clonr-serverd`.
3. Saves the current live DB as `/var/lib/clonr/db/clonr.db.pre-restore.<timestamp>`.
4. Copies the backup into place.
5. Validates the restored file.
6. Starts `clonr-serverd` and confirms it is active.

**Non-interactive mode** (for use in scripts — skips the YES prompt):

```bash
sudo CLONR_RESTORE_YES=1 /opt/clonr/scripts/clonr-restore.sh /var/lib/clonr/backups/clonr-<timestamp>.db
```

**Roll back a bad restore:**

```bash
sudo systemctl stop clonr-serverd
sudo mv /var/lib/clonr/db/clonr.db.pre-restore.<timestamp> /var/lib/clonr/db/clonr.db
sudo systemctl start clonr-serverd
```

## Verifying a backup file

```bash
# Tables should include nodes, images, deploy_events, api_keys, etc.
sqlite3 /var/lib/clonr/backups/clonr-<timestamp>.db ".tables"

# Row counts to sanity-check against the live DB
sqlite3 /var/lib/clonr/backups/clonr-<timestamp>.db "SELECT 'nodes', COUNT(*) FROM nodes UNION ALL SELECT 'images', COUNT(*) FROM images;"
```

## Offsite copy recommendation

**Current state: no offsite target is configured.** This is a Sprint 2 follow-up.

Once an offsite or second-host target is available, add a post-backup rsync to the backup script:

```bash
# Example: rsync to a remote host after the local backup completes
rsync -az /var/lib/clonr/backups/ backup-user@<remote-host>:/backups/clonr/
```

Recommended targets in priority order:
1. A second Linode or VM in a different physical rack/region.
2. S3-compatible object storage (Linode Object Storage or equivalent) — `rclone sync` works well.
3. An existing NAS on the provisioning network if it is on separate power/hardware from clonr-server.

Until offsite backups are configured, a disk failure on clonr-server that affects `/var/lib/clonr/` results in loss of all node inventory, image metadata, and deploy history. ISO cache is rebuildable from source images; DB data is not.
