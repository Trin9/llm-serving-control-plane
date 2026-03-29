#!/bin/bash
# =============================================================================
# LLM Serving Stress Test Runner
# Standardized script to trigger load against the gate-service
# =============================================================================

set -e

# ---- Configuration ----
GATE_URL="${GATE_URL:-http://localhost:8080/v1/chat/completions}"
CONCURRENCY="${CONCURRENCY:-10}"
TOTAL_REQUESTS="${TOTAL_REQUESTS:-100}"
TOKEN="${TOKEN:-eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWUsImlhdCI6MTUxNjIzOTAyMiwidXNlcklEIjoiMjM0NSJ9.svp1zGC0IhWsznkUUxRql6WkpdtwmSuontQnHiTEfEI}"

TEST_BODY_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/../test" && pwd)/stress-test-body.json"

log() { echo "[$(date +%H:%M:%S)] $*"; }

usage() {
    echo "Usage: $0 [options]"
    echo "Options:"
    echo "  -c <num>    Concurrency (default: $CONCURRENCY)"
    echo "  -n <num>    Total requests (default: $TOTAL_REQUESTS)"
    echo "  -u <url>    Gate URL (default: $GATE_URL)"
    exit 1
}

while getopts "c:n:u:h" opt; do
    case "$opt" in
        c) CONCURRENCY=$OPTARG ;;
        n) TOTAL_REQUESTS=$OPTARG ;;
        u) GATE_URL=$OPTARG ;;
        h) usage ;;
        *) usage ;;
    esac
done

if [ ! -f "$TEST_BODY_FILE" ]; then
    echo "ERROR: Test body file not found at $TEST_BODY_FILE"
    exit 1
fi

log "=== Starting Stress Test ==="
log "Target URL:  $GATE_URL"
log "Concurrency: $CONCURRENCY"
log "Total Req:   $TOTAL_REQUESTS"

# 1. Try using ghz (if installed)
if command -v ghz &>/dev/null; then
    log "Using ghz for stress testing..."
    # Note: ghz's HTTP support varies by version. This is a common pattern:
    ghz --insecure \
        --proto "" \
        --call "$GATE_URL" \
        -m POST \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -D "$TEST_BODY_FILE" \
        -n "$TOTAL_REQUESTS" \
        -c "$CONCURRENCY" \
        --format summary
    exit 0
fi

# 2. Try using hey (another common tool)
if command -v hey &>/dev/null; then
    log "Using hey for stress testing..."
    hey -n "$TOTAL_REQUESTS" \
        -c "$CONCURRENCY" \
        -m POST \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -D "$TEST_BODY_FILE" \
        "$GATE_URL"
    exit 0
fi

# 3. Fallback: Concurrent curl loop
log "ghz/hey not found. Falling back to concurrent curl loop (less accurate)..."
send_request() {
    curl -s -X POST "$GATE_URL" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -d @"$TEST_BODY_FILE" > /dev/null
}

export -f send_request
export GATE_URL TOKEN TEST_BODY_FILE

# Use xargs for simple concurrency
seq "$TOTAL_REQUESTS" | xargs -n 1 -P "$CONCURRENCY" -I {} bash -c "send_request"

log "=== Stress Test Finished ==="
