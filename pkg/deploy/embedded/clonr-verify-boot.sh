#!/bin/sh
# clonr-verify-boot: phone-home script injected by clonr into the deployed OS.
# Reads /etc/clonr/node-token and /etc/clonr/verify-boot-url, collects basic
# system info, and POSTs it to the clonr-serverd verify-boot endpoint.
# ADR-0008.
set -eu

TOKEN_FILE="/etc/clonr/node-token"
URL_FILE="/etc/clonr/verify-boot-url"

if [ ! -f "$TOKEN_FILE" ]; then
    echo "clonr-verify-boot: $TOKEN_FILE not found — skipping phone-home" >&2
    exit 1
fi
if [ ! -f "$URL_FILE" ]; then
    echo "clonr-verify-boot: $URL_FILE not found — skipping phone-home" >&2
    exit 1
fi

TOKEN=$(cat "$TOKEN_FILE")
URL=$(cat "$URL_FILE")

HOSTNAME=$(hostname)
KERNEL=$(uname -r)
UPTIME_SEC=$(awk '{print int($1)}' /proc/uptime)
SYSTEMCTL_STATE=$(systemctl is-system-running 2>/dev/null || true)
OS_RELEASE=""
if [ -f /etc/os-release ]; then
    OS_RELEASE=$(cat /etc/os-release)
fi

# Escape special chars in OS_RELEASE for JSON embedding.
# Use printf %s to avoid double-expansion; jq is not guaranteed present,
# so we do minimal escaping: backslash, double-quote, and control chars.
OS_RELEASE_ESCAPED=$(printf '%s' "$OS_RELEASE" \
    | sed 's/\\/\\\\/g' \
    | sed 's/"/\\"/g' \
    | tr '\n' '\\' \
    | sed 's/\\/\\n/g')

PAYLOAD=$(printf '{"hostname":"%s","kernel_version":"%s","uptime_seconds":%s,"systemctl_state":"%s","os_release":"%s"}' \
    "$HOSTNAME" "$KERNEL" "$UPTIME_SEC" "$SYSTEMCTL_STATE" "$OS_RELEASE_ESCAPED")

HTTP_CODE=$(curl --silent --output /dev/null --write-out "%{http_code}" \
    --max-time 30 \
    --retry 0 \
    -X POST "$URL" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD")

if [ "$HTTP_CODE" = "204" ]; then
    echo "clonr-verify-boot: phone-home accepted (204) — boot verified" >&2
    exit 0
else
    echo "clonr-verify-boot: unexpected HTTP status $HTTP_CODE from $URL" >&2
    exit 1
fi
