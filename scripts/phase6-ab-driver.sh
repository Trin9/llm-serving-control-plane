#!/usr/bin/env bash
# =============================================================================
# Phase 6 (P6-A) prefix-aware routing A/B driver (EA cluster).
#
# Trial matrix (per strategy):
#   workload S (single hot prefix): 2000 / 8000 / 16000 chars x 3 trials
#   workload M (multi-tenant):      16000 chars, 40 prefixes x 3 trials
#
# For each trial it captures:
#   * raw loadgen log (REQ + SUMMARY JSON lines)    -> trials/<name>.log
#   * Prometheus instant snapshot (vLLM counters)   -> snapshots/<name>/q*.json
#   * trial wall-clock window for query_range math  -> trials/index.tsv
# It also flips ROUTER_STRATEGY via helm upgrade (chart/values only, no kubectl
# edit) and stores the rollout + gateway logline as strategy-switch proof.
#
# Usage:
#   RUN_ID=20260924T000000Z TAG=phase6-abc1234 scripts/phase6-ab-driver.sh
#
# Env:
#   RUN_ID      (required) UTC run id; align with collect-evidence.sh
#   TAG         (required) image tag built for this phase (gate+operator)
#   ACR         default llmphase5eaacr.azurecr.io
#   ART_ROOT    default <repo>/artifacts/phase6-azure
#   PROM_URL    default http://127.0.0.1:19090 (Prometheus port-forward)
#   TOTAL       requests per trial (default 120)
#   CONC        concurrency (default 8)
#   MAX_TOKENS  per request (default 32)
#   WARMUP      sequential warmup requests (default 2)
#   TRIALS      trials per (size, workload) point (default 3)
# =============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:?RUN_ID is required}"
TAG="${TAG:?TAG is required}"
ACR="${ACR:-llmphase5eaacr.azurecr.io}"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase6-azure}"
ART_DIR="$ART_ROOT/$RUN_ID/G1-prefix-ab"
TRIALS_DIR="$ART_DIR/trials"
SNAP_DIR="$ART_DIR/snapshots"
PROM_URL="${PROM_URL:-http://127.0.0.1:19090}"
TOTAL="${TOTAL:-120}"
CONC="${CONC:-8}"
MAX_TOKENS="${MAX_TOKENS:-32}"
WARMUP="${WARMUP:-2}"
TRIALS="${TRIALS:-3}"
NS=default

SIZES_S=(2000 8000 16000)
SIZES_M=(16000)
PREFIXES_M=40

mkdir -p "$TRIALS_DIR" "$SNAP_DIR"
DRIVER_LOG="$ART_DIR/driver.log"
log() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$DRIVER_LOG"; }

# ---------------------------------------------------------------- Prometheus --
# Per-pod vLLM counters for the qwen-service pool, plus gateway-side SSE metrics.
QUERIES=(
  'sum by (pod) (vllm:request_success_total{service="qwen-service"})'
  'sum by (pod) (vllm:prompt_tokens_total{service="qwen-service"})'
  'sum by (pod) (vllm:prompt_tokens_cached_total{service="qwen-service"})'
  'sum by (pod) (vllm:prefix_cache_queries_total{service="qwen-service"})'
  'sum by (pod) (vllm:prefix_cache_hits_total{service="qwen-service"})'
  'sum by (pod) (vllm:time_to_first_token_seconds_sum{service="qwen-service"})'
  'sum by (pod) (vllm:time_to_first_token_seconds_count{service="qwen-service"})'
  'sum by (pod) (vllm:request_prefill_time_seconds_sum{service="qwen-service"})'
  'sum by (pod) (vllm:request_prefill_time_seconds_count{service="qwen-service"})'
  'sum by (pod) (vllm:kv_cache_usage_perc{service="qwen-service"})'
  'sum by (pod) (vllm:num_requests_running{service="qwen-service"})'
  'sum(ai_ttft_seconds_count{service="llm-operator-gate-service"})'
  'sum(ai_ttft_seconds_sum{service="llm-operator-gate-service"})'
  'sum(ai_tpot_seconds_count{service="llm-operator-gate-service"})'
  'sum(ai_tpot_seconds_sum{service="llm-operator-gate-service"})'
  'sum by (code) (http_requests_total{service="llm-operator-gate-service"})'
)

prom_instant() { # <query> <outfile>
  local q="$1" out="$2"
  curl -fsS --get "$PROM_URL/api/v1/query" --data-urlencode "query=$q" > "$out" 2>/dev/null \
    || echo '{"status":"error","error":"query failed"}' > "$out"
}

snapshot() { # <tag>
  local tag="$1" dir="$SNAP_DIR/$tag"
  mkdir -p "$dir"
  local i=0 q
  for q in "${QUERIES[@]}"; do
    prom_instant "$q" "$dir/q${i}.json"
    i=$((i + 1))
  done
  local j=0
  { for q in "${QUERIES[@]}"; do echo "q${j}: $q"; j=$((j + 1)); done; } > "$dir/README.txt"
  log "snapshot saved: snapshots/$tag (${#QUERIES[@]} queries)"
}

preflight_prom() {
  if ! curl -fsS "$PROM_URL/-/ready" >/dev/null 2>&1; then
    log "FATAL: Prometheus not reachable at $PROM_URL (start the port-forward first)"
    exit 1
  fi
  log "Prometheus reachable at $PROM_URL"
}

# ------------------------------------------------------------------- Gateway --
switch_strategy() { # <strategy>
  local strat="$1"
  log "helm upgrade: ROUTER_STRATEGY=$strat"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$TAG" \
    --set gate-service.env.ROUTER_STRATEGY="$strat" \
    --wait --timeout 10m >> "$DRIVER_LOG" 2>&1
  kubectl rollout status deployment/llm-operator-gate-service -n "$NS" --timeout=300s >> "$DRIVER_LOG" 2>&1

  # Proof: env var on the live deployment + the startup logline of the new pod.
  kubectl get deployment llm-operator-gate-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].env}' \
    > "$ART_DIR/strategy-$strat.env.json"
  local out="$ART_DIR/strategy-$strat.gateway-log.txt"
  sleep 5
  kubectl logs deployment/llm-operator-gate-service -n "$NS" --tail=200 2>/dev/null \
    | grep -E 'routing strategy|ROUTER_STRATEGY' > "$out" || true
  log "strategy proof saved: strategy-$strat.env.json / strategy-$strat.gateway-log.txt"
}

verify_pool_ready() {
  log "waiting for qwen-service 2/2 ready..."
  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service \
    -n "$NS" --timeout=900s >> "$DRIVER_LOG" 2>&1 || true
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service -o wide \
    > "$ART_DIR/pool-pods.txt"
  local ready
  ready="$(kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service \
    --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l)"
  log "running qwen-service pods: $ready"
  print_deployment_args > "$ART_DIR/deployment-args.txt"
}

print_deployment_args() {
  local d
  for d in $(kubectl get deploy -n "$NS" -o name | grep qwen-service); do
    echo "== $d"
    kubectl get "$d" -n "$NS" -o jsonpath='{.spec.template.spec.containers[0].args}'; echo
    kubectl get "$d" -n "$NS" -o jsonpath='{.spec.selector.matchLabels}'; echo
  done
}

ensure_job_source() {
  kubectl create configmap phase6-prefix-ab-src -n "$NS" \
    --from-file=prefix-ab-loadgen.py="$REPO_ROOT/experiments/azure/phase6/prefix-ab-loadgen.py" \
    --dry-run=client -o yaml | kubectl apply -f - >> "$DRIVER_LOG" 2>&1
  log "configmap phase6-prefix-ab-src refreshed"
}

# ---------------------------------------------------------------------- Trial --
run_trial() { # <strategy> <size> <prefixes> <trial_no>
  local strat="$1" size="$2" prefixes="$3" t="$4"
  local name="p6a-${strat//-/_}-${size}c-p${prefixes}-t${t}"
  local seed="${RUN_ID}-${strat}-${size}-${prefixes}-${t}"
  log "trial start: $name (seed=$seed)"

  sed -e "s/__NAME__/${name}/g" \
      -e "s/__SEED__/${seed}/g" \
      -e "s/__PREFIX_CHARS__/${size}/g" \
      -e "s/__PREFIXES__/${prefixes}/g" \
      -e "s/__TOTAL__/${TOTAL}/g" \
      -e "s/__CONC__/${CONC}/g" \
      -e "s/__MAX_TOKENS__/${MAX_TOKENS}/g" \
      "$REPO_ROOT/experiments/azure/phase6/prefix-ab-job.yaml" > "/tmp/${name}.yaml"

  local t0
  t0="$(date -u +%s)"
  kubectl apply -f "/tmp/${name}.yaml" >> "$DRIVER_LOG" 2>&1

  if ! kubectl wait --for=condition=complete "job/${name}" -n "$NS" --timeout=900s >> "$DRIVER_LOG" 2>&1; then
    log "WARN: job ${name} did not complete cleanly"
    kubectl get job "$name" -n "$NS" -o yaml > "$TRIALS_DIR/${name}.job.yaml" 2>&1 || true
    kubectl describe pod -l "job-name=${name}" -n "$NS" > "$TRIALS_DIR/${name}.pod-describe.txt" 2>&1 || true
  fi
  kubectl logs "job/${name}" -n "$NS" > "$TRIALS_DIR/${name}.log" 2>&1 || true
  local t1
  t1="$(date -u +%s)"

  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$name" "$strat" "$size" "$prefixes" "$t" "$t0" "$t1" "$seed" >> "$TRIALS_DIR/index.tsv"

  snapshot "$name"
  kubectl delete job "$name" -n "$NS" --wait=false >> "$DRIVER_LOG" 2>&1 || true
  log "trial done: $name (${t0}..${t1})"
}

# ----------------------------------------------------------------------- Main --
main() {
  log "run id: $RUN_ID  tag: $TAG  art: $ART_DIR"
  preflight_prom
  ensure_job_source

  local strat size t
  for strat in prefix-hash random; do
    switch_strategy "$strat"
    verify_pool_ready
    snapshot "phase-${strat}-start"

    # workload S: single shared prefix, sweep the prefix length
    for size in "${SIZES_S[@]}"; do
      for ((t = 1; t <= TRIALS; t++)); do
        run_trial "$strat" "$size" 1 "$t"
      done
    done

    # workload M: 40 distinct hot prefixes (multi-tenant cache thrash shape)
    for size in "${SIZES_M[@]}"; do
      for ((t = 1; t <= TRIALS; t++)); do
        run_trial "$strat" "$size" "$PREFIXES_M" "$t"
      done
    done
  done

  # Restore the production default strategy after the A/B.
  switch_strategy prefix-hash
  log "A/B complete. Trials: $(wc -l < "$TRIALS_DIR/index.tsv")"
}

main "$@"
