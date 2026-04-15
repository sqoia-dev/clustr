#!/usr/bin/env bash
# lab-validate.sh — Proxmox lab validation gate for clonr pre-release
#
# What this does:
#   1. Deploys the current HEAD of clonr-serverd to 192.168.1.151
#   2. Rebuilds initramfs via the clonr API
#   3. Reimages lab VMs 201, 202, 206, 207 concurrently
#   4. Waits for each VM to reach a serial console login prompt
#   5. Exits 0 only if all 4 VMs show "<hostname> login:" within 5 minutes
#
# Artifacts produced (written to $ARTIFACT_DIR):
#   serial-vmNNN.log     — full serial console capture per VM
#   deploy-vmNNN.log     — deploy event log per VM
#   timings.json         — boot timing per VM (seconds from reimage to login prompt)
#
# Exit codes:
#   0 — all VMs booted to login prompt
#   1 — one or more VMs failed or timed out
#   2 — setup/deploy step failed (pre-validation failure)
#
# Environment variables (required):
#   CLONR_SERVER_URL     — e.g. http://10.99.0.1:8080
#   CLONR_ADMIN_KEY      — admin API key
#   PROXMOX_HOST         — e.g. 192.168.1.223
#   PROXMOX_SSH_KEY      — path to SSH key for Proxmox host (root access)
#   LAB_SSH_KEY          — path to SSH key for clonr-server VM (192.168.1.151, root access)
#   LAB_SERVER_IP        — clonr server VM IP (default: 192.168.1.151)
#   ARTIFACT_DIR         — directory for output artifacts (default: /tmp/lab-validate-artifacts)
#   BOOT_TIMEOUT         — seconds to wait for login prompt per VM (default: 300)

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
CLONR_SERVER_URL="${CLONR_SERVER_URL:-http://10.99.0.1:8080}"
CLONR_ADMIN_KEY="${CLONR_ADMIN_KEY:?CLONR_ADMIN_KEY is required}"
PROXMOX_HOST="${PROXMOX_HOST:-192.168.1.223}"
PROXMOX_SSH_KEY="${PROXMOX_SSH_KEY:?PROXMOX_SSH_KEY is required}"
LAB_SSH_KEY="${LAB_SSH_KEY:?LAB_SSH_KEY is required}"
LAB_SERVER_IP="${LAB_SERVER_IP:-192.168.1.151}"
ARTIFACT_DIR="${ARTIFACT_DIR:-/tmp/lab-validate-artifacts}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-300}"

# VMIDs to validate
VM_IDS=(201 202 206 207)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*" >&2; }
die() { log "FATAL: $*"; exit 2; }

ssh_proxmox() {
    ssh -o StrictHostKeyChecking=no -o BatchMode=yes -i "$PROXMOX_SSH_KEY" \
        root@"$PROXMOX_HOST" "$@"
}

ssh_lab() {
    ssh -o StrictHostKeyChecking=no -o BatchMode=yes -i "$LAB_SSH_KEY" \
        root@"$LAB_SERVER_IP" "$@"
}

clonr_api() {
    local method="$1" path="$2"
    shift 2
    curl -sf -X "$method" \
        -H "Authorization: Bearer $CLONR_ADMIN_KEY" \
        -H "Content-Type: application/json" \
        "$CLONR_SERVER_URL$path" "$@"
}

mkdir -p "$ARTIFACT_DIR"

# ---------------------------------------------------------------------------
# Step 1: Deploy HEAD to clonr-server VM
# ---------------------------------------------------------------------------
log "Step 1: Building and deploying clonr-serverd from HEAD"
DEPLOY_LOG="$ARTIFACT_DIR/deploy-server.log"

# Determine the binary to copy (built by CI before this script runs,
# or build here if running locally)
if [[ -f "./bin/clonr-serverd" ]]; then
    BINARY="./bin/clonr-serverd"
elif [[ -f "/tmp/clonr-serverd" ]]; then
    BINARY="/tmp/clonr-serverd"
else
    log "No pre-built binary found — building from source"
    GOTOOLCHAIN=auto CGO_ENABLED=0 go build -o /tmp/clonr-serverd ./cmd/clonr-serverd 2>>"$DEPLOY_LOG" \
        || die "Build failed — see $DEPLOY_LOG"
    BINARY="/tmp/clonr-serverd"
fi

log "Copying binary to lab server"
scp -o StrictHostKeyChecking=no -i "$LAB_SSH_KEY" "$BINARY" root@"$LAB_SERVER_IP":/tmp/clonr-serverd.new >> "$DEPLOY_LOG" 2>&1 \
    || die "scp failed"

log "Installing and restarting clonr-serverd"
ssh_lab 'bash -s' >> "$DEPLOY_LOG" 2>&1 << 'REMOTE'
set -euo pipefail
systemctl stop clonr-serverd
install -m 0755 /tmp/clonr-serverd.new /usr/local/bin/clonr-serverd
rm -f /tmp/clonr-serverd.new
systemctl start clonr-serverd
# Wait up to 10s for health check
for i in $(seq 1 20); do
    if curl -sf http://localhost:8080/api/v1/health >/dev/null 2>&1; then
        echo "Health check passed"
        exit 0
    fi
    sleep 0.5
done
echo "Health check failed after 10s"
exit 1
REMOTE
log "clonr-serverd deployed and healthy"

# ---------------------------------------------------------------------------
# Step 2: Rebuild initramfs via API
# ---------------------------------------------------------------------------
log "Step 2: Rebuilding initramfs"
INITRAMFS_JOB=$(clonr_api POST /api/v1/initramfs/rebuild 2>>"$ARTIFACT_DIR/initramfs.log" \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('job_id',''))" 2>/dev/null || echo "")

if [[ -z "$INITRAMFS_JOB" ]]; then
    log "WARNING: Could not get initramfs job ID — proceeding with existing initramfs"
else
    log "Initramfs rebuild job: $INITRAMFS_JOB — waiting for completion"
    for i in $(seq 1 60); do
        STATUS=$(clonr_api GET "/api/v1/initramfs/rebuild/$INITRAMFS_JOB" 2>/dev/null \
            | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','unknown'))" 2>/dev/null || echo "unknown")
        if [[ "$STATUS" == "complete" ]]; then
            log "Initramfs rebuild complete"
            break
        elif [[ "$STATUS" == "failed" ]]; then
            die "Initramfs rebuild failed"
        fi
        sleep 5
    done
fi

# ---------------------------------------------------------------------------
# Step 3: Reimage all VMs concurrently
# ---------------------------------------------------------------------------
log "Step 3: Reimaging VMs ${VM_IDS[*]} concurrently"

# Get the first available image ID
IMAGE_ID=$(clonr_api GET /api/v1/images 2>/dev/null \
    | python3 -c "import sys,json; imgs=json.load(sys.stdin); print(imgs[0]['id'] if imgs else '')" 2>/dev/null || echo "")
[[ -n "$IMAGE_ID" ]] || die "No images available in clonr — cannot reimage"
log "Using image ID: $IMAGE_ID"

reimage_vm() {
    local vmid="$1"
    local deploy_log="$ARTIFACT_DIR/deploy-vm${vmid}.log"
    log "VM${vmid}: triggering reimage"

    # Power cycle the VM to PXE boot via Proxmox
    ssh_proxmox "qm set ${vmid} --boot order=net0 2>/dev/null; qm stop ${vmid} --skiplock 2>/dev/null; sleep 2; qm start ${vmid}" >> "$deploy_log" 2>&1 \
        || { log "VM${vmid}: failed to power cycle"; return 1; }

    # Get node ID by hostname/MAC lookup from clonr API (best effort)
    local node_id
    node_id=$(clonr_api GET /api/v1/nodes 2>/dev/null \
        | python3 -c "import sys,json; nodes=json.load(sys.stdin); n=[x for x in nodes if x.get('vm_id')==${vmid} or 'node-0${vmid}' in x.get('hostname','')]; print(n[0]['id'] if n else '')" 2>/dev/null || echo "")
    if [[ -n "$node_id" ]]; then
        # Trigger deploy via API
        clonr_api POST "/api/v1/nodes/${node_id}/deploy" \
            -d "{\"image_id\":\"${IMAGE_ID}\"}" >> "$deploy_log" 2>&1 || true
        log "VM${vmid}: deploy triggered for node $node_id"
    else
        log "VM${vmid}: no matching node record — will PXE boot and auto-enroll"
    fi

    log "VM${vmid}: reimage initiated"
}

REIMAGE_PIDS=()
for vmid in "${VM_IDS[@]}"; do
    reimage_vm "$vmid" &
    REIMAGE_PIDS+=($!)
done

# Wait for all reimage triggers to complete
REIMAGE_FAILED=0
for i in "${!VM_IDS[@]}"; do
    if ! wait "${REIMAGE_PIDS[$i]}"; then
        log "VM${VM_IDS[$i]}: reimage trigger failed"
        REIMAGE_FAILED=1
    fi
done
[[ "$REIMAGE_FAILED" -eq 0 ]] || die "One or more reimage triggers failed"

# ---------------------------------------------------------------------------
# Step 4 & 5: Wait for login prompts via serial console capture
# ---------------------------------------------------------------------------
log "Step 4: Waiting for login prompts (timeout: ${BOOT_TIMEOUT}s per VM)"

TIMINGS_FILE="$ARTIFACT_DIR/timings.json"
echo "{}" > "$TIMINGS_FILE"

wait_for_login() {
    local vmid="$1"
    local serial_log="$ARTIFACT_DIR/serial-vm${vmid}.log"
    local start_ts
    start_ts=$(date +%s)
    local deadline
    deadline=$((start_ts + BOOT_TIMEOUT))

    log "VM${vmid}: starting serial console capture"
    # Capture serial console output. qm terminal exits when the VM is reset or stopped;
    # we loop it so we catch output across PXE boot -> deploy -> reboot -> OS boot.
    # We pipe through tee to capture and also search the stream.
    : > "$serial_log"

    while true; do
        local now
        now=$(date +%s)
        if [[ "$now" -ge "$deadline" ]]; then
            log "VM${vmid}: TIMEOUT — no login prompt within ${BOOT_TIMEOUT}s"
            return 1
        fi

        local remaining=$(( deadline - now ))

        # Capture up to $remaining seconds of serial output, appending to log
        timeout "$remaining" bash -c \
            "ssh -o StrictHostKeyChecking=no -i '$PROXMOX_SSH_KEY' root@'$PROXMOX_HOST' \
             'timeout $((remaining - 1)) qm terminal ${vmid} --iface serial0 2>/dev/null'" \
            >> "$serial_log" 2>/dev/null || true

        # Check if login prompt is present in captured output
        # Pattern: "<hostname> login:" (standard getty output)
        if grep -qE '[a-zA-Z0-9_-]+ login:' "$serial_log" 2>/dev/null; then
            local elapsed=$(( $(date +%s) - start_ts ))
            log "VM${vmid}: LOGIN PROMPT detected at ${elapsed}s"
            # Record timing
            python3 -c "
import json, sys
f='$TIMINGS_FILE'
try:
    d=json.load(open(f))
except:
    d={}
d['vm${vmid}']=${elapsed}
json.dump(d,open(f,'w'),indent=2)
" 2>/dev/null || true
            return 0
        fi

        # If the VM might be rebooting after deploy, give it a moment and retry
        sleep 5
    done
}

# Launch all watchers concurrently
WATCH_PIDS=()
WATCH_STATUS=()
for vmid in "${VM_IDS[@]}"; do
    wait_for_login "$vmid" &
    WATCH_PIDS+=($!)
done

# Collect results
OVERALL_EXIT=0
for i in "${!VM_IDS[@]}"; do
    vmid="${VM_IDS[$i]}"
    if wait "${WATCH_PIDS[$i]}"; then
        WATCH_STATUS+=("PASS:vm${vmid}")
    else
        WATCH_STATUS+=("FAIL:vm${vmid}")
        OVERALL_EXIT=1
    fi
done

# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------
log "=== Lab Validation Report ==="
for status in "${WATCH_STATUS[@]}"; do
    log "  $status"
done
log "Timings: $(cat "$TIMINGS_FILE" 2>/dev/null || echo '{}')"
log "Artifacts in: $ARTIFACT_DIR"

if [[ "$OVERALL_EXIT" -eq 0 ]]; then
    log "RESULT: PASS — all VMs reached login prompt"
else
    log "RESULT: FAIL — one or more VMs did not reach login prompt"
fi

exit "$OVERALL_EXIT"
