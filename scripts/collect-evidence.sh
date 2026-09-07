#!/usr/bin/env bash
# =============================================================================
# Phase5 validation evidence collector (task8 runbook sections 0.4 / 6.x)
#
# Creates a git-ignored artifact bundle under artifacts/phase5-azure/<UTC-run-id>/
# and captures raw evidence: k8s state, Prometheus query results (JSON), Grafana
# dashboard JSON, and a record.md skeleton (code SHA / UTC range / limitations).
#
# Usage (run against the AKS / Kind context):
#   scripts/collect-evidence.sh init      # make run dirs; prints RUN_ID
#   scripts/collect-evidence.sh snapshot  # k8s nodes/pods/events/inferenceservices
#   scripts/collect-evidence.sh prom      # Prometheus query_range for key metrics
#   scripts/collect-evidence.sh dash      # export Grafana dashboard JSON (LLM Serving Monitor)
#   scripts/collect-evidence.sh record    # (re)generate record.md skeleton
#   scripts/collect-evidence.sh all       # init+snapshot+prom+dash+record
#
# Env overrides:
#   RUN_ID            (default: UTC timestamp)
#   KUBE_CONTEXT      (default: current context)
#   PROM_URL          (default: http://127.0.0.1:19090  - use a port-forward)
#   GRAFANA_URL       (default: http://localhost:3000)
#   GRAFANA_USER/PASS (default: admin / admin; basic auth)
#   EVID_QUERIES      (optional: newline-separated PromQL to capture)
# =============================================================================

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase5-azure}"
RUN_ID="${RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}"
RUN_DIR="$ART_ROOT/$RUN_ID"
PROM_URL="${PROM_URL:-http://127.0.0.1:19090}"
GRAFANA_URL="${GRAFANA_URL:-http://localhost:3000}"
GRAFANA_USER="${GRAFANA_USER:-admin}"
GRAFANA_PASS="${GRAFANA_PASS:-admin}"
DASH_UID="${DASH_UID:-llm-serving-monitor}"

log()  { echo -e "[collect-evidence][$RUN_ID] $*"; }
kube() { if [[ -n "${KUBE_CONTEXT:-}" ]]; then kubectl --context "$KUBE_CONTEXT" "$@"; else kubectl "$@"; fi; }

# PromQL list: gateway metrics verified locally; vLLM/DCGM only meaningful on GPU.
EVID_QUERIES_DEFAULT=(
  'sum(rate(http_requests_total{service="llm-operator-gate-service"}[1m]))'
  'histogram_quantile(0.95, sum(rate(ai_ttft_seconds_bucket{service="llm-operator-gate-service"}[5m])) by (le))'
  'sum(rate(ai_tpot_seconds_sum{service="llm-operator-gate-service"}[5m])) / sum(rate(ai_tpot_seconds_count{service="llm-operator-gate-service"}[5m]))'
  'sum by (service) (vllm:num_requests_waiting)'
  'DCGM_FI_DEV_GPU_UTIL{job="dcgm-exporter"}'
  'DCGM_FI_DEV_FB_USED{job="dcgm-exporter"}'
)

cmd_init() {
  mkdir -p "$RUN_DIR"/{manifest,cluster,logs,metrics,dashboards,load,profiler}
  log "run dir: $RUN_DIR"
}

cmd_snapshot() {
  cmd_init
  log "capturing cluster state..."
  kube get nodes,pods,deployments,services,endpoints,inferenceservices -A -o wide \
    > "$RUN_DIR/cluster/cluster-state.txt" 2>&1 || true
  kube get events -A --sort-by=.lastTimestamp > "$RUN_DIR/cluster/events.txt" 2>&1 || true
  kube version > "$RUN_DIR/cluster/versions.txt" 2>&1 || true
  kube get inferenceservice -A -o yaml > "$RUN_DIR/manifest/inferenceservices.yaml" 2>&1 || true
  log "cluster snapshot written to $RUN_DIR/cluster"
}

cmd_prom() {
  cmd_init
  local start end step query file
  start="$(date -u -d '30 min ago' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-30M +%Y-%m-%dT%H:%M:%SZ)"
  end="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  step="${EVID_STEP:-15s}"
  log "capturing Prometheus query_range ($start .. $end, step $step)"
  local queries=( "${EVID_QUERIES_DEFAULT[@]}" )
  if [[ -n "${EVID_QUERIES:-}" ]]; then
    IFS=$'\n' read -r -d '' -a queries <<< "$EVID_QUERIES" || true
  fi
  local i=0
  for q in "${queries[@]}"; do
    [ -z "$q" ] && continue
    file="$RUN_DIR/metrics/query_${i}.json"
    # short filename-safe label from the first metric name
    local name; name="$(echo "$q" | sed -E 's/.*\(([A-Za-z_][A-Za-z0-9_]*).*/\1/;s/.*([A-Za-z_][A-Za-z0-9_]*\{.*)/\1/;s/[^A-Za-z0-9_]/_/g' | cut -c1-40)"
    file="$RUN_DIR/metrics/${name:-q$i}.json"
    if curl -fsS --get "$PROM_URL/api/v1/query_range" \
        --data-urlencode "query=$q" \
        --data-urlencode "start=$start" --data-urlencode "end=$end" \
        --data-urlencode "step=$step" > "$file" 2>/dev/null; then
      log "  saved [$name] -> $(basename "$file")"
    else
      log "  [warn] query failed: $q"
    fi
    i=$((i+1))
  done
  # store metadata beside the JSON files
  {
    echo "prometheus_url: $PROM_URL"
    echo "start_utc: $start"
    echo "end_utc:   $end"
    echo "step:      $step"
    echo "queries:"
    local j=0
    for q in "${queries[@]}"; do echo "  q$j: $q"; j=$((j+1)); done
  } > "$RUN_DIR/metrics/README.txt"
}

cmd_dash() {
  cmd_init
  log "exporting Grafana dashboard uid=$DASH_UID"
  curl -fsS -u "$GRAFANA_USER:$GRAFANA_PASS" \
    "$GRAFANA_URL/api/dashboards/uid/$DASH_UID" \
    > "$RUN_DIR/dashboards/$DASH_UID.json" 2>/dev/null \
    || log "  [warn] Grafana export failed (is it reachable/authorized?)"
}

cmd_record() {
  cmd_init
  local sha=""
  ( cd "$REPO_ROOT" && git rev-parse HEAD 2>/dev/null ) && sha="$(cd "$REPO_ROOT" && git rev-parse HEAD)" || sha="unknown"
  cat > "$RUN_DIR/record.md" <<EOF
# Validation Record: $(date -u +%Y-%m-%dT%H:%M:%SZ)Z - <E0/E1/E2/E3/E4>

- RUN_ID: $RUN_ID
- Code revision: $sha
- Azure region/resource group: <...>
- AKS version and node pools: <...>
- GPU: <NVIDIA T4 x N; Spot or regular>
- Runtime: <device plugin/GPU Operator, DCGM versions>
- Workload: <vLLM image, model revision, resource profile (gpu-t4-small recommended for NC4as_T4_v3)>
- Commands: <see artifacts under $RUN_DIR>
- Result: PASS / FAIL / INCONCLUSIVE
- Evidence: $RUN_DIR/{cluster,metrics,dashboards}
- Proven conclusion: <narrow factual statement>
- Limitation: <what this run did not prove>
- Cost and disruption: <duration, Spot evictions, cleanup confirmed>
EOF
  log "record.md skeleton written."
}

cmd_all() { cmd_init; cmd_snapshot; cmd_prom; cmd_dash; cmd_record; }

case "${1:-all}" in
  init)     cmd_init ;;
  snapshot) cmd_snapshot ;;
  prom)     cmd_prom ;;
  dash)     cmd_dash ;;
  record)   cmd_record ;;
  all)      cmd_all ;;
  *) echo "usage: $0 init|snapshot|prom|dash|record|all"; exit 1 ;;
esac
