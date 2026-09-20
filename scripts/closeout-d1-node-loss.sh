#!/usr/bin/env bash
# D1 node-loss drill (EA, no spot quota -> remove a gputest node):
#   1) bring qwen-service to 2 replicas (one GPU pod per node)
#   2) light load 8c/420s for user-visible impact
#   3) az aks nodepool scale gputest 2->1  (simulated node loss; drains + deletes a node)
#   4) observe: nodes, pods, ready replicas, endpoints, events, loadgen errors
#   5) restore 1->2 -> new node joins COLD (image pull ~2.5min + vLLM start ~2.5min)
#   6) restore replicas=1 (declared state) and summarize
set -uo pipefail
cd /home/trin/project/elastic-llm-serving/llm-serving-control-plane
LOG=/tmp/d1.log
D=artifacts/phase5-azure/20260920T065208Z/closeout/drills/d1
mkdir -p "$D"
RG=llm-phase5-ea; CL=llm-aks-ea; POOL=gputest

snap() { # $1 label ; $2 full|light
  local label=$1 mode=${2:-full}
  {
    echo "===== [$label] $(date -u +%Y-%m-%dT%H:%M:%SZ) ====="
    echo "--- gputest nodes ---"
    kubectl get nodes 2>/dev/null | grep -E 'gputest|NAME' || echo 'no gputest nodes'
    echo "--- qwen pods ---"
    kubectl get pods -n default -o wide 2>/dev/null | grep -E 'qwen-service|NAME' || echo 'no qwen pods'
    echo "--- qwen deployment ready ---"
    kubectl get deploy qwen-service -n default -o jsonpath='{.status.readyReplicas} ready / {.spec.replicas} spec{"\n"}' 2>/dev/null || true
    if [ "$mode" = full ]; then
      echo "--- qwen service endpoints ---"
      kubectl get endpoints qwen-service -n default 2>/dev/null || echo '(no service/endpoints)'
      echo "--- hpa ---"
      kubectl get hpa -n default keda-hpa-qwen-service-vllm-queue 2>/dev/null || true
      echo "--- recent events (default ns) ---"
      kubectl get events -n default --sort-by=.lastTimestamp 2>/dev/null | tail -10
    fi
  }
}

{
  echo "### D1 start $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo '== preflight: az + state =='
  az account show -o none 2>&1 && echo 'az-ok' || { echo 'az-FAIL'; exit 1; }
  kubectl get nodes | grep -E 'gputest|NAME'
  kubectl get isvc qwen-service -n default -o jsonpath='{.spec.replicas} declared replicas{"\n"}'

  echo '== token refresh =='
  kubectl create secret generic loadgen-token -n default \
    --from-literal=TOKEN="$(bash scripts/mint-jwt.sh smoke-user 7200)" \
    --dry-run=client -o yaml | kubectl apply -f -

  echo '== pause KEDA (HPA min=1 would otherwise scale the 2nd replica back down) =='
  kubectl annotate scaledobject qwen-service-vllm-queue -n default autoscaling.keda.sh/paused=true --overwrite
  sleep 8
  echo '-- hpa after pause (expect NotFound) --'
  kubectl get hpa -n default 2>&1 || true

  echo "== step1: patch qwen-service replicas=2 ($(date -u +%H:%M:%SZ)) =="
  kubectl patch inferenceservice qwen-service -n default --type=merge -p '{"spec":{"replicas":2}}'
  for i in $(seq 1 36); do
    sleep 10
    R=$(kubectl get deploy qwen-service -n default -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    echo "t+$((i*10))s ready=${R:-0}/2"
    [ "${R:-0}" = "2" ] && break
  done
  echo "TWO_REPLICA_READY_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  snap "T0 2-replica baseline" full

  echo "== step2: light load 8c/420s ($(date -u +%H:%M:%SZ)) =="
  kubectl delete job loadgen-job -n default --ignore-not-found >/dev/null
  sed -e 's/__CONC__/8/' -e 's/__DUR__/420/' experiments/azure/loadgen-job.yaml | kubectl apply -f -
  sleep 20
  kubectl logs job/loadgen-job -n default --tail=3 2>/dev/null || true

  echo "== step3: NODE LOSS: gputest 2->1 ($(date -u +%Y-%m-%dT%H:%M:%SZ)) =="
  az aks nodepool scale -g "$RG" --cluster-name "$CL" --name "$POOL" --node-count 1 --no-wait -o none
  echo "scale-down issued, polling node count..."
  for i in $(seq 1 36); do
    sleep 10
    N=$(kubectl get nodes 2>/dev/null | grep -c gputest)
    echo "t+$((i*10))s gputest_nodes=$N"
    [ "$N" -le 1 ] && break
  done
  echo "NODE_LOST_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  snap "T1 after node loss" full

  echo '== step4: observe 2 minutes =='
  for i in 1 2 3 4; do
    sleep 30
    snap "T1.$i +$((i*30))s" light
  done
  echo '-- loadgen progress --'
  kubectl logs job/loadgen-job -n default --tail=5 2>/dev/null || true

  echo "== step5: RESTORE gputest 1->2 ($(date -u +%Y-%m-%dT%H:%M:%SZ)) =="
  az aks nodepool scale -g "$RG" --cluster-name "$CL" --name "$POOL" --node-count 2 --no-wait -o none
  echo "polling for 2 Ready nodes (new node is COLD)..."
  for i in $(seq 1 60); do
    sleep 10
    N=$(kubectl get nodes 2>/dev/null | grep gputest | grep -c ' Ready ')
    echo "t+$((i*10))s gputest_ready=$N"
    [ "$N" -ge 2 ] && break
  done
  echo "NODE_BACK_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  echo "== step6: wait qwen ready=2 on cold node (max 600s) =="
  for i in $(seq 1 60); do
    sleep 10
    R=$(kubectl get deploy qwen-service -n default -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    echo "t+$((i*10))s ready=${R:-0}/2"
    [ "${R:-0}" = "2" ] && break
  done
  echo "RECOVERED_2OF2_AT=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  snap "T2 recovered" full
  echo '-- new pod pull/start evidence --'
  kubectl get events -n default --sort-by=.lastTimestamp 2>/dev/null | grep -E 'Pulling|Pulled|Started|Scheduled|Created' | tail -12

  echo '== step7: loadgen final =='
  kubectl wait --for=condition=complete job/loadgen-job -n default --timeout=300s || true
  kubectl logs job/loadgen-job -n default 2>&1 | tail -12 || true

  echo "== step8: restore replicas=1 + unpause KEDA ($(date -u +%Y-%m-%dT%H:%M:%SZ)) =="
  kubectl patch inferenceservice qwen-service -n default --type=merge -p '{"spec":{"replicas":1}}'
  kubectl annotate scaledobject qwen-service-vllm-queue -n default autoscaling.keda.sh/paused- 2>&1 || true
  sleep 15
  kubectl get hpa -n default 2>&1 || true

  echo "### D1 end $(date -u +%Y-%m-%dT%H:%M:%SZ)"
} > "$LOG" 2>&1
cp "$LOG" "$D/D1-node-loss.log"
echo TASK-DONE
