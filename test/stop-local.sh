#!/bin/bash
# =============================================================================
# LLM Serving Local Mode Stop Script (Docker Compose)
# =============================================================================

PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log() { echo "[$(date +%H:%M:%S)] $*"; }

# Helper to detect Docker Compose command
get_docker_compose() {
    if command -v docker-compose &>/dev/null; then
        echo "docker-compose"
    elif docker compose version &>/dev/null; then
        echo "docker compose"
    else
        echo ""
    fi
}

main() {
    log "=== Stopping LLM Serving in Local Mode (Docker Compose) ==="
    
    DOCKER_COMPOSE_CMD=$(get_docker_compose)
    if [ -z "$DOCKER_COMPOSE_CMD" ]; then
        echo "ERROR: Neither 'docker-compose' nor 'docker compose' found. Cannot stop Docker Compose services."
        exit 1
    fi

    cd "$PROJECT_ROOT_DIR"
    log "Bringing down containers using '$DOCKER_COMPOSE_CMD'..."
    $DOCKER_COMPOSE_CMD down
    
    log "=== Local Mode Stopped ==="
}

main "$@"
