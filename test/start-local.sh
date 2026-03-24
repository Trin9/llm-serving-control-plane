#!/bin/bash
# =============================================================================
# LLM Serving Local Mode Start Script (Docker Compose)
# =============================================================================

set -e

PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log() { echo "[$(date +%H:%M:%S)] $*"; }

main() {
    log "=== Starting LLM Serving in Local Mode (Docker Compose) ==="
    
    if ! command -v docker-compose &>/dev/null; then
        echo "ERROR: docker-compose not found."
        exit 1
    fi

    cd "$PROJECT_ROOT_DIR"
    log "Bringing up containers..."
    docker-compose up -d

    log "Checking service health..."
    # Add a small wait for services to stabilize
    sleep 5
    
    echo ""
    echo "  vLLM (mock):   http://localhost:8000"
    echo "  gate-service:  http://localhost:8080"
    echo "  Prometheus:    http://localhost:9090"
    echo "  Grafana:       http://localhost:3000 (admin/admin)"
    echo ""
    log "=== Local Mode Started ==="
}

main "$@"
