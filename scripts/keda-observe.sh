#!/usr/bin/env bash
# Sample KEDA/HPA + vLLM state during a load test.
# Usage: bash scripts/keda-observe.sh [iterations] [sleep_seconds] [logfile]
set -uo pipefail
N=${1:-26}; S=${2:-8}; LOG=${3:-/tmp/keda-observe.log}

prom() {
  curl -s -m 5 --get 'http://127.0.0.1:19090/api/v1/query' \
    --data-urlencode "query=$1" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    r = d["data"]["result"]
    print(r[0]["value"][1] if r else "NA")
except Exception:
    print("ERR")'
}

for i in $(seq 1 "$N"); do
  ts=$(date -u +%FT%TZ)
  run=$(prom 'sum(vllm:num_requests_running{service="qwen-service"})')
  waitq=$(prom 'sum(vllm:num_requests_waiting{service="qwen-service"})')
  gpu=$(prom 'max(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})')
  hpa=$(kubectl get hpa keda-hpa-qwen-service-vllm-queue -n default -o jsonpath='{.status.currentReplicas}->{.status.desiredReplicas}' 2>/dev/null || echo '?')
  spec=$(kubectl get inferenceservice qwen-service -n default -o jsonpath='{.spec.replicas}' 2>/dev/null || echo '?')
  line="$ts running=$run waiting=$waitq gpu=$gpu hpa=$hpa spec=$spec"
  echo "$line"
  echo "$line" >> "$LOG"
  sleep "$S"
done
