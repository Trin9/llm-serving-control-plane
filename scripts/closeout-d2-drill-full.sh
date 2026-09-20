#!/usr/bin/env bash
# D2 metric-outage drill (FULL):
#   Attempt 1 (closeout-d2-drill.sh) revealed that prometheus-operator
#   self-heals a manual STS scale-down in ~1 min. For a true metric outage we
#   must ALSO stop the operator (it is restored only after Prometheus is back).
# Expected observations:
#   - HPA TARGETS <unknown>, ScalingActive=False (FailedGetExternalMetric)
#   - ScaledObject HPAActive=False (FailedGetExternalMetric), KEDAScalerFailed events
#   - predictor replicas HOLD at current value (no wrong scale up/down)
#   - Grafana Prometheus panels blank; alert evaluation server-side is blind
set -uo pipefail

NS=monitoring
STS=prometheus-monitoring-stack-kube-prom-prometheus
HPA=keda-hpa-qwen-service-vllm-queue
ISVC=qwen-service
OUT_DIR="${OUT_DIR:-artifacts/phase5-azure/20260920T065208Z/closeout}"
LOG="$OUT_DIR/drills/D2-metric-outage-full.log"
mkdir -p "$(dirname "$LOG")"

OP=$(kubectl get deploy -n "$NS" -o name | grep operator | head -1 | cut -d/ -f2)
echo "operator deployment resolved: $OP" | tee -a "$LOG"

sample() { # $1 = label
  local label=$1
  {
    echo "===== [$label] $(date -u +%Y-%m-%dT%H:%M:%SZ) ====="
    echo "--- prometheus sts/pods ---"
    kubectl get sts -n "$NS" "$STS" -o jsonpath='{.spec.replicas} spec / {.status.replicas} status / {.status.readyReplicas} ready{"\n"}' 2>&1 || true
    kubectl get pods -n "$NS" 2>/dev/null | grep 'prometheus-monitoring' || echo 'no prometheus pod'
    echo "--- predictor replicas ---"
    kubectl get deploy -n default -l serving.kserve.io/inferenceservice=$ISVC \
      -o jsonpath='{range .items[*]}{.metadata.name}: spec={.spec.replicas} status={.status.replicas} ready={.status.readyReplicas}{"\n"}{end}' 2>&1 || true
    echo "--- HPA ---"
    kubectl get hpa -n default "$HPA" 2>&1 || true
    echo "--- HPA conditions ---"
    kubectl get hpa -n default "$HPA" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>&1 || true
    echo "--- ScaledObject conditions ---"
    kubectl get scaledobject -n default "$ISVC-vllm-queue" \
      -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}' 2>&1 || true
  } | tee -a "$LOG"
}

echo "### D2-full start $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
sample "T0 baseline (Prometheus UP, operator UP)"

echo "### step1: disable prometheus-operator ($OP -> 0)" | tee -a "$LOG"
kubectl scale deploy -n "$NS" "$OP" --replicas=0
sleep 15
echo "### step2: stop Prometheus STS ($STS -> 0)" | tee -a "$LOG"
kubectl scale sts -n "$NS" "$STS" --replicas=0
sleep 25
sample "T1 +25s (operator down, Prometheus stopped)"

for i in 1 2 3 4 5 6; do
  sleep 45
  sample "T2.$i +$((25+i*45))s"
done

{
  echo "--- KEDA operator log during outage (tail) ---"
  kubectl logs -n keda deploy/keda-operator --since=7m 2>/dev/null | tail -12 || true
  echo "--- HPA events during outage ---"
  kubectl describe hpa -n default "$HPA" 2>/dev/null | grep -A10 'Events:' | tail -12 || true
} | tee -a "$LOG"

echo "### step3: restore Prometheus STS -> 1" | tee -a "$LOG"
kubectl scale sts -n "$NS" "$STS" --replicas=1
kubectl rollout status sts/"$STS" -n "$NS" --timeout=300s || true
sleep 45
echo "### step4: restore operator ($OP -> 1)" | tee -a "$LOG"
kubectl scale deploy -n "$NS" "$OP" --replicas=1
kubectl rollout status deploy/"$OP" -n "$NS" --timeout=180s || true
sleep 60
sample "T3 restored +60s"

echo "### D2-full end $(date -u +%Y-%m-%dT%H:%M:%SZ)" | tee -a "$LOG"
