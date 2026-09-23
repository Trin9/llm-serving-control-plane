#!/usr/bin/env bash
# =============================================================================
# Phase 6 G1 window (P6-A) - cluster bootstrap / drive / close.
#
# Usage:
#   RUN_ID=20260924T010000Z TAG=phase6-abc1234 scripts/phase6-g1-window.sh bootstrap
#   RUN_ID=... TAG=... scripts/phase6-g1-window.sh drive
#   RUN_ID=... TAG=... scripts/phase6-g1-window.sh close
#
# bootstrap: verify cluster+nodes, apply CRD, helm upgrade (phase6 images,
#            ROUTER_STRATEGY=prefix-hash), drop the KEDA ScaledObject that owns
#            qwen-service replicas, deploy the 2-replica A/B service, wire the
#            loadgen JWT, capture metric-availability proof.
# drive:     run scripts/phase6-ab-driver.sh (needs the Prometheus port-forward).
# close:     analyze, collect evidence, delete the workload, scale GPU pool to 0,
#            stop the cluster, kill port-forwards, verify powerState.
#
# Prereqs for drive: `kubectl port-forward -n monitoring svc/monitoring-stack-kube-prom-prometheus 19090:9090`
# running in its own terminal.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID required}"
TAG="${TAG:?TAG required}"
ACR="${ACR:-llmphase5eaacr.azurecr.io}"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase6-azure}"
ART_DIR="$ART_ROOT/$RUN_ID"
RG="${RG:-llm-phase5-ea}"
AKS="${AKS:-llm-aks-ea}"
CTX="${CTX:-llm-ea-admin-admin}"
PROM_URL="${PROM_URL:-http://127.0.0.1:19090}"
NS=default

mkdir -p "$ART_DIR"
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

bootstrap() {
  kubectl config use-context "$CTX" >/dev/null
  log "waiting for 2 ready gputest nodes..."
  for _ in $(seq 1 90); do
    local ready
    ready="$(kubectl get nodes -l kubernetes.azure.com/agentpool=gputest --no-headers 2>/dev/null | grep -c ' Ready' || true)"
    [[ "$ready" -ge 2 ]] && break
    sleep 20
  done
  kubectl get nodes -L kubernetes.azure.com/agentpool -o wide | tee "$ART_DIR/G1-nodes.txt"
  kubectl get nodes -l kubernetes.azure.com/agentpool=gputest --no-headers | grep -c ' Ready' | tee "$ART_DIR/G1-gputest-ready-count.txt"

  log "applying CRD (extraArgs + gpu-t4-7b enum; helm never upgrades CRDs)"
  kubectl apply -f "$REPO_ROOT/helm/llm-operator/crds/crd.yaml" | tee "$ART_DIR/G1-crd-apply.txt"

  log "helm upgrade llm-operator ($TAG, ROUTER_STRATEGY=prefix-hash)"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$TAG" \
    --set gate-service.env.ROUTER_STRATEGY=prefix-hash \
    --wait --timeout 10m | tee "$ART_DIR/G1-helm-upgrade.txt"
  kubectl get deploy -n "$NS" -o wide | tee "$ART_DIR/G1-deployments.txt"

  log "dropping KEDA ScaledObject that owns qwen-service replicas (phase 5 E5 finding)"
  kubectl delete scaledobject qwen-service-vllm-queue -n "$NS" --ignore-not-found | tee "$ART_DIR/G1-scaledobject-delete.txt"

  log "deploying the 2-replica A/B InferenceService"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" | tee "$ART_DIR/G1-isvc-apply.txt"
  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service \
    -n "$NS" --timeout=1200s | tee "$ART_DIR/G1-pods-ready.txt"
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service -o wide | tee "$ART_DIR/G1-pool-pods.txt"

  log "applying vLLM ServiceMonitor"
  kubectl apply -f "$REPO_ROOT/test/vllm-servicemonitor.yaml" | tee "$ART_DIR/G1-servicemonitor-apply.txt"

  log "minting loadgen JWT (secret loadgen-token)"
  local token
  token="$(JWT_SECRET="${JWT_SECRET:-aks-cpu-smoke-jwt-secret}" "$REPO_ROOT/scripts/mint-jwt.sh" p6-ab-user 28800)"
  kubectl create secret generic loadgen-token -n "$NS" --from-literal=TOKEN="$token" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/G1-loadgen-secret.txt"

  log "capturing generated Deployment args (--enable-prefix-caching proof)"
  kubectl get deploy qwen-service -n "$NS" -o yaml > "$ART_DIR/G1-qwen-deployment.yaml"
  kubectl get deploy qwen-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].args}' | tee "$ART_DIR/G1-qwen-args.txt"; echo

  log "capturing vLLM metric availability (prefix cache / prompt tokens)"
  kubectl exec deploy/qwen-service -n "$NS" -- python3 -c \
    "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8000/metrics').read().decode())" \
    2>/dev/null | grep -E 'prefix_cache|prompt_tokens|kv_cache_usage|time_to_first_token|request_prefill' \
    | tee "$ART_DIR/G1-vllm-metric-names.txt" || true

  log "capturing vLLM startup log (prefix caching / args echo)"
  kubectl logs deploy/qwen-service -n "$NS" --tail=400 2>/dev/null \
    | grep -iE 'prefix cach|enable.prefix|Loading model|Serving|API server|torch.compile' \
    | tee "$ART_DIR/G1-vllm-startup-grep.txt" || true

  log "bootstrap done. Start the Prometheus port-forward, then run: $0 drive"
}

drive() {
  if ! curl -fsS "$PROM_URL/-/ready" >/dev/null 2>&1; then
    log "FATAL: Prometheus not reachable at $PROM_URL - start the port-forward first"
    exit 1
  fi
  RUN_ID="$RUN_ID" TAG="$TAG" ART_ROOT="$ART_ROOT" PROM_URL="$PROM_URL" \
    "$REPO_ROOT/scripts/phase6-ab-driver.sh"
}

close() {
  log "analyzing A/B artifacts"
  python3 "$REPO_ROOT/scripts/phase6-ab-analyze.py" "$ART_DIR/G1-prefix-ab" \
    | tee "$ART_DIR/G1-prefix-ab/summary.md.out" || true

  log "collecting platform evidence bundle"
  EVID_QUERIES="$(printf '%s\n' \
    'sum by (pod) (vllm:request_success_total{service="qwen-service"})' \
    'sum by (pod) (vllm:prompt_tokens_total{service="qwen-service"})' \
    'sum by (pod) (vllm:prefix_cache_hits_total{service="qwen-service"})' \
    'sum by (pod) (vllm:prefix_cache_queries_total{service="qwen-service"})' \
    'histogram_quantile(0.5, sum by (le) (rate(vllm:time_to_first_token_seconds_bucket{service="qwen-service"}[5m])))' \
    'histogram_quantile(0.95, sum by (le) (rate(vllm:time_to_first_token_seconds_bucket{service="qwen-service"}[5m])))' \
    'sum by (pod) (vllm:kv_cache_usage_perc{service="qwen-service"})' \
    'DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"}' \
    'sum by (code) (http_requests_total{service="llm-operator-gate-service"})')" \
    ART_ROOT="$ART_ROOT" RUN_ID="$RUN_ID" PROM_URL="$PROM_URL" \
    "$REPO_ROOT/scripts/collect-evidence.sh" all

  log "deleting A/B workload and stopping the GPU pool"
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found || true
  az aks nodepool scale -g "$RG" --cluster-name "$AKS" -n gputest --node-count 0 -o none
  az aks stop -g "$RG" -n "$AKS" -o none
  pkill -f 'kubectl port-forward' || true
  az aks show -g "$RG" -n "$AKS" --query powerState.code -o tsv | tee "$ART_DIR/G1-powerstate.txt"
  az aks nodepool show -g "$RG" --cluster-name "$AKS" -n gputest --query 'count' -o tsv | tee "$ART_DIR/G1-gputest-count.txt"
  log "G1 window closed"
}

case "${1:-}" in
  bootstrap) bootstrap ;;
  drive) drive ;;
  close) close ;;
  *) echo "usage: $0 bootstrap|drive|close"; exit 1 ;;
esac
