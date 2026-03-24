#!/bin/bash
# =============================================================================
# LLM Serving K8s Mode Start Script (Helm)
# =============================================================================

set -e

PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
log() { echo "[$(date +%H:%M:%S)] $*"; }

main() {
    log "=== Starting LLM Serving in K8s Mode (Helm) ==="
    
    if ! command -v helm &>/dev/null; then
        echo "ERROR: helm not found."
        exit 1
    fi

    # 1. Deploy Operator
    log "Installing/Upgrading llm-operator..."
    helm upgrade --install llm-operator "$PROJECT_ROOT_DIR/helm/llm-operator" --wait

    # 2. Deploy Monitoring Stack
    log "Installing/Upgrading monitoring-stack..."
    helm upgrade --install monitoring-stack "$PROJECT_ROOT_DIR/helm/monitoring-stack" --wait

    log "Checking pods..."
    kubectl get pods

    echo ""
    echo "  Access Grafana:   kubectl port-forward service/monitoring-stack-grafana 3000:80"
    echo "  Access Prometheus: kubectl port-forward service/monitoring-stack-prometheus-server 9090:80"
    echo ""
    log "=== K8s Mode Started ==="
}

main "$@"
