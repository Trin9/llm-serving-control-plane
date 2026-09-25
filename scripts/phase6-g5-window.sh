#!/usr/bin/env bash
# =============================================================================
# Phase 6 G5 window (quota 16 vCPU = 4x T4): fixed-replica scaling curve (A),
# full coexistence elasticity (B1/B2/B3), elasticity Pareto (C).
#
# Usage:
#   scripts/phase6-g5-window.sh bootstrap
#   scripts/phase6-g5-window.sh drive-a | drive-b1 | drive-b2 | drive-b3 | drive-c
#   scripts/phase6-g5-window.sh close
#
# Prereqs: cluster started + gputest scaled to 4 + nodes Ready (done outside).
#          prometheus port-forward 19090 + gateway 18080.
# G4 5.10 lessons all baked in: az PATH probe, wait_job 's' suffix + poll,
# python-side non-terminating pod filter, kubectl custom-columns (no awk),
# curl --max-time.
# =============================================================================
set -euo pipefail

if ! command -v az >/dev/null 2>&1; then
  for d in "$HOME/azcli/bin" "/usr/local/bin" "/opt/az/bin" "/snap/bin"; do
    [[ -x "$d/az" ]] && { export PATH="$d:$PATH"; break; }
  done
fi
command -v az >/dev/null 2>&1 || echo "WARN: az CLI not found" >&2

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVFILE="${PHASE6_G5_ENV:-/tmp/phase6-g5.env}"
# shellcheck disable=SC1090
if [[ -z "${RUN_ID:-}" || -z "${RIDP:-}" ]] && [[ -f "$ENVFILE" ]]; then
  source "$ENVFILE"
fi
RUN_ID="${RUN_ID:?RUN_ID required (run bootstrap first)}"
TAG="${TAG:-phase6-ad7cc61}"
RIDP="${RIDP:-p6g5-manual}"
ACR="${ACR:-llmphase5eaacr.azurecr.io}"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase6-azure}"
ART_DIR="$ART_ROOT/$RUN_ID/G5"
RG="${RG:-llm-phase5-ea}"
AKS="${AKS:-llm-aks-ea}"
CTX="${CTX:-llm-ea-admin-admin}"
PROM_URL="${PROM_URL:-http://127.0.0.1:19090}"
GATE_URL="${GATE_URL:-http://127.0.0.1:18080/v1/chat/completions}"
NS=default
MODEL05B="Qwen/Qwen2.5-0.5B-Instruct"
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

get_token() { kubectl get secret loadgen-token -n "$NS" -o jsonpath='{.data.TOKEN}' | base64 -d; }

submit_job() { # name split conc dur maxa maxb label
  local name="$1" split="$2" conc="$3" dur="$4" maxa="$5" maxb="$6" label="$7"
  local deadline=$((dur + 300))
  sed -e "s/__NAME__/$name/g" -e "s/__SPLIT__/$split/g" -e "s/__CONC__/$conc/g" \
      -e "s/__DURATION__/$dur/g" -e "s/__DEADLINE__/$deadline/g" \
      -e "s/__MAXA__/$maxa/g" -e "s/__MAXB__/$maxb/g" \
      -e "s/__RID__/$RIDP/g" -e "s/__LABEL__/$label/g" \
      "$REPO_ROOT/experiments/azure/phase6/mixed-loadgen-job.yaml" | kubectl apply -f -
}

wait_job() { # name timeout_seconds
  kubectl wait --for=condition=complete "job/$1" -n "$NS" --timeout="${2}s" >/dev/null 2>&1 || true
  for _ in $(seq 1 120); do
    local ok
    ok="$(kubectl get job "$1" -n "$NS" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)"
    [[ "$ok" == "1" ]] && break
    sleep 5
  done
}

hpa_snapshot() {
  kubectl get hpa -n "$NS" -o custom-columns=NAME:.metadata.name,CUR:.status.currentReplicas,DES:.status.desiredReplicas,MIN:.spec.minReplicas,MAX:.spec.maxReplicas 2>/dev/null
}

pods_snapshot() {
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice -o wide 2>/dev/null | grep -E "NAME|qwen" || true
}

pending_count() {
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice 2>/dev/null | grep -c Pending || true
}

set_strategy() { # prefix-hash | random
  local s="$1"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$TAG" \
    --set gate-service.env.ROUTER_STRATEGY="$s" \
    --wait --timeout 10m | tail -3
  kubectl rollout status deployment/llm-operator-gate-service -n "$NS" --timeout=300s
  kubectl get deploy llm-operator-gate-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="ROUTER_STRATEGY")].value}' \
    | tee "$ART_DIR/strategy.txt"; echo
}

scale_05b() { # replicas
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas="$1"
  kubectl rollout status deployment/qwen-service -n "$NS" --timeout=900s
}

start_sampler() { # outfile service count
  (
    for _ in $(seq 1 "$3"); do
      echo "$(date -u +%FT%TZ) running=$(prom "sum(vllm:num_requests_running{service=\"$2\"})") waiting=$(prom "sum(vllm:num_requests_waiting{service=\"$2\"})") kv=$(prom "max(vllm:kv_cache_usage_perc{service=\"$2\"})") gpu=$(prom 'max(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})')"
      sleep 5
    done
  ) > "$1" 2>&1 &
  echo $!
}

bootstrap() {
  mkdir -p "$ART_DIR"
  kubectl config use-context "$CTX" >/dev/null

  log "quota + residue check"
  {
    echo "== quota =="
    az vm list-usage -l eastasia -o json 2>/dev/null | python3 -c '
import json,sys
for r in json.load(sys.stdin):
    if "NCASv3_T4" in r["name"]["value"]:
        print(r["name"]["value"], "limit=", r["limit"])
'
    echo "== residues (isvc / scaledobject / jobs) =="
    kubectl get inferenceservices.serving.trin.io -n "$NS" 2>&1 || true
    kubectl get scaledobject -n "$NS" 2>&1 || true
    kubectl get jobs -n "$NS" 2>&1 | grep -E "p6g4-|p6g5-" || echo "(none)"
  } | tee "$ART_DIR/residue-pre.txt"

  kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.allocatable.nvidia\.com/gpu}{"\n"}{end}' | tee "$ART_DIR/nodes-gpu.txt"
  kubectl get nodes -l kubernetes.azure.com/agentpool=gputest --no-headers | grep -c ' Ready' | tee "$ART_DIR/gputest-ready-count.txt"

  kubectl apply -f "$REPO_ROOT/helm/llm-operator/crds/crd.yaml" | tee "$ART_DIR/crd-apply.txt"
  set_strategy prefix-hash | tail -2 | tee "$ART_DIR/helm-upgrade.txt"

  log "clean experiment resources for deterministic start"
  kubectl delete scaledobject qwen-service-vllm-queue qwen7b-service-vllm-queue -n "$NS" --ignore-not-found | tee "$ART_DIR/cleanup.txt"
  kubectl delete job -n "$NS" -l app=phase6-g4-mixed --ignore-not-found >> "$ART_DIR/cleanup.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found >> "$ART_DIR/cleanup.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found >> "$ART_DIR/cleanup.txt" 2>&1 || true

  log "deploy 0.5B (1 replica) + 7B (1 replica)"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" | tee "$ART_DIR/isvc05-apply.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" | tee "$ART_DIR/isvc7b-apply.txt"
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas=1 | tee "$ART_DIR/isvc05-scale1.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/vllm-servicemonitor-qwen7b.yaml" >/dev/null || true
  kubectl apply -f "$REPO_ROOT/test/vllm-servicemonitor.yaml" >/dev/null 2>&1 || true

  kubectl rollout status deployment/qwen-service -n "$NS" --timeout=900s | tee "$ART_DIR/qwen05-ready.txt"
  kubectl rollout status deployment/qwen7b-service -n "$NS" --timeout=1800s | tee "$ART_DIR/qwen7b-ready.txt"
  pods_snapshot | tee "$ART_DIR/pods.txt"

  kubectl create configmap phase6-mixed-src -n "$NS" \
    --from-file=mixed-loadgen.py="$REPO_ROOT/experiments/azure/phase6/mixed-loadgen.py" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/configmap-mixed.txt"
  local token
  token="$(JWT_SECRET="${JWT_SECRET:-aks-cpu-smoke-jwt-secret}" "$REPO_ROOT/scripts/mint-jwt.sh" p6-g5-user 28800)"
  kubectl create secret generic loadgen-token -n "$NS" --from-literal=TOKEN="$token" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/loadgen-secret.txt"

  {
    echo "RUN_ID=$RUN_ID"
    echo "TAG=$TAG"
    echo "RIDP=$RIDP"
    echo "ART_DIR=$ART_DIR"
  } > "$ENVFILE"
  log "bootstrap done (RIDP=$RIDP)"
}

drive_a() {
  local D="$ART_DIR/A"
  mkdir -p "$D"
  log "A: set ROUTER_STRATEGY=random"
  set_strategy random | tail -2 | tee "$D/strategy-random.txt"
  kubectl delete scaledobject qwen-service-vllm-queue -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  local R S
  for R in 1 2 3 4; do
    log "A: replicas=$R (192c, max_tokens=128, dur=120)"
    scale_05b "$R" | tee "$D/r$R-scale.txt"
    pods_snapshot | tee "$D/r$R-pods.txt"
    S="$(start_sampler "$D/r$R-samples.txt" qwen-service 30)"
    submit_job "p6g5-a-r$R" 1:0 192 120 128 512 "a-r$R" | tee "$D/r$R-apply.txt"
    sleep 30
    curl -s -m 10 --get "$PROM_URL/api/v1/query" --data-urlencode \
      'query=sum by (pod) (rate(vllm:request_success_total{service="qwen-service"}[1m]))' \
      > "$D/r$R-pod-rps.json"
    wait_job "p6g5-a-r$R" 600
    kill "$S" 2>/dev/null || true
    kubectl logs "job/p6g5-a-r$R" -n "$NS" > "$D/r$R-load.txt" 2>&1 || true
    grep -h '^SUMMARY' "$D/r$R-load.txt" >> "$D/summaries.txt" 2>/dev/null || true
  done
  log "A: restore prefix-hash + 1 replica"
  scale_05b 1 | tail -1
  set_strategy prefix-hash | tail -2 | tee "$D/strategy-restore.txt"
  cat "$D/summaries.txt" || true
  log "A done"
}

apply_sos() {
  kubectl apply -f "$REPO_ROOT/test/keda-scaledobject.yaml" | tee "$ART_DIR/so-05b-apply.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" | tee "$ART_DIR/so-7b-apply.txt"
}

drive_b1() {
  local D="$ART_DIR/B1"
  mkdir -p "$D"
  log "B1: 7B-only elastic (0.5B idle), 4 nodes => 2nd 7B pod must be Ready"
  apply_sos
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas=1 >/dev/null
  kubectl scale inferenceservice/qwen7b-service -n "$NS" --replicas=1 >/dev/null
  kubectl rollout status deployment/qwen-service deployment/qwen7b-service -n "$NS" --timeout=600s >/dev/null || true
  (
    for _ in $(seq 1 40); do
      { echo "== $(date -u +%FT%TZ) pending=$(pending_count) =="; hpa_snapshot; pods_snapshot; } >> "$D/observe.txt"
      sleep 15
    done
  ) &
  local OBS=$!
  submit_job p6g5-b1 0:1 64 180 64 512 b1 | tee "$D/apply.txt"
  wait_job p6g5-b1 600
  kubectl logs job/p6g5-b1 -n "$NS" > "$D/load.txt" 2>&1 || true
  hpa_snapshot | tee "$D/hpa-mid.txt"
  pods_snapshot | tee "$D/pods-mid.txt"
  kubectl get events -n "$NS" --sort-by=.lastTimestamp 2>/dev/null | grep -iE 'hpa|keda|qwen7b|insufficient|nvidia' | tail -40 > "$D/events.txt" || true
  # bounded wait for cooldown back to 1
  for _ in $(seq 1 72); do
    local cur
    cur="$(kubectl get hpa keda-hpa-qwen7b-service-vllm-queue -n "$NS" -o jsonpath='{.status.currentReplicas}' 2>/dev/null || echo '?')"
    echo "$(date -u +%FT%TZ) hpa7b_cur=$cur" >> "$D/cooldown.txt"
    [[ "$cur" == "1" ]] && break
    sleep 5
  done
  kill "$OBS" 2>/dev/null || true
  hpa_snapshot | tee "$D/hpa-post.txt"
  pods_snapshot | tee "$D/pods-post.txt"
  grep -h '^SUMMARY' "$D/load.txt" | tee "$D/summaries.txt" || true
  log "B1 done"
}

drive_b2() {
  local D="$ART_DIR/B2"
  mkdir -p "$D"
  log "B2: both elastic 1+1 -> 2+2 (192c = 0.5B 128 + 7B 64)"
  apply_sos
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas=1 >/dev/null
  kubectl scale inferenceservice/qwen7b-service -n "$NS" --replicas=1 >/dev/null
  kubectl rollout status deployment/qwen-service deployment/qwen7b-service -n "$NS" --timeout=600s >/dev/null || true
  (
    for _ in $(seq 1 40); do
      { echo "== $(date -u +%FT%TZ) pending=$(pending_count) =="; hpa_snapshot; pods_snapshot; } >> "$D/observe.txt"
      sleep 15
    done
  ) &
  local OBS=$!
  submit_job p6g5-b2 2:1 192 180 256 512 b2 | tee "$D/apply.txt"
  wait_job p6g5-b2 600
  kubectl logs job/p6g5-b2 -n "$NS" > "$D/load.txt" 2>&1 || true
  hpa_snapshot | tee "$D/hpa-mid.txt"
  pods_snapshot | tee "$D/pods-mid.txt"
  kubectl get events -n "$NS" --sort-by=.lastTimestamp 2>/dev/null | grep -iE 'hpa|keda|qwen|insufficient|nvidia' | tail -60 > "$D/events.txt" || true
  for _ in $(seq 1 72); do
    local c05 c7
    c05="$(kubectl get hpa keda-hpa-qwen-service-vllm-queue -n "$NS" -o jsonpath='{.status.currentReplicas}' 2>/dev/null || echo '?')"
    c7="$(kubectl get hpa keda-hpa-qwen7b-service-vllm-queue -n "$NS" -o jsonpath='{.status.currentReplicas}' 2>/dev/null || echo '?')"
    echo "$(date -u +%FT%TZ) hpa05=$c05 hpa7b=$c7" >> "$D/cooldown.txt"
    [[ "$c05" == "1" && "$c7" == "1" ]] && break
    sleep 5
  done
  kill "$OBS" 2>/dev/null || true
  hpa_snapshot | tee "$D/hpa-post.txt"
  pods_snapshot | tee "$D/pods-post.txt"
  grep -h '^SUMMARY' "$D/load.txt" | tee "$D/summaries.txt" || true
  log "B2 done"
}

drive_b3() {
  local D="$ART_DIR/B3"
  mkdir -p "$D"
  log "B3: steady 2+2 mixed (SO removed, fixed replicas; G4 harness)"
  kubectl delete scaledobject qwen-service-vllm-queue qwen7b-service-vllm-queue -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas=2 >/dev/null
  kubectl scale inferenceservice/qwen7b-service -n "$NS" --replicas=2 >/dev/null
  kubectl rollout status deployment/qwen-service deployment/qwen7b-service -n "$NS" --timeout=900s | tee "$D/rollout.txt"
  pods_snapshot | tee "$D/pods.txt"
  local S1 S2
  S1="$(start_sampler "$D/samples-05b.txt" qwen-service 45)"
  S2="$(start_sampler "$D/samples-7b.txt" qwen7b-service 45)"
  submit_job p6g5-b3 2:1 96 180 64 512 b3 | tee "$D/apply.txt"
  wait_job p6g5-b3 900
  kill "$S1" "$S2" 2>/dev/null || true
  kubectl logs job/p6g5-b3 -n "$NS" > "$D/load.txt" 2>&1 || true
  grep -h '^SUMMARY' "$D/load.txt" | tee "$D/summaries.txt" || true
  log "B3 done"
}

drive_c() {
  local D="$ART_DIR/C"
  mkdir -p "$D"
  log "C: burst SLO Pareto min=1/2/4 (192c, max_tokens=64, dur=180). 7B removed to free nodes."
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found >/dev/null 2>&1 || true
  kubectl wait --for=delete pod -l serving.trin.io/inferenceservice=qwen7b-service -n "$NS" --timeout=300s >/dev/null 2>&1 || true
  kubectl delete scaledobject qwen-service-vllm-queue -n "$NS" --ignore-not-found >/dev/null 2>&1 || true
  kubectl apply -f "$REPO_ROOT/test/keda-scaledobject.yaml" | tee "$D/so-apply.txt"
  local N max
  for spec in "1:2" "2:2" "4:4"; do
    N="${spec%%:*}"; max="${spec##*:}"
    log "C: min=$N max=$max"
    kubectl patch scaledobject qwen-service-vllm-queue -n "$NS" --type merge \
      -p "{\"spec\":{\"minReplicaCount\":$N,\"maxReplicaCount\":$max}}" | tee "$D/min$N-patch.txt"
    kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service -n "$NS" --timeout=600s >/dev/null 2>&1 || true
    # wait until exactly N ready pods
    for _ in $(seq 1 120); do
      local ready
      ready="$(kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service -o jsonpath='{range .items[?(@.status.containerStatuses[0].ready==true)]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c . || true)"
      echo "$(date -u +%FT%TZ) ready=$ready want=$N" >> "$D/min$N-wait.txt"
      [[ "$ready" -ge "$N" ]] && break
      sleep 5
    done
    pods_snapshot | tee "$D/min$N-pods.txt"
    hpa_snapshot | tee "$D/min$N-hpa-pre.txt"
    local bstart
    bstart="$(date -u +%s)"
    echo "burst_start=$bstart min=$N" | tee "$D/min$N-burst.txt"
    local S
    S="$(start_sampler "$D/min$N-samples.txt" qwen-service 45)"
    submit_job "p6g5-c-min$N" 1:0 192 180 64 512 "c-min$N" | tee "$D/min$N-apply.txt"
    wait_job "p6g5-c-min$N" 600
    kill "$S" 2>/dev/null || true
    kubectl logs "job/p6g5-c-min$N" -n "$NS" > "$D/min$N-load.txt" 2>&1 || true
    grep -h '^SUMMARY' "$D/min$N-load.txt" >> "$D/summaries.txt" 2>/dev/null || true
    hpa_snapshot | tee "$D/min$N-hpa-post.txt"
    pods_snapshot | tee "$D/min$N-pods-post.txt"
    python3 "$REPO_ROOT/scripts/phase6-g5-analyze.py" --log "$D/min$N-load.txt" \
      --burst-start "$bstart" --slo-ms 100 --out "$D/min$N-slo.md" | tee "$D/min$N-slo.md" || true
  done
  log "C: restore min=1 max=2"
  kubectl patch scaledobject qwen-service-vllm-queue -n "$NS" --type merge \
    -p '{"spec":{"minReplicaCount":1,"maxReplicaCount":2}}' | tee "$D/restore-patch.txt"
  kubectl rollout status deployment/qwen-service -n "$NS" --timeout=900s | tee "$D/restore-rollout.txt"
  cat "$D/summaries.txt" || true
  log "C done"
}

close() {
  local D="$ART_DIR/close"
  mkdir -p "$D"
  log "collecting evidence"
  EVID_QUERIES="$(printf '%s\n' \
    'sum by (pod) (vllm:request_success_total{service=~"qwen.*"})' \
    'sum by (pod) (vllm:prompt_tokens_total{service=~"qwen.*"})' \
    'histogram_quantile(0.5, sum by (le,service) (rate(vllm:time_to_first_token_seconds_bucket{service=~"qwen.*"}[5m])))' \
    'histogram_quantile(0.95, sum by (le,service) (rate(vllm:time_to_first_token_seconds_bucket{service=~"qwen.*"}[5m])))' \
    'sum by (pod) (vllm:kv_cache_usage_perc{service=~"qwen.*"})' \
    'DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"}' \
    'DCGM_FI_DEV_FB_USED{job=~".*dcgm-exporter.*"}')" \
    ART_ROOT="$ART_ROOT" RUN_ID="$RUN_ID" PROM_URL="$PROM_URL" \
    "$REPO_ROOT/scripts/collect-evidence.sh" all | tee "$D/collect-evidence.txt"

  kubectl delete job -n "$NS" -l app=phase6-g4-mixed --ignore-not-found | tee "$D/delete-jobs.txt" || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/test/keda-scaledobject.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true

  az aks nodepool scale -g "$RG" --cluster-name "$AKS" -n gputest --node-count 0 -o none
  az aks stop -g "$RG" -n "$AKS" -o none || true
  pkill -f 'kubectl port-forward' || true
  az aks show -g "$RG" -n "$AKS" --query "{state:powerState.code,prov:provisioningState}" -o json | tee "$D/powerstate.json"
  az aks nodepool show -g "$RG" --cluster-name "$AKS" -n gputest --query count -o tsv | tee "$D/gputest-count.txt"
  log "G5 window closed"
}

case "${1:-}" in
  bootstrap) bootstrap ;;
  drive-a) drive_a ;;
  drive-b1) drive_b1 ;;
  drive-b2) drive_b2 ;;
  drive-b3) drive_b3 ;;
  drive-c) drive_c ;;
  close) close ;;
  *) echo "usage: $0 bootstrap|drive-a|drive-b1|drive-b2|drive-b3|drive-c|close"; exit 1 ;;
esac
