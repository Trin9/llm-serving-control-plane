#!/bin/bash
# =============================================================================
# LLM Serving K8s Mode Stop Script (Helm)
# =============================================================================

log() { echo "[$(date +%H:%M:%S)] $*"; }

main() {
    log "=== Stopping LLM Serving in K8s Mode (Helm) ==="
    
    helm uninstall monitoring-stack || true
    helm uninstall llm-operator || true
    
    log "=== K8s Mode Stopped ==="
}

main "$@"
