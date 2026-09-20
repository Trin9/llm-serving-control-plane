#!/usr/bin/env bash
# Export Prometheus query_range JSON evidence for a time window.
# Usage: closeout-metric-export.sh <outdir> <start RFC3339> <end RFC3339> [step]
# Requires a reachable Prometheus (PROM env, default local port-forward 19090).
set -uo pipefail
OUT=$1; START=$2; END=$3; STEP=${4:-15}
PROM=${PROM:-http://127.0.0.1:19090}
mkdir -p "$OUT"

# Ensure a working Prometheus endpoint; self-heal with a temporary port-forward.
if ! curl -sS -m 2 -o /dev/null "$PROM/-/ready" 2>/dev/null; then
  pkill -f 'port-forward.*19090' 2>/dev/null; sleep 1
  echo "starting temporary port-forward for export..."
  kubectl port-forward -n monitoring svc/monitoring-stack-kube-prom-prometheus 19090:9090 >/tmp/pf-export.log 2>&1 &
  PF_PID=$!
  trap 'kill $PF_PID 2>/dev/null' EXIT
  for _ in $(seq 1 12); do
    sleep 1
    curl -sS -m 2 -o /dev/null "$PROM/-/ready" 2>/dev/null && break
  done
fi

iq() { # name expr
  local name=$1 expr=$2
  curl -sS -G "$PROM/api/v1/query_range" \
    --data-urlencode "query=$expr" \
    --data-urlencode "start=$START" \
    --data-urlencode "end=$END" \
    --data-urlencode "step=${STEP}s" > "$OUT/${name}.json"
  local series
  series=$(grep -o '"metric"' "$OUT/${name}.json" | wc -l)
  echo "  $name: $(wc -c < "$OUT/${name}.json") bytes, ~$series series"
}

echo "== export window ${START} .. ${END} (step ${STEP}s) -> $OUT"
iq vllm_running     'sum(vllm:num_requests_running{service="qwen-service"})'
iq vllm_waiting     'sum(vllm:num_requests_waiting{service="qwen-service"})'
iq vllm_cache       'avg(vllm:gpu_cache_usage_perc{service="qwen-service"})'
iq hpa_replicas     'kube_horizontalpodautoscaler_status_current_replicas{horizontalpodautoscaler="keda-hpa-qwen-service-vllm-queue"}'
iq deploy_ready     'kube_deployment_status_replicas_ready{namespace="default",deployment="qwen-service"}'
iq pod_restarts     'sum(kube_pod_container_status_restarts_total{namespace="default",pod=~"qwen-service.*"})'
iq pod_phase        'kube_pod_status_phase{namespace="default",pod=~"qwen-service.*"}'
iq target_up        'up{job="qwen-service"}'
iq gpu_util         'max(DCGM_FI_DEV_GPU_UTIL)'
echo "== done"
