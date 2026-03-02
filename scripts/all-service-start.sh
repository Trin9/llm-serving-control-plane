#!/bin/bash
# =============================================================================
# LLM Serving 全量服务一键启动脚本
# 前置条件：vLLM、gate-service、dcgm-exporter、Prometheus、Grafana 已安装
# 参考：process/phase1-process.md
# =============================================================================

set -e

# ---- 可配置变量（按实际环境修改） ----
VLLM_MODEL="${VLLM_MODEL:-/root/autodl-tmp/qwen/Qwen1.5-4B-Chat}"
VLLM_SERVED_NAME="${VLLM_SERVED_NAME:-Qwen1.5-4B-Chat}"
VLLM_MAX_MODEL_LEN="${VLLM_MAX_MODEL_LEN:-2048}"

GATE_SERVICE_DIR="${GATE_SERVICE_DIR:-$HOME/autodl-tmp/llm-serving-control-plane}"

VLLM_URL="${VLLM_URL:-http://localhost:8000/v1/chat/completions}"

LOG_DIR="${LOG_DIR:-/tmp}"

# ---- 辅助函数 ----
log() { echo "[$(date +%H:%M:%S)] $*"; }
err() { echo "[$(date +%H:%M:%S)] ERROR: $*" >&2; }

# 检查端口是否已被占用（有进程监听则跳过启动）
port_in_use() {
  local port=$1
  if command -v ss &>/dev/null; then
    ait_for_url() {
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

  # 1. 启动 vLLM
  log "--- [1/5] vLLM ---"
  if port_in_use 8000; then
    log "vLLM already running (port 8000 in use), skip"
  else
    if [ ! -d "$VLLM_MODEL" ]; then
      err "vLLM model not found: $VLLM_MODEL"
      exit 1
    fi
    nohup python -m vllm.entrypoints.openai.api_server \
      --model "$VLLM_MODEL" \
      --served-model-name "$VLLM_SERVED_NAME" \
      --max-model-len "$VLLM_MAX_MODEL_LEN" \
      > "$LOG_DIR/vllm.log" 2>&1 &
    log "vLLM started, PID=$!, log: $LOG_DIR/vllm.log"
    wait_for_url "http://localhost:8000/v1/models" "vLLM" 120 || i

  # 2. 启动 gate-service
  log "--- [2/5] gate-service ---"
  if port_in_use 8080; then
    log "gate-service already running (port 8080 in use), skip"
  else
    if [ ! -d "$GATE_SERVICE_DIR" ]; then
      err "gate-service dir not found: $GATE_SERVICE_DIR"
      exit 1
    fi
    cd "$GATE_SERVICE_DIR"
    go build -o gate-service ./app/cmd/main.go 2>/dev/null || true
    if [ ! -f ./gate-service ]; then
      err "gate-service binary not found, build failed"
      exit 1
    fi
    nohup env VLLM_URL="$VLLM_URL" ./gate-service \
      > "$LOG_DIR/gate-service.log" 2>&1 &
    log "gate-service started, PID=$!, log: $LOG_DIR/gate-service.log"
    wait_for_url "http://localhost:8080/health" "gate-service" 30 || exit 1
  fi

  # 3. 启动 dcgm-exporter
  log "--- [3/5] dcgm-exporter ---"
  if port_in_use 9400; then
    log "dcgm-exporter already running (port 9400 in use), skip"
  else
    if ! command -v dcgm-exporter &>/dev/null; then
      err "dcgm-exporter not found in PATH, skip"
    else
      no-exporter > "$LOG_DIR/dcgm-exporter.log" 2>&1 &
      log "dcgm-exporter started, PID=$!, log: $LOG_DIR/dcgm-exporter.log"
      sleep 2
    fi
  fi

  # 4. 启动 Prometheus
  log "--- [4/5] Prometheus ---"
  if port_in_use 9090; then
    log "Prometheus already running (port 9090 in use), skip"
  else
    if ! command -v prometheus &>/dev/null; then
      err "prometheus not found in PATH, skip"
    else
      nohup prometheus \
        --config.file=/etc/prometheus/prometheus.yml \
        --storage.tsdb.path=/var/lib/prometheus/ \
        --web.listen-address=:9090 \
        --web.enable-lifecycle \
        > "$LOG_DIR/prometheus.log" 2>&1 &
      log "Prometheus started, PID=$!, log: $LOG_DIR/prometheus.log"
      wait_for_url "http://localhost:9090/-/healthy" "Prometheus" 30 || true
    fi
  fi

  # 5. 启动 Grafana
  log "--- [5/5] Grafana ---"
  if port_in_use 3000; then
    log "Grafana already running (port 3000 in use), skip"
  else
    if [ ! -x /usr/sbin/grafana-server ]; then
      err "grafar not found at /usr/sbin/grafana-server, skip"
    else
      nohup /usr/sbin/grafana-server \
        --config=/etc/grafana/grafana.ini \
        --homepath=/usr/share/grafana \
        cfg:default.paths.data=/var/lib/grafana \
        cfg:default.paths.logs=/var/log/grafana \
        > "$LOG_DIR/grafana.log" 2>&1 &
      log "Grafana started, PID=$!, log: $LOG_DIR/grafana.log"
      sleep 3
    fi
  fi

  log "=== All Services Started ==="
  echo ""
  echo "  vLLM:        http://localhost:8000"
  echo "  gate-service: http://localhost:8080  (metrics: /metrics)"
  echo "  dcgm-exporter: http://localhost:9400/metrics"
  echo "  Prometheus:   http://localhost:9090"
  echo "  Grafana:      http://localhost:3000   (admin/admin)"
  echo ""
  echo "  Logs: $LOG_DIR/{vllm,gate-service,dcgm-exporter,prometheus,grafana}.log"
  echo ""
  echo "  SSH port forward: ssh -L 9090:localhost:9090 -L 3000:localhost:3000 -p PORT user@host"
  echo ""
}

main "$@"

