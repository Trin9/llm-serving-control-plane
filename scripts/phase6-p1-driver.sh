#!/usr/bin/env bash
# =============================================================================
# Phase 6 W7/P1 load-aware prefix-cache routing A/B driver (EA cluster).
#
# Compares prefix-hash (affinity only) vs prefix-load (affinity + spill when the
# affinity owner's in-flight count reaches ROUTER_SPILL_THRESHOLD=64) on real
# vLLM, with 2 ready 0.5B pods + 1 gateway replica.
#
# Workloads (both 8000-char prefix):
#   hot  1 shared prefix   -> single hot tenant, worst-case queueing
#   many 40 distinct prefixes -> multi-tenant cache-thrash shape
#
# Fixed concurrency 96 (above the 64 spill threshold so prefix-load can spill).
# 3 trials per (strategy, workload) = 12 trials.
#
# Metrics captured per trial (Prometheus instant snapshot before/after via the
# API-server proxy, no port-forward):
#   vLLM per-pod: request_success_total, prompt_tokens_total,
#                 prompt_tokens_cached_total, prefix_cache_{queries,hits}_total
#   gateway: gateway_router_spill_total, gateway_router_selection_total,
#            gateway_router_active_requests, ai_ttft_seconds_{count,sum}
#
# Evidence root: artifacts/phase6-azure/<RUN_ID>/p1-ab/
#   trials/<name>.log        raw loadgen log (REQ + SUMMARY lines)
#   snaps/<tag>/q*.json      Prometheus instant snapshots
#   strategy-<strat>.env.json deployment env proof
#
# Usage: RUN_ID=... GATE_TAG=... OP_TAG=... scripts/phase6-p1-driver.sh
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:-20261011T024900Z-W7fix}"
GATE_TAG="${GATE_TAG:-phase6-ca9f247}"
OP_TAG="${OP_TAG:-phase6-1fdc133}"
ACR="llmphase5eaacr.azurecr.io"
ART_DIR="$REPO_ROOT/artifacts/phase6-azure/$RUN_ID/p1-ab"
NS=default
CONC="${CONC:-96}"
TOTAL="${TOTAL:-120}"
MAX_TOKENS="${MAX_TOKENS:-64}"
WARMUP=2
TRIALS="${TRIALS:-3}"
PREFIX_CHARS=8000

mkdir -p "$ART_DIR/trials" "$ART_DIR/snaps"
LOG="$ART_DIR/driver.log"
log() { echo "[$(date -u +%H:%M:%S)] $*" | tee -a "$LOG"; }

PROM_PROXY="/api/v1/namespaces/monitoring/services/prometheus-operated:9090/proxy"

QUERIES=(
  'sum by (pod) (vllm:request_success_total{service="qwen-service"})'
  'sum by (pod) (vllm:prompt_tokens_total{service="qwen-service"})'
  'sum by (pod) (vllm:prompt_tokens_cached_total{service="qwen-service"})'
  'sum by (pod) (vllm:prefix_cache_queries_total{service="qwen-service"})'
  'sum by (pod) (vllm:prefix_cache_hits_total{service="qwen-service"})'
  'sum (gateway_router_spill_total{service="llm-operator-gate-service"})'
  'sum by (strategy) (gateway_router_selection_total{service="llm-operator-gate-service"})'
  'sum by (backend) (gateway_router_active_requests{service="llm-operator-gate-service"})'
  'sum (ai_ttft_seconds_count{service="llm-operator-gate-service"})'
  'sum (ai_ttft_seconds_sum{service="llm-operator-gate-service"})'
)

prom_q() { # <query> <outfile>
  local q="$1" out="$2"
  local enc
  enc="$(python3 -c 'import urllib.parse,sys;print(urllib.parse.quote(sys.argv[1]))' "$q")"
  kubectl get --raw "$PROM_PROXY/api/v1/query?query=$enc" > "$out" 2>/dev/null \
    || echo '{"status":"error"}' > "$out"
}

snapshot() { # <tag>
  local d="$ART_DIR/snaps/$1"
  mkdir -p "$d"
  local i=0 q
  for q in "${QUERIES[@]}"; do
    prom_q "$q" "$d/q$i.json"
    i=$((i + 1))
  done
  local j=0
  { for q in "${QUERIES[@]}"; do echo "q$j: $q"; j=$((j + 1)); done; } > "$d/README.txt"
}

switch_strategy() { # <strategy>
  local strat="$1"
  log "helm upgrade: ROUTER_STRATEGY=$strat (gateway pinned to 1 replica)"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$OP_TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$GATE_TAG" \
    --set gate-service.replicaCount=1 \
    --set gate-service.env.ROUTER_STRATEGY="$strat" \
    --wait --timeout 10m >> "$LOG" 2>&1
  kubectl rollout status deployment/llm-operator-gate-service -n "$NS" --timeout=300s >> "$LOG" 2>&1
  kubectl get deployment llm-operator-gate-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].env}' > "$ART_DIR/strategy-$strat.env.json"
  log "strategy proof: strategy-$strat.env.json"
}

run_job() { # <name> <seed> <prefixes> <total> <conc> <max_tokens>
  local name="$1" seed="$2" prefixes="$3" total="$4" conc="$5" mt="$6"
  sed -e "s/__NAME__/$name/g" -e "s/__SEED__/$seed/g" -e "s/__PREFIX_CHARS__/$PREFIX_CHARS/g" \
      -e "s/__PREFIXES__/$prefixes/g" -e "s/__TOTAL__/$total/g" -e "s/__CONC__/$conc/g" \
      -e "s/__MAX_TOKENS__/$mt/g" \
      "$REPO_ROOT/experiments/azure/phase6/prefix-ab-job.yaml" > "/tmp/$name.yaml"
  kubectl apply -f "/tmp/$name.yaml" >> "$LOG" 2>&1
  if ! kubectl wait --for=condition=complete "job/$name" -n "$NS" --timeout=600s >> "$LOG" 2>&1; then
    log "WARN: job $name did not complete cleanly"
  fi
  kubectl logs "job/$name" -n "$NS" > "$ART_DIR/trials/$name.log" 2>&1 || true
}

smoke() {
  log "smoke: p1-smoke (10 req, 4c)"
  run_job p1-smoke "${RUN_ID}-smoke" 1 10 4 16
  local s
  s="$(grep -o 'SUMMARY.*' "$ART_DIR/trials/p1-smoke.log" | head -c 300 || true)"
  log "smoke SUMMARY: $s"
  kubectl delete job p1-smoke -n "$NS" --wait=false >> "$LOG" 2>&1 || true
}

run_trial() { # <strategy> <prefixes> <trial>
  local strat="$1" prefixes="$2" t="$3"
  local name="p1-${strat}-p${prefixes}-t${t}"
  local seed="${RUN_ID}-${strat}-${prefixes}-${t}"
  local t0
  t0="$(date -u +%s)"
  snapshot "${strat}-p${prefixes}-t${t}-pre"
  log "trial: $name (prefixes=$prefixes conc=$CONC)"
  run_job "$name" "$seed" "$prefixes" "$TOTAL" "$CONC" "$MAX_TOKENS"
  # let Prometheus scrape the final vLLM counters before the post snapshot
  sleep 20
  snapshot "${strat}-p${prefixes}-t${t}-post"
  local t1
  t1="$(date -u +%s)"
  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$name" "$strat" "$prefixes" "$CONC" "$TOTAL" "$t0" "$t1" >> "$ART_DIR/trials/index.tsv"
  kubectl delete job "$name" -n "$NS" --wait=false >> "$LOG" 2>&1 || true
  log "trial done: $name ($((t1 - t0))s)"
}

main() {
  log "RUN_ID=$RUN_ID gate=$GATE_TAG op=$OP_TAG conc=$CONC total=$TOTAL max_tokens=$MAX_TOKENS"

  log "ensure configmap phase6-prefix-ab-src"
  kubectl create configmap phase6-prefix-ab-src -n "$NS" \
    --from-file=prefix-ab-loadgen.py="$REPO_ROOT/experiments/azure/phase6/prefix-ab-loadgen.py" \
    --dry-run=client -o yaml | kubectl apply -f - >> "$LOG" 2>&1

  log "verify pool ready (2 qwen-service pods)"
  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service \
    -n "$NS" --timeout=900s >> "$LOG" 2>&1 || true
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service -o wide \
    > "$ART_DIR/pool-pods.txt"

  smoke

  local strat pf t
  for strat in prefix-hash prefix-load; do
    switch_strategy "$strat"
    for pf in 1 40; do
      for ((t = 1; t <= TRIALS; t++)); do
        run_trial "$strat" "$pf" "$t"
      done
    done
  done

  log "restore gateway replicas=3"
  kubectl scale deployment llm-operator-gate-service -n "$NS" --replicas=3 >> "$LOG" 2>&1
  log "A/B complete. trials: $(wc -l < "$ART_DIR/trials/index.tsv")"
}

main "$@"
