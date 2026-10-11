#!/usr/bin/env bash
# =============================================================================
# Phase 6 W7/P1 — hot-prefix BURST variant of the prefix-load A/B.
#
# The default driver's staggered worker start (0.15s) + 120-request queue never
# built the affinity owner's in-flight queue above the 64 spill threshold, so the
# hot-prefix arm showed zero spills. This variant fires all 96 workers at once
# (stagger=0) with a deeper 240-request queue so the owner saturates and the
# prefix-load spill path actually engages.
#
# Evidence root: artifacts/phase6-azure/<RUN_ID>/p1-burst/
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN_ID="${RUN_ID:-20261011T024900Z-W7fix}"
GATE_TAG="${GATE_TAG:-phase6-ca9f247}"
OP_TAG="${OP_TAG:-phase6-1fdc133}"
ACR="llmphase5eaacr.azurecr.io"
ART_DIR="$REPO_ROOT/artifacts/phase6-azure/$RUN_ID/p1-burst"
NS=default
CONC="${CONC:-96}"
TOTAL="${TOTAL:-240}"
MAX_TOKENS="${MAX_TOKENS:-64}"
TRIALS="${TRIALS:-3}"
STAGGER=0.0
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
  log "helm upgrade: ROUTER_STRATEGY=$strat"
  # --force-conflicts resolves the .spec.replicas field-manager conflict left by
  # a prior `kubectl scale` (kubectl owns the field after the main driver's
  # restore step); it lets helm's server-side apply take the field back.
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$OP_TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$GATE_TAG" \
    --set gate-service.replicaCount=1 \
    --set gate-service.env.ROUTER_STRATEGY="$strat" \
    --force-conflicts --wait --timeout 10m >> "$LOG" 2>&1
  kubectl rollout status deployment/llm-operator-gate-service -n "$NS" --timeout=300s >> "$LOG" 2>&1
  kubectl get deployment llm-operator-gate-service -n "$NS" \
    -o jsonpath='{.spec.template.spec.containers[0].env}' > "$ART_DIR/strategy-$strat.env.json"
}

run_trial() { # <strategy> <trial>
  local strat="$1" t="$2"
  local name="p1b-${strat}-t${t}"
  local seed="${RUN_ID}-burst-${strat}-${t}"
  snapshot "${strat}-t${t}-pre"
  log "trial: $name (stagger=$STAGGER total=$TOTAL conc=$CONC)"
  sed -e "s/__NAME__/$name/g" -e "s/__SEED__/$seed/g" -e "s/__PREFIX_CHARS__/$PREFIX_CHARS/g" \
      -e "s/__PREFIXES__/1/g" -e "s/__TOTAL__/$TOTAL/g" -e "s/__CONC__/$CONC/g" \
      -e "s/__MAX_TOKENS__/$MAX_TOKENS/g" -e "s/__STAGGER__/$STAGGER/g" \
      "$REPO_ROOT/experiments/azure/phase6/prefix-ab-burst-job.yaml" > "/tmp/$name.yaml"
  kubectl apply -f "/tmp/$name.yaml" >> "$LOG" 2>&1
  if ! kubectl wait --for=condition=complete "job/$name" -n "$NS" --timeout=600s >> "$LOG" 2>&1; then
    log "WARN: job $name did not complete cleanly"
  fi
  kubectl logs "job/$name" -n "$NS" > "$ART_DIR/trials/$name.log" 2>&1 || true
  sleep 20
  snapshot "${strat}-t${t}-post"
  kubectl delete job "$name" -n "$NS" --wait=false >> "$LOG" 2>&1 || true
  log "trial done: $name"
}

main() {
  log "BURST RUN_ID=$RUN_ID gate=$GATE_TAG op=$OP_TAG stagger=$STAGGER total=$TOTAL conc=$CONC"

  kubectl create configmap phase6-prefix-ab-src -n "$NS" \
    --from-file=prefix-ab-loadgen.py="$REPO_ROOT/experiments/azure/phase6/prefix-ab-loadgen.py" \
    --dry-run=client -o yaml | kubectl apply -f - >> "$LOG" 2>&1

  kubectl wait --for=condition=ready pod -l serving.trin.io/inferenceservice=qwen-service \
    -n "$NS" --timeout=900s >> "$LOG" 2>&1 || true

  local strat t
  for strat in prefix-hash prefix-load; do
    switch_strategy "$strat"
    for ((t = 1; t <= TRIALS; t++)); do
      run_trial "$strat" "$t"
    done
  done

  log "restore gateway replicas=3"
  kubectl scale deployment llm-operator-gate-service -n "$NS" --replicas=3 >> "$LOG" 2>&1
  log "BURST complete"
}

main "$@"
