#!/usr/bin/env bash
# D2 metric-outage drill:
#   1) baseline snapshot (Prometheus up)
#   2) stop Prometheus STS (replicas=0)  -> KEDA/HPA lose metric source
#   3) sample HPA conditions / replicas / KEDA events for ~5 min (expect: hold replicas, no wrong scaling)
#   4) restore Prometheus STS (replicas=1) and verify HPA recovers
# Evidence is appended to: artifacts/phase5-azure/20260920T065208Z/closeout/drills/D2-metric-outage.log
set -uo pipefail

NS=monitoring
STS=prometheus-monitoring-stack-kube-prom-prometheus
HPA=keda-hpa-qwen-service-vllm-queue
ISVC=qwen-service
OUT_DIR="${OUT_DIR:-artifacts/phase5-azure/20260920T065208Z/closeout}"
LOG="$OUT_DIR/drills/D2-metric-outage.log"
mkdir -p "$(dirname "$LOG")"

sample() { # $1 = label
  local label=$1
  {
    echo "===== [$label] $(date -u +%Y-%m-%dT%H:%M:%SZ) ====="
    echo "--- predictor replicas ---"
    kubectl get deploy -n default -l serving.kserve.io/inferenceservice=$ISVC \
      -o jsonpath='{range .items[*]}{.metadata.name}: spec={.spec.replicas} status={.status.replicas} ready={.status.readyReplicas}{"\n"}{end}' 2>&1 || true
    echo "--- HPA ---"
    kubectl get hpa -n default "$HPA" 2>&1 || true
    echo "--- HPA conditions ---"
    kubectl get hpa -n default "$HPA" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}) {.message}{"\n"}{end}' 2>&1 || true
    echo "--- ScaledObject conditions ---"
    kubectl get scaledobject -n default "$ISVC-vllm-queue" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>&1 || true
    echo "--- prometheus endpoint check ---"
    curl -sS -m 4 -o /dev/null -w 'prometheus http_code=%{http_code}\n' \
      http://127.0.0.1:19090/-/ready 2>&1 || echo "prometheus port-forward unreachable"
  } | tee -a "$LOG"
}

echo "### D2 drill start $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
sample "T0 baseline (Prometheus UP)"

echo "### stopping Prometheus STS $STS -> 0" | tee -a "$LOG"
kubectl scale statefulset -n "$NS" "$STS" --replicas=0
sleep 25
sample "T1 +25s (Prometheus stopped)"

i=0
for i in 1 2 3 4 5 6; do
  sleep 45
  sample "T2.$i +$((25+i*45))s"
done

echo "--- KEDA operator log (last lines during outage) ---" | tee -a "$LOG"
kubectl logs -n keda deploy/keda-operator --since=6m 2>/dev/null | tail -15 | tee -a "$LOG" || true

echo "### restoring Prometheus STS $STS -> 1" | tee -a "$LOG"
kubectl scale statefulset -n "$NS" "$STS" --replicas=1
kubectl rollout status statefulset/"$STS" -n "$NS" --timeout=300s || true
sleep 60
sample "T3 restored +60s"

echo "### D2 drill end $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
