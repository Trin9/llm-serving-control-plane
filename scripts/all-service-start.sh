#!/bin/bash
# =============================================================================
# LLM Serving 全量服务一键启动脚本
# 根据环境（Kubernetes 或 Local Docker Compose）启动相应服务
# =============================================================================

set -e

# ---- 可配置变量（按实际环境修改） ----
# These are primarily for local non-K8s deployments or if Helm values need overrides
VLLM_MODEL="${VLLM_MODEL:-/root/autodl-tmp/qwen/Qwen1.5-4B-Chat}"
VLLM_SERVED_NAME="${VLLM_SERVED_NAME:-Qwen1.5-4B-Chat}"
VLLM_MAX_MODEL_LEN="${VLLM_MAX_MODEL_LEN:-2048}"

# Directory for the main project, used for Docker Compose context
PROJECT_ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

LOG_DIR="${LOG_DIR:-/tmp}" # For manually started processes like dcgm-exporter

# ---- 辅助函数 ----
log() { echo "[$(date +%H:%M:%S)] $*"; }
err() { echo "[$(date +%H:%M:%S)] ERROR: $*" >&2; exit 1; }

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

# Check if a port is in use (only for local non-K8s processes)
port_in_use() {
  local port=$1
  if command -v ss &>/dev/null; then
    ss -tuln | grep -q ":$port "
  else # Fallback for systems without ss (e.g., older BusyBox or very minimal containers)
    netstat -tuln | grep -q ":$port "
  fi
}

# Wait for a URL to be ready (only for local non-K8s processes)
wait_for_url() {
  local url=$1
  local name=$2
  local max_sec=${3:-60}
  local elapsed=0

  while [ $elapsed -lt $max_sec ]; do
    if curl -sf --connect-timeout 2 "$url" >/dev/null 2>&1; then
      log "$name ready (${elapsed}s)"
      return 0
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
  err "$name failed to become ready in ${max_sec}s"
  return 1
}

# ---- 主流程 ----
main() {
  log "=== LLM Serving All Services Start ==="

  # Determine deployment environment: Kubernetes (Kind/EKS) or Local Docker Compose
  # Prioritize KUBECONFIG for Kubernetes detection
  if command -v kubectl &>/dev/null && [ -n "$KUBECONFIG" ]; then
    DEPLOY_ENV="kubernetes"
    log "Detected Kubernetes environment (kubectl and KUBECONFIG set)."
  elif [ -f "$PROJECT_ROOT_DIR/docker-compose.yml" ]; then
    DOCKER_COMPOSE_CMD=$(get_docker_compose)
    if [ -z "$DOCKER_COMPOSE_CMD" ]; then
        err "docker-compose.yml found, but neither 'docker-compose' nor 'docker compose' command is available."
    fi
    DEPLOY_ENV="local-docker-compose"
    log "Detected local Docker Compose environment (using '$DOCKER_COMPOSE_CMD')."
  else
    err "Neither Kubernetes (kubectl/KUBECONFIG) nor Docker Compose found or configured."
  fi

  if [ "$DEPLOY_ENV" == "kubernetes" ]; then
    log "--- Deploying services to Kubernetes via Helm ---"

    # 1. Deploy/Upgrade llm-operator (which includes gate-service)
    log "Deploying llm-operator Helm chart..."
    helm upgrade --install llm-operator "$PROJECT_ROOT_DIR/helm/llm-operator" --wait || err "Failed to deploy llm-operator Helm chart."
    log "llm-operator (and gate-service) deployed."

    # 2. Deploy/Upgrade monitoring-stack (Prometheus, Grafana)
    log "Deploying monitoring-stack Helm chart (Prometheus, Grafana)..."
    helm upgrade --install monitoring-stack "$PROJECT_ROOT_DIR/helm/monitoring-stack" --wait || err "Failed to deploy monitoring-stack Helm chart."
    log "Monitoring stack (Prometheus, Grafana) deployed."

    # 3. Deploy nvidia-dcgm-exporter
    # NOTE: The NVIDIA DCGM Exporter often comes as a separate Helm chart.
    # You might need to add the NVIDIA Helm repo first:
    # helm repo add nvdp https://nvidia.github.io/helm-charts
    # helm repo update
    log "Deploying NVIDIA DCGM Exporter Helm chart..."
    helm upgrade --install nvidia-dcgm-exporter nvdp/dcgm-exporter --repo https://nvidia.github.io/helm-charts --wait || 
    log "Warning: Failed to deploy nvidia-dcgm-exporter via Helm. Manual installation or chart adjustment might be needed."
    
    log "--- Kubernetes Deployment Complete ---"
    echo ""
    echo "  Kubernetes services are typically accessed via Ingress, NodePort, or kubectl port-forward."
    echo "  To access Grafana: kubectl port-forward service/monitoring-stack-grafana 3000:80"
    echo "  To access Prometheus: kubectl port-forward service/monitoring-stack-prometheus-server 9090:9090"
    echo "  (Use the correct service names as per Helm deployment, e.g., <release-name>-grafana)"
    echo ""

  elif [ "$DEPLOY_ENV" == "local-docker-compose" ]; then
    log "--- Starting services via Docker Compose ---"
    
    # Start all services with Docker Compose
    log "Bringing up Docker Compose services defined in $PROJECT_ROOT_DIR/docker-compose.yml using '$DOCKER_COMPOSE_CMD'..."
    cd "$PROJECT_ROOT_DIR"
    $DOCKER_COMPOSE_CMD up -d || err "Failed to bring up Docker Compose services."
    log "Docker Compose services started."

    # For local GPU development, dcgm-exporter might still be a manual process
    log "--- [Optional: Manual DCGM Exporter for Local GPU, if not in Docker Compose] ---"
    if ! port_in_use 9400; then
      if command -v dcgm-exporter &>/dev/null; then
        log "Attempting to start dcgm-exporter manually..."
        nohup dcgm-exporter > "$LOG_DIR/dcgm-exporter.log" 2>&1 &
        log "dcgm-exporter started, PID=$!, log: $LOG_DIR/dcgm-exporter.log"
        sleep 2 # Give it a moment to start
      else
        log "dcgm-exporter not found in PATH. GPU metrics will not be available locally without manual installation."
      fi
    else
      log "dcgm-exporter already running (port 9400 in use), skip manual start."
    fi

    log "=== All Local Services Started ==="
    echo ""
    echo "  vLLM (mock):   http://localhost:8000 (via Docker Compose)"
    echo "  gate-service:  http://localhost:8080 (metrics: /metrics, via Docker Compose)"
    echo "  Prometheus:    http://localhost:9090 (via Docker Compose)"
    echo "  Grafana:       http://localhost:3000 (admin/admin, via Docker Compose)"
    echo ""
    echo "  Logs: Check 'docker-compose -f $PROJECT_ROOT_DIR/docker-compose.yml logs -f' or $LOG_DIR/dcgm-exporter.log (if started manually)"
    echo ""
    echo "  Ensure you have run 'helm repo add nvdp https://nvidia.github.io/helm-charts' and 'helm repo update' if deploying DCGM Exporter via Helm in K8s."
    echo ""
  fi

  echo "For more details, check logs in $LOG_DIR or Docker Compose/Kubernetes logs."
}

main "$@"
