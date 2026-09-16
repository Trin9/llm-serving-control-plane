#!/usr/bin/env bash
# =============================================================================
# Mint an HS256 JWT for gate-service smoke/load tests (Phase 5 Azure E0-E4).
#
# Usage:
#   scripts/mint-jwt.sh [userID] [ttl_seconds]
#
# Env:
#   JWT_SECRET  (default: aks-cpu-smoke-jwt-secret - the AKS smoke-test secret)
#
# Example:
#   export TOKEN="$(scripts/mint-jwt.sh)"
#   curl -H "Authorization: Bearer $TOKEN" ...
# =============================================================================
set -euo pipefail

SECRET="${JWT_SECRET:-aks-cpu-smoke-jwt-secret}"
USER_ID="${1:-smoke-user}"
TTL="${2:-7200}"

python3 - "$SECRET" "$USER_ID" "$TTL" <<'PY'
import base64, hashlib, hmac, json, sys, time

secret, uid, ttl = sys.argv[1], sys.argv[2], int(sys.argv[3])

def b64(b):
    return base64.urlsafe_b64encode(b).rstrip(b'=').decode()

header = b64(json.dumps({"alg": "HS256", "typ": "JWT"}).encode())
payload = b64(json.dumps({"userID": uid, "exp": int(time.time()) + ttl}).encode())
sig = b64(hmac.new(secret.encode(), f"{header}.{payload}".encode(), hashlib.sha256).digest())
print(f"{header}.{payload}.{sig}")
PY
