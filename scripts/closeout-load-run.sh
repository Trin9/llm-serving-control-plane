#!/usr/bin/env bash
# Stage 3 load run: 100c x 3m load + KEDA observe sampling.
# Usage: bash scripts/closeout-load-run.sh <label>
# Outputs: /tmp/load-<label>.txt, /tmp/observe-<label>.txt/.log
set -uo pipefail
cd "$(dirname "$0")/.."
LABEL=${1:?usage: closeout-load-run.sh <label>}

TOKEN=$(bash scripts/mint-jwt.sh smoke-user 7200)

bash scripts/keda-observe.sh 26 8 "/tmp/observe-${LABEL}.log" > "/tmp/observe-${LABEL}.txt" 2>&1 </dev/null &
OBSPID=$!

kubectl run loadgen --rm -i --restart=Never -n default --image=williamyeh/hey -- \
  -z 3m -c 100 -t 60 -m POST \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${TOKEN}" \
  -d "$(cat test/stress-test-body-qwen-heavy.json)" \
  http://llm-operator-gate-service.default.svc.cluster.local:8080/v1/chat/completions \
  > "/tmp/load-${LABEL}.txt" 2>&1

echo "LOAD-DONE $(date -u +%FT%TZ)"
wait "$OBSPID" || true

echo "== load summary (${LABEL}) =="
tail -n 25 "/tmp/load-${LABEL}.txt"
echo "== observe (${LABEL}) =="
cat "/tmp/observe-${LABEL}.txt"
