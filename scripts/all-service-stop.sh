#!/bin/bash
# =============================================================================
# LLM Serving 全量服务一键停止脚本
# 按启动顺序反向停止（Grafana -> Prometheus -> dcgm-exporter -> gate-service -> vLLM）
# =============================================================================

log() { echo "[$(date +%H:%M:%S)] $*"; }

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
    log "$name not running"
  fi
}

main() {
  log "=== LLM Serving All Services Stop ==="

  stop_by_pattern "grafana-server" "Grafana"
  stop_by_pattern "prometheus.*--config" "Prometheus"
  stop_by_pattern "dcgm-exporter" "dcgm-exporter"
  stop_by_pattern "gate-service" "gate-service"
  stop_by_pattern "vllm.entrypoints.openai.api_server" "vLLM"

  log "=== Done ==="
}

main "$@"

