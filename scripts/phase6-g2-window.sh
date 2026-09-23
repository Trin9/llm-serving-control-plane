#!/usr/bin/env bash
# =============================================================================
# Phase 6 G2 window (P6-B, 7B AWQ scale-up) - bootstrap / drive / close.
#
# Usage:
#   RUN_ID=... TAG=phase6-abc1234 scripts/phase6-g2-window.sh bootstrap
#   RUN_ID=... TAG=... scripts/phase6-g2-window.sh drive [sweep|keda|coldstart]
#   RUN_ID=... TAG=... scripts/phase6-g2-window.sh close
#
# Prereqs: Prometheus port-forward on 19090; gateway port-forward on 18080 for
# the drive steps; `hey` installed locally.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID required}"
TAG="${TAG:?TAG required}"
ACR="${ACR:-llmphase5eaacr.azurecr.io}"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase6-azure}"
ART_DIR="$ART_ROOT/$RUN_ID/G2-7b"
RG="${RG:-llm-phase5-ea}"
AKS="${AKS:-llm-aks-ea}"
CTX="${CTX:-llm-ea-admin-admin}"
PROM_URL="${PROM_URL:-http://127.0.0.1:19090}"
GATE_URL="${GATE_URL:-http://127.0.0.1:18080/v1/chat/completions}"
NS=default
MODEL7B="Qwen/Qwen2.5-7B-Instruct-AWQ"

mkdir -p "$ART_DIR"
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

prom() {
  curl -s -m 5 --get "$PROM_URL/api/v1/query" --data-urlencode "query=$1" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin); r = d["data"]["result"]
    print(r[0]["value"][1] if r else "NA")
except Exception:
    print("ERR")'
}

bootstrap() {
  kubectl config use-context "$CTX" >/dev/null
  kubectl get nodes -L kubernetes.azure.com/agentpool -o wide | tee "$ART_DIR/nodes.txt"
  kubectl get nodes -l kubernetes.azure.com/agentpool=gputest --no-headers | grep -c ' Ready' \
    | tee "$ART_DIR/gputest-ready-count.txt"
  az aks nodepool show -g "$RG" --cluster-name "$AKS" -n gputest \
    --query '{count:count,vmSize:vmSize,priority:scaleSetPriority,spotMaxPrice:spotMaxPrice}' \
    -o json | tee "$ART_DIR/nodepool-info.json"

  kubectl apply -f "$REPO_ROOT/helm/llm-operator/crds/crd.yaml" | tee "$ART_DIR/crd-apply.txt"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$TAG" \
    --set gate-service.env.ROUTER_STRATEGY=prefix-hash \
    --wait --timeout 10m | tee "$ART_DIR/helm-upgrade.txt"

  log "deploying 7B AWQ InferenceService (first run downloads ~4.4GB of weights)"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" | tee "$ART_DIR/isvc-apply.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/vllm-servicemonitor-qwen7b.yaml" | tee "$ART_DIR/servicemonitor-apply.txt"

  # 0.5B sibling service for the cold-start comparison (P6-B comparison table).
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" | tee "$ART_DIR/isvc05-apply.txt"

  log "waiting for qwen7b-service pod (30 min budget: download + load)"
  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen7b-service \
    -n "$NS" --timeout=1800s | tee "$ART_DIR/qwen7b-ready.txt"
  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service \
    -n "$NS" --timeout=600s | tee "$ART_DIR/qwen05-ready.txt"
  kubectl get pods -n "$NS" -o wide | grep -E 'qwen' | tee "$ART_DIR/pods.txt"

  kubectl get deploy qwen7b-service -n "$NS" -o yaml > "$ART_DIR/qwen7b-deployment.yaml"
  kubectl get deploy qwen7b-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].args}' | tee "$ART_DIR/qwen7b-args.txt"; echo
  kubectl logs deploy/qwen7b-service -n "$NS" --tail=100000 > "$ART_DIR/qwen7b-startup.log" 2>&1 || true

  # Refresh the loadgen JWT (gateway auth is shared by both models).
  local token
  token="$(JWT_SECRET="${JWT_SECRET:-aks-cpu-smoke-jwt-secret}" "$REPO_ROOT/scripts/mint-jwt.sh" p6-ab-user 28800)"
  kubectl create secret generic loadgen-token -n "$NS" --from-literal=TOKEN="$token" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/loadgen-secret.txt"
  log "bootstrap done"
}

smoke() {
  local token rid
  token="$(kubectl get secret loadgen-token -n "$NS" -o jsonpath='{.data.TOKEN}' | base64 -d)"
  log "smoke request through the gateway -> $MODEL7B"
  curl -sS -D "$ART_DIR/smoke-headers.txt" -o "$ART_DIR/smoke-body.txt" \
    -H "Content-Type: application/json" -H "Authorization: Bearer $token" \
    -d "{\"model\":\"$MODEL7B\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in exactly five words.\"}],\"max_tokens\":32}" \
    "$GATE_URL"
  rid="$(grep -i '^x-request-id:' "$ART_DIR/smoke-headers.txt" | tr -d '\r' | awk '{print $2}')"
  echo "request-id=$rid" | tee "$ART_DIR/smoke-request-id.txt"

  log "billing ledger check for $rid"
  local pass
  pass="$(kubectl get secret llm-operator-gate-env -n "$NS" -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d)"
  {
    echo "== usage:req:$rid"
    kubectl exec deploy/llm-operator-redis -n "$NS" -- redis-cli -a "$pass" --no-auth-warning hgetall "usage:req:$rid"
    echo "== usage:ledger:$rid"
    kubectl exec deploy/llm-operator-redis -n "$NS" -- redis-cli -a "$pass" --no-auth-warning hgetall "usage:ledger:$rid"
  } | tee "$ART_DIR/smoke-ledger.txt" || true
}

coldstart() { # <service> <tag>
  local svc="$1" tag="$2"
  log "cold start timing for $svc"
  kubectl delete pod -l "serving.trin.io/inferenceservice=$svc" -n "$NS" --wait=true >> "$ART_DIR/coldstart-$tag.log" 2>&1 || true
  # wait for the replacement pod, then time to Ready
  local pod t0 t1
  for _ in $(seq 1 60); do
    pod="$(kubectl get pods -n "$NS" -l "serving.trin.io/inferenceservice=$svc" \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    [[ -n "$pod" ]] && break
    sleep 2
  done
  t0="$(date -u +%s)"
  echo "start=$t0 pod=$pod" >> "$ART_DIR/coldstart-$tag.log"
  kubectl wait --for=condition=ready "pod/$pod" -n "$NS" --timeout=1800s >> "$ART_DIR/coldstart-$tag.log" 2>&1
  t1="$(date -u +%s)"
  echo "ready_after=$((t1 - t0))s" | tee -a "$ART_DIR/coldstart-$tag.log"
  kubectl get pod "$pod" -n "$NS" -o yaml > "$ART_DIR/coldstart-$tag.pod.yaml" 2>&1 || true
  kubectl logs "pod/$pod" -n "$NS" > "$ART_DIR/coldstart-$tag.container.log" 2>&1 || true
}

sweep() { # capacity sweep 8/16/32/64 concurrency
  local token
  token="$(kubectl get secret loadgen-token -n "$NS" -o jsonpath='{.data.TOKEN}' | base64 -d)"
  local sweep_cs="${SWEEP_C:-8 16 32 64}"
  local c
  for c in $sweep_cs; do
    local out="$ART_DIR/sweep-c${c}.txt"
    log "capacity sweep c=$c"
    (
      for _ in $(seq 1 200); do
        echo "$(date -u +%FT%TZ) running=$(prom 'sum(vllm:num_requests_running{service="qwen7b-service"})') waiting=$(prom 'sum(vllm:num_requests_waiting{service="qwen7b-service"})') kv=$(prom 'max(vllm:kv_cache_usage_perc{service="qwen7b-service"})') gpu=$(prom 'max(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})')"
        sleep 5
      done
    ) > "$ART_DIR/sweep-c${c}-samples.txt" 2>&1 &
    local sampler=$!
    TOKEN="$token" GATE_URL="$GATE_URL" "$REPO_ROOT/scripts/run-stress-test.sh" \
      -c "$c" -n $((c * 4)) -b "$REPO_ROOT/test/stress-test-body-qwen7b.json" > "$out" 2>&1 || true
    kill "$sampler" 2>/dev/null || true
    tail -25 "$out"
  done
}

keda() {
  log "applying 7B KEDA ScaledObject"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" | tee "$ART_DIR/keda-apply.txt"
  kubectl get scaledobject qwen7b-service-vllm-queue -n "$NS" -o yaml > "$ART_DIR/keda-scaledobject.yaml" || true
  local token
  token="$(kubectl get secret loadgen-token -n "$NS" -o jsonpath='{.data.TOKEN}' | base64 -d)"
  log "driving load to push running > threshold (concurrency ${KEDA_CONC:-128})"
  (
    for _ in $(seq 1 160); do
      echo "$(date -u +%FT%TZ) running=$(prom 'sum(vllm:num_requests_running{service="qwen7b-service"})') waiting=$(prom 'sum(vllm:num_requests_waiting{service="qwen7b-service"})') hpa=$(kubectl get hpa -n $NS -o name 2>/dev/null | grep qwen7b | head -1) spec=$(kubectl get inferenceservice qwen7b-service -n $NS -o jsonpath='{.spec.replicas}' 2>/dev/null) cur=$(kubectl get deployment qwen7b-service -n $NS -o jsonpath='{.status.replicas}' 2>/dev/null)"
      sleep 5
    done
  ) > "$ART_DIR/keda-observe.txt" 2>&1 &
  local sampler=$!
  TOKEN="$token" GATE_URL="$GATE_URL" "$REPO_ROOT/scripts/run-stress-test.sh" \
    -c "${KEDA_CONC:-128}" -n $(( ${KEDA_CONC:-128} * 4 )) -b "$REPO_ROOT/test/stress-test-body-qwen7b.json" \
    > "$ART_DIR/keda-load.txt" 2>&1 || true
  kill "$sampler" 2>/dev/null || true
  kubectl get hpa -n "$NS" > "$ART_DIR/keda-hpa.txt" 2>&1 || true
  kubectl get events -n "$NS" --sort-by=.lastTimestamp | grep -iE 'hpa|keda|qwen7b' | tail -40 > "$ART_DIR/keda-events.txt" 2>&1 || true
  kubectl get deployment qwen7b-service -n "$NS" -o wide | tee "$ART_DIR/keda-deployment.txt"
}

close() {
  EVID_QUERIES="$(printf '%s\n' \
    'sum by (pod) (vllm:request_success_total{service="qwen7b-service"})' \
    'sum by (pod) (vllm:prompt_tokens_total{service="qwen7b-service"})' \
    'sum by (pod) (vllm:generation_tokens_total{service="qwen7b-service"})' \
    'histogram_quantile(0.5, sum by (le) (rate(vllm:time_to_first_token_seconds_bucket{service="qwen7b-service"}[5m])))' \
    'histogram_quantile(0.95, sum by (le) (rate(vllm:time_to_first_token_seconds_bucket{service="qwen7b-service"}[5m])))' \
    'sum by (pod) (vllm:kv_cache_usage_perc{service="qwen7b-service"})' \
    'DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"}' \
    'DCGM_FI_DEV_FB_USED{job=~".*dcgm-exporter.*"}')" \
    ART_ROOT="$ART_ROOT" RUN_ID="$RUN_ID" PROM_URL="$PROM_URL" \
    "$REPO_ROOT/scripts/collect-evidence.sh" all

  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" --ignore-not-found || true
  az aks nodepool scale -g "$RG" --cluster-name "$AKS" -n gputest --node-count 0 -o none
  az aks stop -g "$RG" -n "$AKS" -o none
  pkill -f 'kubectl port-forward' || true
  az aks show -g "$RG" -n "$AKS" --query powerState.code -o tsv | tee "$ART_DIR/powerstate.txt"
  log "G2 window closed"
}

case "${1:-}:${2:-all}" in
  bootstrap:*) bootstrap ;;
  drive:smoke) smoke ;;
  drive:coldstart)
    coldstart qwen7b-service 7b
    coldstart qwen-service 05b
    ;;
  drive:sweep) sweep ;;
  drive:keda) keda ;;
  drive:all) smoke; sweep; keda ;;
  close:*) close ;;
  *) echo "usage: $0 bootstrap|drive [smoke|coldstart|sweep|keda|all]|close"; exit 1 ;;
esac
