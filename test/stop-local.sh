#!/bin/bash
# =============================================================================
# LLM Serving Local Mode Stop Script (Docker Compose)
# =============================================================================

PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log() { echo "[$(date +%H:%M:%S)] $*"; }

main() {
    log "=== Stopping LLM Serving in Local Mode (Docker Compose) ==="
    
    cd "$PROJECT_ROOT_DIR"
    docker-compose down
    
    log "=== Local Mode Stopped ==="
}

main "$@"
