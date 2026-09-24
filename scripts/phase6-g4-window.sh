#!/usr/bin/env bash
# =============================================================================
# Phase 6 G4 window: P6-D (multi-model coexistence + isolation) and
# P6-E remainder (0.5B sweep to 256c, five-stage cold start, scale-to-zero).
#
# Usage:
#   scripts/phase6-g4-window.sh bootstrap            # writes /tmp/phase6-g4.env
#   scripts/phase6-g4-window.sh run:baseline
#   scripts/phase6-g4-window.sh run:mixed
#   scripts/phase6-g4-window.sh run:isolation        # KILL_AT=120 default
#   scripts/phase6-g4-window.sh run:sweep05
#   scripts/phase6-g4-window.sh run:coldstart
#   scripts/phase6-g4-window.sh run:keda
#   scripts/phase6-g4-window.sh run:szero
#   scripts/phase6-g4-window.sh close
#
# Prereqs: kubectl context, prometheus port-forward on 19090, gateway
# port-forward on 18080 (for smoke requests). TAG defaults to phase6-ad7cc61.
# =============================================================================
set -euo pipefail

# The az CLI lives in a user-local prefix ($HOME/azcli/bin); VS Code task
# shells may start without it on PATH. Add it defensively.
if ! command -v az >/dev/null 2>&1; then
  for d in "$HOME/azcli/bin" "/usr/local/bin" "/opt/az/bin" "/snap/bin"; do
    if [[ -x "$d/az" ]]; then
      export PATH="$d:$PATH"
      break
    fi
  done
fi
command -v az >/dev/null 2>&1 || echo "WARN: az CLI not found on PATH (az-dependent steps will fail)" >&2

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENVFILE="${PHASE6_G4_ENV:-/tmp/phase6-g4.env}"
# shellcheck disable=SC1090
if [[ -z "${RUN_ID:-}" || -z "${RIDP:-}" ]] && [[ -f "$ENVFILE" ]]; then
  source "$ENVFILE"
fi
RUN_ID="${RUN_ID:?RUN_ID required (run bootstrap first; env file: $ENVFILE)}"
TAG="${TAG:-phase6-ad7cc61}"
RIDP="${RIDP:-p6g4-manual}"
ACR="${ACR:-llmphase5eaacr.azurecr.io}"
ART_ROOT="${ART_ROOT:-$REPO_ROOT/artifacts/phase6-azure}"
ART_DIR="$ART_ROOT/$RUN_ID/G4-mixed"
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
get_redis_pass() { kubectl get secret llm-operator-gate-env -n "$NS" -o jsonpath='{.data.REDIS_PASSWORD}' | base64 -d; }

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
  # hard fallback: poll completion state in case the watch errored
  for _ in $(seq 1 120); do
    local ok
    ok="$(kubectl get job "$1" -n "$NS" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)"
    [[ "$ok" == "1" ]] && break
    sleep 5
  done
}

start_sampler() { # outfile service count
  (
    for _ in $(seq 1 "$3"); do
      echo "$(date -u +%FT%TZ) running=$(prom "sum(vllm:num_requests_running{service=\"$2\"})") waiting=$(prom "sum(vllm:num_requests_waiting{service=\"$2\"})") kv=$(prom "max(vllm:kv_cache_usage_perc{service=\"$2\"})") hits=$(prom "sum(rate(vllm:prefix_cache_hits_total{service=\"$2\"}[1m]))") queries=$(prom "sum(rate(vllm:prefix_cache_queries_total{service=\"$2\"}[1m]))") gpu=$(prom 'max(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})')"
      sleep 5
    done
  ) > "$1" 2>&1 &
  echo $!
}

bootstrap() {
  mkdir -p "$ART_DIR"
  kubectl config use-context "$CTX" >/dev/null
  log "checking G2 residues before cleanup"
  {
    echo "== inferenceservices =="
    kubectl get inferenceservices.serving.trin.io -n "$NS" -o wide 2>&1 || true
    echo "== scaledobjects =="
    kubectl get scaledobject -n "$NS" 2>&1 || true
    echo "== leftover phase6 jobs =="
    kubectl get jobs -n "$NS" 2>&1 | grep -E "p6a-|p6b-|p6g4-" || echo "(none)"
  } | tee "$ART_DIR/residue-pre.txt"

  kubectl get nodes -L kubernetes.azure.com/agentpool -o wide | tee "$ART_DIR/nodes.txt"
  kubectl get nodes -l kubernetes.azure.com/agentpool=gputest --no-headers | grep -c ' Ready' \
    | tee "$ART_DIR/gputest-ready-count.txt"
  az aks nodepool show -g "$RG" --cluster-name "$AKS" -n gputest \
    --query '{count:count,vmSize:vmSize}' -o json | tee "$ART_DIR/nodepool-info.json"

  kubectl apply -f "$REPO_ROOT/helm/llm-operator/crds/crd.yaml" | tee "$ART_DIR/crd-apply.txt"
  helm upgrade llm-operator "$REPO_ROOT/helm/llm-operator" -n "$NS" \
    -f "$REPO_ROOT/helm/llm-operator/values-ea.yaml" \
    --set image.repository="$ACR/llm-operator" --set image.tag="$TAG" \
    --set gate-service.image.repository="$ACR/gate-service" --set gate-service.image.tag="$TAG" \
    --set gate-service.env.ROUTER_STRATEGY=prefix-hash \
    --wait --timeout 10m | tee "$ART_DIR/helm-upgrade.txt"

  log "cleaning experiment resources for a deterministic start"
  kubectl delete scaledobject qwen-service-vllm-queue qwen7b-service-vllm-queue -n "$NS" --ignore-not-found | tee "$ART_DIR/cleanup-scaledobjects.txt"
  kubectl delete job -n "$NS" -l app=phase6-g4-mixed --ignore-not-found >> "$ART_DIR/cleanup-scaledobjects.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found >> "$ART_DIR/cleanup-scaledobjects.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found >> "$ART_DIR/cleanup-scaledobjects.txt" 2>&1 || true

  log "deploying 0.5B + 7B (7B re-downloads ~5GB of weights on this node)"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" | tee "$ART_DIR/isvc05-apply.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" | tee "$ART_DIR/isvc7b-apply.txt"
  kubectl scale inferenceservice/qwen-service -n "$NS" --replicas=1 | tee "$ART_DIR/isvc05-scale1.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/vllm-servicemonitor-qwen7b.yaml" | tee "$ART_DIR/servicemonitor-apply.txt"
  kubectl apply -f "$REPO_ROOT/test/vllm-servicemonitor.yaml" 2>/dev/null | tee "$ART_DIR/servicemonitor05-apply.txt" || true

  for _ in $(seq 1 30); do
    kubectl get deploy qwen7b-service -n "$NS" >/dev/null 2>&1 && break
    sleep 2
  done

  log "waiting for both deployments (30 min budget)"
  kubectl rollout status deployment/qwen-service -n "$NS" --timeout=900s | tee "$ART_DIR/qwen05-ready.txt"
  kubectl rollout status deployment/qwen7b-service -n "$NS" --timeout=1800s | tee "$ART_DIR/qwen7b-ready.txt"
  kubectl get pods -n "$NS" -o wide | grep -E 'qwen|gate|redis' | tee "$ART_DIR/pods.txt"

  kubectl get deploy qwen-service qwen7b-service -n "$NS" \
    -o jsonpath='{range .items[*]}{.metadata.name}{" args="}{.spec.template.spec.containers[0].args}{"\n"}{end}' | tee "$ART_DIR/deploy-args.txt"

  # mixed loadgen source configmap + fresh JWT
  kubectl create configmap phase6-mixed-src -n "$NS" \
    --from-file=mixed-loadgen.py="$REPO_ROOT/experiments/azure/phase6/mixed-loadgen.py" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/configmap-mixed.txt"
  local token
  token="$(JWT_SECRET="${JWT_SECRET:-aks-cpu-smoke-jwt-secret}" "$REPO_ROOT/scripts/mint-jwt.sh" p6-g4-user 28800)"
  kubectl create secret generic loadgen-token -n "$NS" --from-literal=TOKEN="$token" \
    --dry-run=client -o yaml | kubectl apply -f - | tee "$ART_DIR/loadgen-secret.txt"

  {
    echo "RUN_ID=$RUN_ID"
    echo "TAG=$TAG"
    echo "RIDP=$RIDP"
    echo "ART_DIR=$ART_DIR"
  } > "$ENVFILE"
  log "bootstrap done; env written to $ENVFILE (RIDP=$RIDP)"
}

run_baseline() {
  local D="$ART_DIR/baseline"
  mkdir -p "$D"
  local S
  log "baseline A: 0.5B alone c=64 dur=120"
  S="$(start_sampler "$D/base05-samples.txt" qwen-service 30)"
  submit_job p6g4-base05 1:0 64 120 64 512 base05 | tee "$D/base05-apply.txt"
  wait_job p6g4-base05 600
  kill "$S" 2>/dev/null || true
  kubectl logs job/p6g4-base05 -n "$NS" > "$D/base05.log" 2>&1 || true

  log "baseline B: 7B alone c=32 dur=120"
  S="$(start_sampler "$D/base7b-samples.txt" qwen7b-service 30)"
  submit_job p6g4-base7b 0:1 32 120 64 512 base7b | tee "$D/base7b-apply.txt"
  wait_job p6g4-base7b 600
  kill "$S" 2>/dev/null || true
  kubectl logs job/p6g4-base7b -n "$NS" > "$D/base7b.log" 2>&1 || true

  grep -h '^SUMMARY' "$D"/*.log > "$D/summaries.txt" 2>/dev/null || true
  log "baseline done"; cat "$D/summaries.txt" || true
}

run_mixed() {
  local D="$ART_DIR/mixed"
  mkdir -p "$D"
  local S1 S2
  log "mixed: 0.5B c=64 + 7B c=32 concurrently, dur=180"
  S1="$(start_sampler "$D/samples-05b.txt" qwen-service 45)"
  S2="$(start_sampler "$D/samples-7b.txt" qwen7b-service 45)"
  submit_job p6g4-mixed 2:1 96 180 64 512 mixed | tee "$D/apply.txt"
  wait_job p6g4-mixed 900
  kill "$S1" "$S2" 2>/dev/null || true
  kubectl logs job/p6g4-mixed -n "$NS" > "$D/mixed.log" 2>&1 || true
  grep -h '^SUMMARY' "$D/mixed.log" > "$D/summaries.txt" 2>/dev/null || true
  log "mixed done"; cat "$D/summaries.txt" || true
}

run_isolation() {
  local D="$ART_DIR/isolation"
  mkdir -p "$D"
  local KILL_AT="${KILL_AT:-120}"
  local S1 S2
  log "isolation: mixed load dur=300, force-kill 7B pod at t+${KILL_AT}s"
  S1="$(start_sampler "$D/samples-05b.txt" qwen-service 75)"
  S2="$(start_sampler "$D/samples-7b.txt" qwen7b-service 75)"
  submit_job p6g4-isolation 2:1 96 300 64 512 isolation | tee "$D/apply.txt"
  sleep "$KILL_AT"
  {
    echo "kill_at=$(date -u +%FT%TZ) (t+${KILL_AT}s)"
    kubectl delete pod -l serving.trin.io/inferenceservice=qwen7b-service -n "$NS" --grace-period=0 --force
    echo "killed"
  } | tee "$D/kill.txt"
  wait_job p6g4-isolation 700
  kill "$S1" "$S2" 2>/dev/null || true
  kubectl logs job/p6g4-isolation -n "$NS" > "$D/isolation.log" 2>&1 || true
  grep -h '^SUMMARY' "$D/isolation.log" > "$D/summaries.txt" 2>/dev/null || true

  log "gate-side evidence (upstream_error)"
  kubectl logs -l app=gate-service -n "$NS" --tail=400 --prefix 2>/dev/null | grep -E "upstream_error|ERROR reading from vLLM" | tail -10 > "$D/gate-upstream-errors.txt" || true
  cat "$D/gate-upstream-errors.txt" || true

  log "ledger scan for $RIDP-b-* rows"
  local PASS
  PASS="$(get_redis_pass)"
  kubectl exec deploy/llm-operator-redis -n "$NS" -- sh -c "for k in \$(redis-cli -a '$PASS' --no-auth-warning --scan --pattern 'usage:ledger:${RIDP}-b-*'); do st=\$(redis-cli -a '$PASS' --no-auth-warning hget \$k state); rs=\$(redis-cli -a '$PASS' --no-auth-warning hget \$k request_status); echo \"\$k \$st \$rs\"; done" > "$D/ledger-b-rows.txt" 2>"$D/ledger-b-scan.err" || true
  awk '{print $2"/"$3}' "$D/ledger-b-rows.txt" 2>/dev/null | sort | uniq -c > "$D/ledger-b-states.txt" || true
  cat "$D/ledger-b-states.txt" || true
  local pendkey
  pendkey="$(grep -m1 ' pending ' "$D/ledger-b-rows.txt" 2>/dev/null | awk '{print $1}' || true)"
  if [[ -n "${pendkey:-}" ]]; then
    kubectl exec deploy/llm-operator-redis -n "$NS" -- redis-cli -a "$PASS" --no-auth-warning hgetall "$pendkey" > "$D/ledger-pending-sample.txt" 2>&1 || true
    cat "$D/ledger-pending-sample.txt" || true
  fi

  log "waiting for 7B pod recovery"
  kubectl rollout status deployment/qwen7b-service -n "$NS" --timeout=900s | tee "$D/7b-recovered.txt"

  log "post-recovery smoke to 7B"
  local token rid
  token="$(get_token)"
  curl -sS -D "$D/smoke-post-headers.txt" -o "$D/smoke-post-body.txt" \
    -H "Content-Type: application/json" -H "Authorization: Bearer $token" \
    -d "{\"model\":\"$MODEL7B\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello in exactly five words.\"}],\"max_tokens\":32}" \
    "$GATE_URL" || true
  rid="$(grep -i '^x-request-id:' "$D/smoke-post-headers.txt" 2>/dev/null | tr -d '\r' | awk '{print $2}')"
  echo "request-id=$rid" | tee "$D/smoke-post-request-id.txt"
  log "isolation done"
}

run_sweep05() {
  local D="$ART_DIR/sweep05"
  mkdir -p "$D"
  local S c
  for c in 8 32 64 128 256; do
    log "sweep 0.5B c=$c (512-token completions, dur=120)"
    S="$(start_sampler "$D/c$c-samples.txt" qwen-service 30)"
    submit_job "p6g4-sweep05-c$c" 1:0 "$c" 120 512 512 "sweep05-c$c" | tee "$D/c$c-apply.txt"
    wait_job "p6g4-sweep05-c$c" 600
    kill "$S" 2>/dev/null || true
    kubectl logs "job/p6g4-sweep05-c$c" -n "$NS" > "$D/c$c.log" 2>&1 || true
    grep -h '^SUMMARY' "$D/c$c.log" >> "$D/summaries.txt" 2>/dev/null || true
  done
  log "sweep05 done"; cat "$D/summaries.txt" || true
}

coldstart_one() { # service tag
  local svc="$1" tag="$2" D="$ART_DIR/coldstart"
  mkdir -p "$D"
  kubectl delete pod -l "serving.trin.io/inferenceservice=$svc" -n "$NS" --wait=false > "$D/$tag.delete.txt" 2>&1 || true
  local pod="" t0 t1 rd creation readyt
  # pick a pod that is NOT terminating (kubectl jsonpath filters cannot compare
  # to null reliably; do the filtering in python)
  for _ in $(seq 1 60); do
    pod="$(kubectl get pods -n "$NS" -l "serving.trin.io/inferenceservice=$svc" -o json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for it in d.get("items", []):
    if "deletionTimestamp" not in it["metadata"]:
        print(it["metadata"]["name"])
        break
' 2>/dev/null || true)"
    [[ -n "$pod" ]] && break
    sleep 2
  done
  creation="$(kubectl get pod "$pod" -n "$NS" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)"
  if [[ -n "$creation" ]]; then
    t0="$(date -u -d "$creation" +%s 2>/dev/null || date -u +%s)"
  else
    t0="$(date -u +%s)"
  fi
  echo "pod=$pod creation=$creation t0=$t0" > "$D/$tag.timing.txt"
  kubectl wait --for=condition=ready "pod/$pod" -n "$NS" --timeout=1800s >> "$D/$tag.timing.txt" 2>&1 || true
  t1="$(date -u +%s)"
  echo "ready_after=$((t1 - t0))s" | tee -a "$D/$tag.timing.txt"
  # precise number from the pod object: Ready condition lastTransitionTime - creationTimestamp
  readyt="$(kubectl get pod "$pod" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}' 2>/dev/null || true)"
  if [[ -n "$creation" && -n "$readyt" ]]; then
    rd="$(( $(date -u -d "$readyt" +%s) - $(date -u -d "$creation" +%s) ))"
    echo "ready_after_object=$((rd))s (creation -> Ready lastTransitionTime)" | tee -a "$D/$tag.timing.txt"
  else
    rd="$(sed -n 's/ready_after=\([0-9]*\)s/\1/p' "$D/$tag.timing.txt" | tail -1)"
  fi
  kubectl get pod "$pod" -n "$NS" -o yaml > "$D/$tag.pod.yaml" 2>&1 || true
  kubectl logs "pod/$pod" -n "$NS" > "$D/$tag.container.log" 2>&1 || true
  python3 "$REPO_ROOT/scripts/phase6-coldstart-parse.py" --pod "$D/$tag.pod.yaml" \
    --log "$D/$tag.container.log" --label "$tag" --ready-seconds "${rd:-0}" \
    --json-out "$D/$tag.stages.json" > "$D/$tag.stages.md" 2>&1 || true
}

run_coldstart() {
  log "cold start 0.5B"; coldstart_one qwen-service 05b
  log "cold start 7B"; coldstart_one qwen7b-service 7b
  log "coldstart done"
}

run_keda() {
  local D="$ART_DIR/keda"
  mkdir -p "$D"
  log "applying both ScaledObjects (independence test)"
  kubectl apply -f "$REPO_ROOT/test/keda-scaledobject.yaml" | tee "$D/so-05b-apply.txt"
  kubectl apply -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" | tee "$D/so-7b-apply.txt"
  kubectl get scaledobject -n "$NS" -o wide > "$D/so-list.txt" 2>&1 || true

  log "driving 7B-only load c=64 dur=300 while observing both HPAs"
  (
    for _ in $(seq 1 150); do
      echo "$(date -u +%FT%TZ) hpa05=$(kubectl get hpa -n "$NS" --no-headers 2>/dev/null | grep 'qwen-service-vllm' | awk '{print $4"->"$5}') hpa7b=$(kubectl get hpa -n "$NS" --no-headers 2>/dev/null | grep 'qwen7b' | awk '{print $4"->"$5}') isvc05=$(kubectl get isvc qwen-service -n "$NS" -o jsonpath='{.spec.replicas}' 2>/dev/null) isvc7b=$(kubectl get isvc qwen7b-service -n "$NS" -o jsonpath='{.spec.replicas}' 2>/dev/null) run7b=$(prom 'sum(vllm:num_requests_running{service="qwen7b-service"})')"
      sleep 5
    done
  ) > "$D/observe.txt" 2>&1 &
  local OBS=$!

  submit_job p6g4-keda7b 0:1 64 300 64 512 keda7b | tee "$D/load-apply.txt"
  wait_job p6g4-keda7b 900
  kubectl logs job/p6g4-keda7b -n "$NS" > "$D/load.log" 2>&1 || true

  log "post-load snapshot + cooldown observation (stabilization ~300s)"
  sleep 60
  kubectl get hpa -n "$NS" > "$D/hpa-mid.txt" 2>&1 || true
  kubectl get pods -n "$NS" -o wide | grep -E 'qwen' > "$D/pods-mid.txt" 2>&1 || true
  # bounded wait for 7B desired to return to 1
  for _ in $(seq 1 72); do
    local cur
    cur="$(kubectl get hpa -n "$NS" --no-headers 2>/dev/null | grep 'qwen7b' | awk '{print $4"->"$5}' || true)"
    echo "$(date -u +%FT%TZ) hpa7b=$cur" >> "$D/cooldown.txt"
    [[ "$cur" == "1->1" ]] && break
    sleep 5
  done
  kill "$OBS" 2>/dev/null || true
  kubectl get hpa -n "$NS" > "$D/hpa-post.txt" 2>&1 || true
  kubectl get isvc -n "$NS" -o wide > "$D/isvc-post.txt" 2>&1 || true
  kubectl get pods -n "$NS" -o wide | grep -E 'qwen' > "$D/pods-post.txt" 2>&1 || true
  kubectl get events -n "$NS" --sort-by=.lastTimestamp 2>/dev/null | grep -iE 'hpa|keda|qwen7b|qwen-service|insufficient|nvidia' | tail -80 > "$D/events.txt" || true
  log "keda done"
}

run_szero() {
  local D="$ART_DIR/szero"
  mkdir -p "$D"
  log "scale-to-zero feasibility: patch qwen-service ScaledObject minReplicaCount 1 -> 0"
  kubectl patch scaledobject qwen-service-vllm-queue -n "$NS" --type merge \
    -p '{"spec":{"minReplicaCount":0}}' | tee "$D/patch-min0.txt"
  log "waiting for 0.5B scale-down (HPA stabilization ~300s, up to 12 min)"
  local ts
  for _ in $(seq 1 144); do
    ts="$(kubectl get deploy qwen-service -n "$NS" -o jsonpath='{.status.replicas}' 2>/dev/null || echo "?")"
    echo "$(date -u +%FT%TZ) replicas=$ts" >> "$D/scale-to-zero.txt"
    [[ "$ts" == "0" ]] && break
    sleep 5
  done

  log "idle samples while at zero"
  for _ in $(seq 1 6); do
    echo "$(date -u +%FT%TZ) gpu=$(prom 'max(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})')" >> "$D/idle-gpu.txt"
    sleep 5
  done

  log "request while at zero (expecting fast failure: model de-registered)"
  local token
  token="$(get_token)"
  curl -sS -m 20 -D "$D/request-while-zero-headers.txt" -o "$D/request-while-zero-body.txt" \
    -H "Content-Type: application/json" -H "Authorization: Bearer $token" \
    -d "{\"model\":\"$MODEL05B\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"max_tokens\":16}" \
    "$GATE_URL" || true
  head -1 "$D/request-while-zero-headers.txt" >> "$D/request-while-zero.txt" 2>/dev/null || true
  head -c 300 "$D/request-while-zero-body.txt" >> "$D/request-while-zero.txt" 2>/dev/null || true

  log "checking whether KEDA wakes the model (expect: no wake; metric trigger needs live pods)"
  sleep 120
  kubectl get pods -n "$NS" -l serving.trin.io/inferenceservice=qwen-service -o wide > "$D/no-wake.txt" 2>&1 || true
  cat "$D/no-wake.txt" | tee -a "$D/request-while-zero.txt" || true

  log "restoring minReplicaCount=1 and waiting for recovery"
  kubectl patch scaledobject qwen-service-vllm-queue -n "$NS" --type merge \
    -p '{"spec":{"minReplicaCount":1}}' | tee "$D/patch-min1.txt"
  kubectl rollout status deployment/qwen-service -n "$NS" --timeout=900s | tee "$D/recovery.txt"
  token="$(get_token)"
  curl -sS -D "$D/recovery-smoke-headers.txt" -o "$D/recovery-smoke-body.txt" \
    -H "Content-Type: application/json" -H "Authorization: Bearer $token" \
    -d "{\"model\":\"$MODEL05B\",\"messages\":[{\"role\":\"user\",\"content\":\"hello again\"}],\"max_tokens\":16}" \
    "$GATE_URL" || true
  head -1 "$D/recovery-smoke-headers.txt" | tee -a "$D/recovery.txt" || true
  log "szero done"
}

close() {
  local D="$ART_DIR/close"
  mkdir -p "$D"
  log "collecting evidence"
  EVID_QUERIES="$(printf '%s\n' \
    'sum by (pod) (vllm:request_success_total{service=~"qwen.*"})' \
    'sum by (pod) (vllm:prompt_tokens_total{service=~"qwen.*"})' \
    'sum by (pod) (vllm:generation_tokens_total{service=~"qwen.*"})' \
    'histogram_quantile(0.5, sum by (le,service) (rate(vllm:time_to_first_token_seconds_bucket{service=~"qwen.*"}[5m])))' \
    'histogram_quantile(0.95, sum by (le,service) (rate(vllm:time_to_first_token_seconds_bucket{service=~"qwen.*"}[5m])))' \
    'sum by (pod) (vllm:kv_cache_usage_perc{service=~"qwen.*"})' \
    'sum(rate(vllm:prefix_cache_hits_total{service=~"qwen.*"}[1m]))' \
    'sum(rate(vllm:prefix_cache_queries_total{service=~"qwen.*"}[1m]))' \
    'DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"}' \
    'DCGM_FI_DEV_FB_USED{job=~".*dcgm-exporter.*"}')" \
    ART_ROOT="$ART_ROOT" RUN_ID="$RUN_ID" PROM_URL="$PROM_URL" \
    "$REPO_ROOT/scripts/collect-evidence.sh" all | tee "$D/collect-evidence.txt"

  log "deleting experiment resources"
  kubectl delete job -n "$NS" -l app=phase6-g4-mixed --ignore-not-found | tee "$D/delete-jobs.txt" || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/prefix-ab-isvc.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/qwen7b-isvc.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/test/keda-scaledobject.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true
  kubectl delete -f "$REPO_ROOT/experiments/azure/phase6/keda-scaledobject-qwen7b.yaml" --ignore-not-found >> "$D/delete-jobs.txt" 2>&1 || true

  log "scaling gputest to 0 and stopping the cluster"
  az aks nodepool scale -g "$RG" --cluster-name "$AKS" -n gputest --node-count 0 -o none
  az aks stop -g "$RG" -n "$AKS" -o none || true
  pkill -f 'kubectl port-forward' || true
  az aks show -g "$RG" -n "$AKS" --query "{state:powerState.code,prov:provisioningState}" -o json | tee "$D/powerstate.json"
  az aks nodepool show -g "$RG" --cluster-name "$AKS" -n gputest --query count -o tsv | tee "$D/gputest-count.txt"
  log "G4 window closed"
}

case "${1:-}" in
  bootstrap) bootstrap ;;
  run:baseline) run_baseline ;;
  run:mixed) run_mixed ;;
  run:isolation) run_isolation ;;
  run:sweep05) run_sweep05 ;;
  run:coldstart) run_coldstart ;;
  run:keda) run_keda ;;
  run:szero) run_szero ;;
  close) close ;;
  *) echo "usage: $0 bootstrap|run:{baseline|mixed|isolation|sweep05|coldstart|keda|szero}|close"; exit 1 ;;
esac
