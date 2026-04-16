#!/usr/bin/env bash
# backup.sh — Create a Qdrant snapshot for the memory collection.
#
# Snapshots are stored inside the container at /qdrant/snapshots (mounted to
# /root/memory/qdrant_snapshots on the host). Point Resilio Sync / rsync /
# rclone at that directory — this script doesn't transfer files anywhere.
#
# Configuration (environment variables):
#
#   QDRANT_URL        Qdrant base URL  (default: http://localhost:6333 — no auth needed)
#                     Override with https://qdrant.<domain> if running off-host.
#   MEMORY_USER       Basic Auth username  (only needed when QDRANT_URL is https://)
#   MEMORY_PASS       Basic Auth password  (only needed when QDRANT_URL is https://)
#   COLLECTION        Qdrant collection name   (default: memory)
#   KEEP_SNAPSHOTS    How many snapshots to retain; 0 = keep all  (default: 7)
#   ENV_FILE          Path to a .env file to source               (default: .env next to this script)
#
# Usage:
#   ./backup.sh
#   ENV_FILE=/root/memory/.env ./backup.sh
#
# Cron example (daily at 03:00):
#   0 3 * * * ENV_FILE=/root/memory/.env /root/memory/backup.sh >> /var/log/qdrant-backup.log 2>&1

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- Load .env file if credentials not already in environment ---
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env}"
if [[ -f "$ENV_FILE" && -z "${MEMORY_USER:-}" ]]; then
    # shellcheck disable=SC1090
    set -a; source "$ENV_FILE"; set +a
fi

# --- Resolve configuration ---
QDRANT_URL="${QDRANT_URL:-http://localhost:6333}"
COLLECTION="${COLLECTION:-memory}"
KEEP_SNAPSHOTS="${KEEP_SNAPSHOTS:-7}"

# Basic Auth — optional, only used when credentials are set
CURL_AUTH=()
if [[ -n "${MEMORY_USER:-}" && -n "${MEMORY_PASS:-}" ]]; then
    CURL_AUTH=(-u "${MEMORY_USER}:${MEMORY_PASS}")
fi

TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "[$TIMESTAMP] Creating snapshot for '${COLLECTION}' at ${QDRANT_URL}..."

# --- Create snapshot ---
RESPONSE=$(curl -sf \
    "${CURL_AUTH[@]}" \
    -X POST "${QDRANT_URL}/collections/${COLLECTION}/snapshots")

SNAPSHOT_NAME=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['result']['name'])" "$RESPONSE" 2>/dev/null || true)

if [[ -z "$SNAPSHOT_NAME" ]]; then
    echo "ERROR: Could not parse snapshot name from response:"
    echo "$RESPONSE"
    exit 1
fi

echo "Created: $SNAPSHOT_NAME"

# --- Prune old snapshots ---
if [[ "$KEEP_SNAPSHOTS" -gt 0 ]]; then
    LIST=$(curl -sf \
        "${CURL_AUTH[@]}" \
        "${QDRANT_URL}/collections/${COLLECTION}/snapshots")

    # Extract names sorted oldest-first; drop the last KEEP_SNAPSHOTS entries
    OLD_SNAPSHOTS=$(python3 - "$LIST" "$KEEP_SNAPSHOTS" <<'EOF'
import sys, json
data = json.loads(sys.argv[1])
keep = int(sys.argv[2])
names = sorted(s["name"] for s in data["result"])
to_delete = names[:-keep] if len(names) > keep else []
print("\n".join(to_delete))
EOF
)

    while IFS= read -r name; do
        [[ -z "$name" ]] && continue
        echo "Deleting old snapshot: $name"
        curl -sf \
            "${CURL_AUTH[@]}" \
            -X DELETE "${QDRANT_URL}/collections/${COLLECTION}/snapshots/${name}" \
            > /dev/null
    done <<< "$OLD_SNAPSHOTS"
fi

echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Done."
