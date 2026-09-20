#!/usr/bin/env bash
# Closeout verification helper (Stage 2: monitoring hardening + workload state).
# Usage: bash scripts/closeout-verify.sh
set -uo pipefail

echo "### helm history (monitoring-stack)"
helm history monitoring-stack -n monitoring | tail -5
echo
echo "### PVCs (monitoring)"
kubectl get pvc -n monitoring
echo
echo "### monitoring pods placement"
kubectl get pods -n monitoring -o wide
echo
echo "### prometheusrule"
kubectl get prometheusrule -n monitoring
echo
echo "### scaledobject / hpa (default)"
kubectl get scaledobject,hpa -n default
echo
echo "### qwen-service pod"
kubectl get pods -n default -l serving.trin.io/inferenceservice=qwen-service -o wide
