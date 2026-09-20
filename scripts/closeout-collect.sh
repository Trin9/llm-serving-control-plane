#!/usr/bin/env bash
# Closeout recovery + collection (run via VS Code task when the main shell is stuck).
set -uo pipefail
export GIT_PAGER=cat PAGER=cat
cd "$(dirname "$0")/.."

echo "== $(date -u +%FT%TZ) phase1: kill stuck closeout procs =="
if pkill -f 'closeout-run-arm.sh armC2'; then echo "killed wrapper"; else echo "wrapper not found"; fi
if pkill -f 'observe-armC2.log'; then echo "killed observe"; else echo "observe not found"; fi
sleep 2

echo "== phase2: arm C (double replica) results =="
kubectl wait --for=condition=complete job/loadgen-job -n default --timeout=120s || true
kubectl logs job/loadgen-job -n default > /tmp/py-load-armC2-full.txt 2>&1 || true
grep '^SUMMARY' /tmp/py-load-armC2-full.txt || tail -n 5 /tmp/py-load-armC2-full.txt

echo "== phase3: metric checks (for dashboard fix) =="
curl -sS -m 10 --get 'http://127.0.0.1:19090/api/v1/query' --data-urlencode 'query=count(kube_node_status_capacity{resource="nvidia_com_gpu"})'; echo
curl -sS -m 10 --get 'http://127.0.0.1:19090/api/v1/query' --data-urlencode 'query=count(kube_node_labels{label_agentpool="gputest"})'; echo
curl -sS -m 10 --get 'http://127.0.0.1:19090/api/v1/query' --data-urlencode 'query=count(DCGM_FI_DEV_GPU_UTIL{job=~".*dcgm-exporter.*"})'; echo

echo "== phase4: cluster state =="
kubectl get pods -n default -l serving.trin.io/inferenceservice=qwen-service -o wide
kubectl get job loadgen-job -n default
kubectl get scaledobject qwen-service-vllm-queue -n default -o jsonpath='paused={.metadata.annotations.autoscaling\.keda\.sh/paused}{"\n"}'

echo "== phase5: json validate =="
python3 -m json.tool helm/monitoring-stack/dashboards/llm-serving-monitor.json > /dev/null && echo 'dashboard JSON valid'

echo "== phase6: post-check =="
echo "-- stuck processes? --"
pgrep -af 'helm upgrade|git commit|closeout-run-arm|keda-observe' | head -10 || echo 'none'
echo "-- helm history --"
helm history monitoring-stack -n monitoring 2>&1 | tail -3
echo "-- git --"
git --no-pager log --oneline -3
git status --short
echo "-- evidence --"
ls artifacts/phase5-azure/20260920T065208Z/closeout/ | tail -8
echo "== done =="
