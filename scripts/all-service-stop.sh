#!/bin/bash
# =============================================================================
# LLM Serving 全量服务一键停止脚本
# =============================================================================

log() { echo "[$(date +%H:%M:%S)] $*"; }

PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

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

stop_by_pattern() {
  local pattern=$1
  local name=$2
  local pids
  pids=$(pgrep -f "$pattern" 2>/dev/null || true)
  if [ -n "$pids" ]; then
    log "Stopping $name (PIDs: $pids)..."
    kill $pids 2>/dev/null || true
    sleep 2
    kill -9 $pids 2>/dev/null || true
  else
    log "$name not running as a manual process."
  fi
}

main() {
  log "=== LLM Serving All Services Stop ==="

  # Determine deployment environment
  if command -v kubectl &>/dev/null && [ -n "$KUBECONFIG" ]; then
    DEPLOY_ENV="kubernetes"
    log "Detected Kubernetes environment."
  elif [ -f "$PROJECT_ROOT_DIR/docker-compose.yml" ]; then
    DOCKER_COMPOSE_CMD=$(get_docker_compose)
    if [ -z "$DOCKER_COMPOSE_CMD" ]; then
        log "docker-compose.yml found, but neither 'docker-compose' nor 'docker compose' command is available. Attempting manual cleanup."
        DEPLOY_ENV="manual"
    else
        DEPLOY_ENV="local-docker-compose"
        log "Detected local Docker Compose environment (using '$DOCKER_COMPOSE_CMD')."
    fi
  else
    log "Environment detection failed, attempting manual cleanup only."
    DEPLOY_ENV="manual"
  fi

  if [ "$DEPLOY_ENV" == "kubernetes" ]; then
    log "--- Uninstalling Kubernetes Helm releases ---"
    helm uninstall monitoring-stack || true
    helm uninstall llm-operator || true
    helm uninstall nvidia-dcgm-exporter || true
    log "Kubernetes releases uninstalled."

  elif [ "$DEPLOY_ENV" == "local-docker-compose" ]; then
    log "--- Stopping Docker Compose services ---"
    cd "$PROJECT_ROOT_DIR"
    $DOCKER_COMPOSE_CMD down
    log "Docker Compose services stopped."
    
    # Also cleanup manual dcgm-exporter if started
    stop_by_pattern "dcgm-exporter" "dcgm-exporter"
  fi

  # Fallback: Cleanup any orphaned processes
  log "--- Final cleanup of manual processes ---"
  stop_by_pattern "grafana-server" "Grafana"
  stop_by_pattern "prometheus.*--config" "Prometheus"
  stop_by_pattern "gate-service" "gate-service"
  stop_by_pattern "vllm.entrypoints.openai.api_server" "vLLM"

  log "=== All Services Stopped ==="
}

main "$@"
