#!/usr/bin/env bash
# =============================================================================
# KPS (kube-prometheus-stack) Preflight Runbook Script
#
# Reproduces the CPU-only monitoring rehearsal from Phase 5 Task 8:
#   Kind cluster -> install monitoring-stack (KPS) -> deploy mock-vLLM + gate-service
#   -> apply ServiceMonitors -> send requests -> confirm gateway metrics are
#   scraped (UP) and queryable in Prometheus.
#
# NOTE: this only validates the GATEWAY metric path. vLLM / DCGM panels are
# intentionally NOT validated here (no GPU); record them as "not yet testable
# on CPU-only" per the runbook.
#
# Usage:
#   scripts/kps-preflight.sh setup     # ensure Kind cluster + monitoring-stack (idempotent)
#   scripts/kps-preflight.sh deploy    # load images + deploy mock/gateway/ServiceMonitor
#   scripts/kps-preflight.sh smoke     # send requests and verify Prometheus metrics
#   scripts/kps-preflight.sh all       # setup + deploy + smoke (default)
#   scripts/kps-preflight.sh cleanup   # remove app + (optional) monitoring + cluster
#
# Env overrides:
#   KPS_CLUSTER_NAME   (default: monitoring-preflight)
#   KPS_JWT_SECRET     (default: kps-preflight-secret  -- local only!)
#   KPS_GATEWAY_IMAGE  (default: gate-service:latest)
#   KPS_MOCK_IMAGE     (default: mock-vllm:latest)
#   KPS_APP_NS         (default: default)
#   KPS_REBUILD        (set to 1 to rebuild local images before loading)
# =============================================================================

set -euo pipefail

# ---- config ----------------------------------------------------------------
CLUSTER_NAME="${KPS_CLUSTER_NAME:-monitoring-preflight}"
CONTEXT="kind-${CLUSTER_NAME}"
MON_NS="monitoring"
APP_NS="${KPS_APP_NS:-default}"
RELEASE="monitoring-stack"
JWT_SECRET="${KPS_JWT_SECRET:-kps-preflight-secret}"
GATEWAY_IMAGE="${KPS_GATEWAY_IMAGE:-gate-service:latest}"
MOCK_IMAGE="${KPS_MOCK_IMAGE:-mock-vllm:latest}"
# The gateway Service name MUST match the dashboard's hardcoded label
# (service="llm-operator-gate-service") and the Helm chart's service name.
GATEWAY_SVC="llm-operator-gate-service"
# Labels use the Helm-compatible value (app: gate-service) so the SAME
# ServiceMonitor works for the Helm deployment on AKS and for this preflight.
GATEWAY_APP_LABEL="${KPS_GATEWAY_APP_LABEL:-gate-service}"
MOCK_SVC="mock-vllm"
GATEWAY_LPORT=18080   # local port-forward for the gateway
PROM_LPORT=19090      # local port-forward for Prometheus
REQUESTS_N="${KPS_REQUESTS_N:-10}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CHART_DIR="$REPO_ROOT/helm/monitoring-stack"
SM_FILE="$REPO_ROOT/test/gate-service-servicemonitor.yaml"

# ---- helpers ---------------------------------------------------------------
GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
log()  { echo -e "${GREEN}[kps-preflight]${NC} $*"; }
warn() { echo -e "${YELLOW}[kps-preflight][warn]${NC} $*"; }
err()  { echo -e "${RED}[kps-preflight][error]${NC} $*" >&2; }

require_cmds() {
  for c in kind kubectl helm docker python3 curl; do
    command -v "$c" >/dev/null 2>&1 || { err "required command not found: $c"; exit 1; }
  done
}

kube() { kubectl --context "$CONTEXT" "$@"; }

# ---- 1. setup --------------------------------------------------------------
ensure_cluster() {
  if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    log "Kind cluster '${CLUSTER_NAME}' already exists - reusing."
  else
    log "Creating Kind cluster '${CLUSTER_NAME}'..."
    kind create cluster --name "$CLUSTER_NAME" --wait 60s
  fi
  # Make sure the kubeconfig context exists (re-export if needed).
  if ! kubectl config get-contexts -o name 2>/dev/null | grep -q "^${CONTEXT}$"; then
    kind export kubeconfig --name "$CLUSTER_NAME"
  fi
  log "Waiting for nodes..."
  kube wait --for=condition=Ready node --all --timeout=120s
  kube get nodes -o wide
}

ensure_monitoring() {
  log "Ensuring monitoring-stack (KPS) is installed in namespace '${MON_NS}'..."
  (cd "$REPO_ROOT" && helm --kube-context "$CONTEXT" upgrade --install "$RELEASE" "$CHART_DIR" \
    -n "$MON_NS" --create-namespace --wait --timeout 15m)
  log "Monitoring stack ready:"
  kube -n "$MON_NS" get pods
}

setup() {
  require_cmds
  ensure_cluster
  ensure_monitoring
}

# ---- 2. deploy -------------------------------------------------------------
load_images() {
  local image="$1"
  if [[ -z "$(docker images -q "$image" 2>/dev/null)" ]]; then
    if [[ "${KPS_REBUILD:-0}" == "1" ]]; then
      log "Image '$image' not found locally, rebuilding..."
      case "$image" in
        mock-vllm:*)    docker build -t "$image" -f "$REPO_ROOT/Dockerfile.mock" "$REPO_ROOT" ;;
        gate-service:*) docker build -t "$image" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT" ;;
        *) err "do not know how to build '$image'"; exit 1 ;;
      esac
    else
      err "image '$image' missing locally and KPS_REBUILD!=1. Build it first (e.g. KPS_REBUILD=1)."
      exit 1
    fi
  fi
  log "Loading '$image' into Kind cluster '${CLUSTER_NAME}'..."
  kind load docker-image "$image" --name "$CLUSTER_NAME"
}

apply_app() {
  # ---- mock vLLM (CPU) ----
  log "Applying mock-vLLM Deployment/Service in '${APP_NS}'..."
  cat <<EOF | kube apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${MOCK_SVC}
  namespace: ${APP_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${MOCK_SVC}
  template:
    metadata:
      labels:
        app: ${MOCK_SVC}
    spec:
      containers:
        - name: mock
          image: ${MOCK_IMAGE}
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 8000
              name: http
          readinessProbe:
            httpGet: { path: /health, port: 8000 }
---
apiVersion: v1
kind: Service
metadata:
  name: ${MOCK_SVC}
  namespace: ${APP_NS}
spec:
  selector:
    app: ${MOCK_SVC}
  ports:
    - name: http
      port: 8000
      targetPort: http
EOF

  # ---- gate-service ----
  # automountServiceAccountToken:false -> in-cluster k8s discovery init fails,
  # so the gateway falls back to the static VLLM_URLS backend (the mock service).
  log "Applying gate-service Deployment/Service in '${APP_NS}'..."
  cat <<EOF | kube apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${GATEWAY_SVC}
  namespace: ${APP_NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${GATEWAY_APP_LABEL}
  template:
    metadata:
      labels:
        app: ${GATEWAY_APP_LABEL}
    spec:
      automountServiceAccountToken: false
      containers:
        - name: gateway
          image: ${GATEWAY_IMAGE}
          imagePullPolicy: IfNotPresent
          env:
            - name: VLLM_URLS
              value: "http://${MOCK_SVC}.${APP_NS}.svc.cluster.local:8000/v1/chat/completions"
            - name: JWT_SECRET
              value: "${JWT_SECRET}"
          ports:
            - containerPort: 8080
              name: http
          readinessProbe:
            httpGet: { path: /health, port: 8080 }
---
apiVersion: v1
kind: Service
metadata:
  name: ${GATEWAY_SVC}
  namespace: ${APP_NS}
  labels:
    app: ${GATEWAY_APP_LABEL}
spec:
  selector:
    app: ${GATEWAY_APP_LABEL}
  ports:
    - name: http
      port: 8080
      targetPort: http
EOF

  # ---- ServiceMonitor for the gateway ----
  log "Applying gateway ServiceMonitor (${SM_FILE})..."
  kube apply -f "$SM_FILE"

  log "Waiting for deployments to roll out..."
  kube -n "$APP_NS" rollout status deployment/${MOCK_SVC} --timeout=180s
  kube -n "$APP_NS" rollout status deployment/${GATEWAY_SVC} --timeout=180s
  kube -n "$APP_NS" get pods -o wide
}

deploy() {
  require_cmds
  load_images "$MOCK_IMAGE"
  load_images "$GATEWAY_IMAGE"
  apply_app
}

# ---- 3. smoke / verify -----------------------------------------------------
mint_jwt() {
  python3 - "$JWT_SECRET" <<'PY'
import base64, hashlib, hmac, json, sys
secret = sys.argv[1]
def b64(v): return base64.urlsafe_b64encode(v).rstrip(b"=")
h = b64(b'{"alg":"HS256","typ":"JWT"}')
p = b64(json.dumps({"userID":"kps-preflight"}, separators=(",", ":")).encode())
s = h + b"." + p
sig = b64(hmac.new(secret.encode(), s, hashlib.sha256).digest())
print((s + b"." + sig).decode())
PY
}

# True if something already answers HTTP on the given local port.
reachable() {
  local code
  code="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:$1/" 2>/dev/null || echo 000)"
  [[ "$code" != "000" ]]
}

smoke() {
  require_cmds
  local token; token="$(mint_jwt)"
  local pf_pids=()

  # Start background port-forwards ONLY if the local ports are not already served
  # (so an externally-running `kubectl port-forward` is reused when present).
  if ! reachable "$GATEWAY_LPORT"; then
    kube -n "$APP_NS" port-forward svc/${GATEWAY_SVC} ${GATEWAY_LPORT}:8080 >/dev/null 2>&1 &
    pf_pids+=("$!")
    log "started gateway port-forward :${GATEWAY_LPORT} (pid ${pf_pids[-1]})"
  fi
  if ! reachable "$PROM_LPORT"; then
    kube -n "$MON_NS" port-forward svc/monitoring-stack-kube-prom-prometheus ${PROM_LPORT}:9090 >/dev/null 2>&1 &
    pf_pids+=("$!")
    log "started prometheus port-forward :${PROM_LPORT} (pid ${pf_pids[-1]})"
  fi
  if [[ "${#pf_pids[@]}" -gt 0 ]]; then
    trap 'for p in "${pf_pids[@]}"; do kill "$p" 2>/dev/null || true; done' EXIT
  fi

  # wait for endpoints to answer
  log "Waiting for gateway (127.0.0.1:${GATEWAY_LPORT}) ..."
  for _ in $(seq 1 30); do
    reachable "$GATEWAY_LPORT" && break
    sleep 2
  done

  log "Sending ${REQUESTS_N} streaming requests through the gateway..."
  for i in $(seq 1 "$REQUESTS_N"); do
    curl -fsS --max-time 20 -N \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      --data '{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"hello"}]}' \
      "http://127.0.0.1:${GATEWAY_LPORT}/v1/chat/completions" >/dev/null 2>&1 || warn "request ${i} failed"
  done
  log "Requests sent."

  # give Prometheus a couple of scrape intervals (progress prints keep the log alive)
  log "Waiting for Prometheus scrape interval (~35s)..."
  for s in $(seq 1 7); do
    sleep 5
    log "  ... ${s}/7 waited"
  done

  # Robust check: poll until gateway metrics are actually queryable.
  log "Checking that gateway metrics have been scraped..."
  local metric_ok=0
  for i in $(seq 1 20); do
    if curl -fsS --get "http://127.0.0.1:${PROM_LPORT}/api/v1/query" \
        --data-urlencode "query=http_requests_total" 2>/dev/null \
        | python3 -c 'import sys,json;d=json.load(sys.stdin);r=d["data"]["result"];sys.exit(0 if r else 1)'; then
      metric_ok=1; break
    fi
    sleep 3
  done

  if [[ "$metric_ok" == "1" ]]; then
    log "gate-service metrics are present in Prometheus."
  else
    warn "No http_requests_total series yet; check ServiceMonitor selector, namespace, and the gateway /metrics endpoint."
  fi

  log "Saving active Prometheus targets (evidence)..."
  curl -fsS "http://127.0.0.1:${PROM_LPORT}/api/v1/targets" \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);[print(t["labels"].get("job",""), t["labels"].get("namespace",""), t["labels"].get("service",""), t["health"], t.get("lastError","")) for t in d["data"]["activeTargets"]]'

  log "Querying gateway metrics..."
  for q in \
      'http_requests_total' \
      'sum(rate(http_requests_total[1m]))' \
      'ai_ttft_seconds_count' \
      'ai_tpot_seconds_count'; do
    echo "--- query: ${q} ---"
    curl -fsS --get "http://127.0.0.1:${PROM_LPORT}/api/v1/query" \
      --data-urlencode "query=${q}" | python3 -m json.tool
  done

  log "Done. If the queries above are non-empty, the gateway metric path works; "
  log "open Grafana (http://localhost:3000, admin/admin) -> llm-serving-monitor dashboard "
  log "to see the gateway panels populated."
}

# ---- cleanup ---------------------------------------------------------------
cleanup_app() {
  log "Removing gateway/mock/ServiceMonitor from '${APP_NS}'..."
  kube -n "$APP_NS" delete deployment/${GATEWAY_SVC} service/${GATEWAY_SVC} \
    deployment/${MOCK_SVC} service/${MOCK_SVC} --ignore-not-found || true
  kube delete -f "$SM_FILE" --ignore-not-found || true
}

cleanup() {
  cleanup_app
  if [[ "${KPS_DELETE_MONITORING:-0}" == "1" ]]; then
    log "Uninstalling ${RELEASE}..."
    helm --kube-context "$CONTEXT" uninstall "$RELEASE" -n "$MON_NS" || true
  fi
  if [[ "${KPS_DELETE_CLUSTER:-0}" == "1" ]]; then
    log "Deleting Kind cluster '${CLUSTER_NAME}'..."
    kind delete cluster --name "$CLUSTER_NAME" || true
  fi
}

usage() {
  sed -n '2,40p' "$0"
}

# ---- main ------------------------------------------------------------------
cmd="${1:-all}"
case "$cmd" in
  setup)   setup ;;
  deploy)  deploy ;;
  smoke)   smoke ;;
  all)     setup && deploy && smoke ;;
  cleanup) cleanup ;;
  *)       usage ;;
esac
