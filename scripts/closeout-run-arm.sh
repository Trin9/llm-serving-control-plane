#!/usr/bin/env bash
# Run a closeout load arm (python loadgen Job) + optional KEDA observe sampling.
# Usage: bash scripts/closeout-run-arm.sh <label> <concurrency> <duration_seconds> [observe]
# Outputs: /tmp/py-load-<label>.txt (job logs incl. SUMMARY), /tmp/observe-<label>.txt
set -uo pipefail
cd "$(dirname "$0")/.."
LABEL=${1:?usage: closeout-run-arm.sh <label> <conc> <dur> [observe]}
CONC=${2:?usage: closeout-run-arm.sh <label> <conc> <dur> [observe]}
DUR=${3:?usage: closeout-run-arm.sh <label> <conc> <dur> [observe]}
OBSERVE=${4:-}

kubectl delete job loadgen-job -n default --ignore-not-found >/dev/null 2>&1
sed -e "s/__CONC__/${CONC}/" -e "s/__DUR__/${DUR}/" experiments/azure/loadgen-job.yaml | kubectl apply -f -

OBSPID=""
if [ "${OBSERVE}" = "observe" ]; then
  N=$(( (DUR + 60) / 8 ))
  bash scripts/keda-observe.sh "$N" 8 "/tmp/observe-${LABEL}.log" > "/tmp/observe-${LABEL}.txt" 2>&1 </dev/null &
  OBSPID=$!
fi

kubectl wait --for=condition=complete job/loadgen-job -n default --timeout=$((DUR + 180))s || true
kubectl logs job/loadgen-job -n default > "/tmp/py-load-${LABEL}.txt" 2>&1 || true

if [ -n "$OBSPID" ]; then
  wait "$OBSPID" || true
fi

echo "== load summary (${LABEL}) =="
grep '^SUMMARY' "/tmp/py-load-${LABEL}.txt" || tail -n 20 "/tmp/py-load-${LABEL}.txt"
