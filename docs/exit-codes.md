# clonr Deploy Agent Exit Codes

The clonr deploy agent (`clonr deploy --auto`) exits with a classified code so
operators and monitoring systems can triage failures without reading logs.

Exit codes are also captured in the `reimage_requests` table (`exit_code`,
`exit_name`, `phase` columns) and visible on the node detail page.

## Code Table

| Code | Name | Phase | Meaning | Common Causes | Operator Next Step |
|------|------|-------|---------|---------------|--------------------|
| 0 | `success` | — | Deploy completed successfully | — | Node will boot from disk on next PXE |
| 1 | `generic` | — | Reserved generic failure | Unclassified internal error | Check agent logs; open an issue |
| 2 | `config` | config | Missing or invalid configuration | `CLONR_SERVER`, `CLONR_MAC`, or `CLONR_TOKEN` not set in initramfs | Rebuild initramfs with correct env vars |
| 3 | `auth` | auth | Authentication or authorization failure | Wrong API token; node key not provisioned | Verify node API key; re-provision key |
| 4 | `image_fetch` | image_fetch | Image metadata fetch failed | Image not ready; server unreachable; invalid image ID | Check image status via `clonr image details <id>` |
| 5 | `download` | deploy | Blob stream failed or throughput watchdog tripped | Network cut during download; server disk full; checksum mismatch | Retry reimage; check server disk space |
| 6 | `partition` | partition | Disk partitioning failed | `sgdisk`/`partprobe`/loopdev error; disk in use; wrong device | Check `lsblk` on node; verify disk layout |
| 7 | `format` | format | Filesystem creation failed | `mkfs.*` error; `EBUSY`; unsupported filesystem | Verify target disk is not mounted; check kernel modules |
| 8 | `extract` | extract | Rootfs extraction failed | `tar`/`rsync` error; no space on target; corrupt image | Check target partition size; re-verify image checksum |
| 9 | `finalize` | finalize | Node configuration application failed | `fstab`/`machine-id`/`systemd-nspawn` error; missing bind mount | Review finalize logs; check image has systemd in chroot |
| 10 | `bootloader` | bootloader | Bootloader installation failed | `grub2-install` or `efibootmgr` error; missing EFI partition | Verify disk layout includes EFI/biosboot partition |
| 11 | `callback` | callback | Server callback failed | `deploy-complete`/`deploy-failed` POST failed after retries | Server may have recorded success; check node state via API |
| 12 | `network` | network | Network connectivity failure | DHCP timeout; DNS failure; TCP to server unreachable | Check PXE network; verify server IP reachable from node |
| 13 | `hardware` | hardware | Hardware enumeration failed | No usable NIC found; no matching disk; `lshw`/`lsblk` error | Check hardware; verify node meets minimum disk requirements |
| 64 | `unknown` | unknown | Unclassified catch-all | Error path not yet wrapped with a specific code | Check agent logs for full error message |
| 99 | `panic` | panic | Deploy agent panicked | Nil pointer or runtime panic in deploy code | File a bug with the panic stacktrace from server logs |

## Reading Exit Codes

### Via reimage history (UI)
Open the node detail page. The **Reimage History** card shows exit code, name,
and phase for the last 10 deploys.

### Via API
```
GET /api/v1/reimages?node_id=<id>&status=failed
```
Returns records with `exit_code`, `exit_name`, `phase`, and `error_message`.

### Via systemd / initramfs
```
journalctl -u clonr-deploy --no-pager | grep "deploy failed"
```
The structured log line includes `exit_code`, `exit_name`, and `phase` fields.

### Via shell exit status
```bash
clonr deploy --auto; echo "exit: $?"
```

## Adding New Codes

To add a new classified failure in the agent:

1. Add a constant to `cmd/clonr/exitcodes.go` with the next available integer.
2. Add a row to the table in this file.
3. Wrap the failure site: `return Wrap(ExitNewCode, "phase_name", err)`.
