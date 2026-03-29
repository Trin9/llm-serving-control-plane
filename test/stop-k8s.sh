#!/bin/bash
# =============================================================================
# LLM Serving K8s Mode Stop Script (Helm)
# =============================================================================

log() { echo "[$(date +%H:%M:%S)] $*"; }

main() {
    log "=== Stopping LLM Serving in K8s Mode (Helm) ==="
    
    # 1. First, delete InferenceService CRs to allow operator to clean up managed resources
    log "Deleting InferenceService CRs..."
    kubectl delete inferenceservices mock-service --ignore-not-found=true || true
    
    # Wait for the operator to clean up the managed Deployment and Service
    log "Waiting for managed resources to be cleaned up..."
    sleep 5
    
    # 2. Uninstall monitoring stack
    log "Uninstalling monitoring-stack..."
    helm uninstall monitoring-stack || true
    
    # 3. Uninstall llm-operator
    log "Uninstalling llm-operator..."
    helm uninstall llm-operator || true
    
    log "=== K8s Mode Stopped ==="
}

main "$@"