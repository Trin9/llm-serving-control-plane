#!/usr/bin/env bash

set -euo pipefail

CLUSTER_NAME="gateway-kind-e2e"
NAMESPACE="default"
CONTROLLER_PID=""
PORT_FORWARD_PID=""
GATEWAY_PORT="18080"
JWT_SECRET="kind-e2e-jwt-secret"

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

cleanup() {
	printf "\n${YELLOW}Cleaning up Gateway Kind E2E environment...${NC}\n"
    if [[ -n "$PORT_FORWARD_PID" ]]; then
        kill "$PORT_FORWARD_PID" 2>/dev/null || true
    fi
    if [[ -n "$CONTROLLER_PID" ]]; then
        kill "$CONTROLLER_PID" 2>/dev/null || true
    fi
    kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  	printf "${GREEN}Cleanup complete${NC}\n"
}
trap cleanup EXIT

require_command() {
    command -v "$1" >/dev/null 2>&1 || {
		printf "${RED}Required command not found: %s${NC}\n" "$1" >&2
        exit 1
    }
}

for command in kind kubectl kustomize docker curl python3; do
    require_command "$command"
done

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OPERATOR_DIR="$ROOT_DIR/operator"
CONTEXT="kind-$CLUSTER_NAME"

if [[ -x "$HOME/go/bin/controller-gen" ]]; then
    CONTROLLER_GEN="$HOME/go/bin/controller-gen"
elif [[ -x "$HOME/.local/bin/controller-gen" ]]; then
    CONTROLLER_GEN="$HOME/.local/bin/controller-gen"
else
	printf "${RED}controller-gen not found in ~/go/bin or ~/.local/bin${NC}\n" >&2
    exit 1
fi

printf "${GREEN}=== Gateway Kind E2E Test ===${NC}\n\n"
printf "${YELLOW}[1/9] Generating Operator manifests and building local images...${NC}\n"
(
    cd "$OPERATOR_DIR"
    "$CONTROLLER_GEN" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
  GOTOOLCHAIN=auto go build -o /tmp/gateway-kind-e2e-operator ./cmd/main.go
)
docker build -t mock-vllm:latest -f "$ROOT_DIR/Dockerfile.mock" "$ROOT_DIR"
docker build -t gate-service:latest -f "$ROOT_DIR/Dockerfile" "$ROOT_DIR"
printf "${GREEN}✓ Local images built${NC}\n\n"

printf "${YELLOW}[2/9] Creating Kind cluster...${NC}\n"
kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
kind create cluster --name "$CLUSTER_NAME" --wait 60s
kind load docker-image mock-vllm:latest --name "$CLUSTER_NAME"
kind load docker-image gate-service:latest --name "$CLUSTER_NAME"
printf "${GREEN}✓ Cluster created and images loaded${NC}\n\n"

printf "${YELLOW}[3/9] Installing CRD and starting Operator...${NC}\n"
(
    cd "$OPERATOR_DIR"
    kustomize build config/crd | kubectl --context "$CONTEXT" apply -f -
)
kubectl --context "$CONTEXT" wait --for=condition=established --timeout=30s crd/inferenceservices.serving.trin.io
KUBECONFIG="$HOME/.kube/config" /tmp/gateway-kind-e2e-operator \
  --metrics-bind-address=0 \
  --health-probe-bind-address=:18081 \
  > /tmp/gateway-kind-e2e-controller.log 2>&1 &
CONTROLLER_PID=$!

for _ in {1..5}; do
    if ! kill -0 "$CONTROLLER_PID" 2>/dev/null; then
        cat /tmp/gateway-kind-e2e-controller.log >&2
        exit 1
    fi
    sleep 1
done
  printf "${GREEN}✓ CRD installed and Operator running${NC}\n\n"

  printf "${YELLOW}[4/9] Deploying Gateway with Endpoint discovery RBAC...${NC}\n"
cat <<EOF | kubectl --context "$CONTEXT" apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: gateway-kind-e2e
  namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gateway-kind-e2e-discovery
  namespace: $NAMESPACE
rules:
  - apiGroups: [""]
    resources: ["services", "endpoints"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gateway-kind-e2e-discovery
  namespace: $NAMESPACE
subjects:
  - kind: ServiceAccount
    name: gateway-kind-e2e
    namespace: $NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: gateway-kind-e2e-discovery
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway-kind-e2e
  namespace: $NAMESPACE
spec:
  replicas: 1
  selector:
    matchLabels:
      app: gateway-kind-e2e
  template:
    metadata:
      labels:
        app: gateway-kind-e2e
    spec:
      serviceAccountName: gateway-kind-e2e
      containers:
        - name: gateway
          image: gate-service:latest
          imagePullPolicy: IfNotPresent
          env:
            - name: KUBE_NAMESPACE
              value: $NAMESPACE
            - name: JWT_SECRET
              value: $JWT_SECRET
          ports:
            - containerPort: 8080
              name: http
---
apiVersion: v1
kind: Service
metadata:
  name: gateway-kind-e2e
  namespace: $NAMESPACE
spec:
  selector:
    app: gateway-kind-e2e
  ports:
    - port: 8080
      targetPort: http
EOF
kubectl --context "$CONTEXT" rollout status deployment/gateway-kind-e2e --timeout=120s
printf "${GREEN}✓ Gateway is ready${NC}\n\n"

printf "${YELLOW}[5/9] Creating CPU-only Mock InferenceService...${NC}\n"
kubectl --context "$CONTEXT" apply -f "$OPERATOR_DIR/config/samples/serving_v1_inferenceservice_mock.yaml"
kubectl --context "$CONTEXT" rollout status deployment/mock-service --timeout=120s
kubectl --context "$CONTEXT" get endpoints mock-service -o jsonpath='{.subsets[0].addresses[0].ip}' | grep -q .
printf "${GREEN}✓ Mock InferenceService and Endpoint are ready${NC}\n\n"

kubectl --context "$CONTEXT" port-forward service/gateway-kind-e2e "$GATEWAY_PORT:8080" > /tmp/gateway-kind-e2e-port-forward.log 2>&1 &
PORT_FORWARD_PID=$!

JWT="$(python3 - <<'PY'
import base64
import hashlib
import hmac
import json

def encode(value):
    return base64.urlsafe_b64encode(value).rstrip(b"=")

header = encode(b'{"alg":"HS256","typ":"JWT"}')
payload = encode(json.dumps({"userID": "kind-e2e"}, separators=(",", ":")).encode())
signed = header + b"." + payload
signature = encode(hmac.new(b"kind-e2e-jwt-secret", signed, hashlib.sha256).digest())
print((signed + b"." + signature).decode())
PY
)"

request_gateway() {
  curl --silent --show-error --fail --max-time 15 -N \
        -H "Authorization: Bearer $JWT" \
        -H "Content-Type: application/json" \
        --data '{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"hello"}]}' \
        "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions"
}

printf "${YELLOW}[6/9] Waiting for Gateway to discover the ready mock Endpoint...${NC}\n"
for _ in {1..45}; do
    if response="$(request_gateway 2>/tmp/gateway-kind-e2e-curl.err)" && grep -q 'data: ' <<<"$response"; then
        break
    fi
    sleep 1
done
if [[ -z "${response:-}" ]] || ! grep -q 'data: ' <<<"$response"; then
	printf "${RED}Gateway did not discover mock-model within 45 seconds${NC}\n" >&2
    cat /tmp/gateway-kind-e2e-curl.err >&2 || true
    kubectl --context "$CONTEXT" logs deployment/gateway-kind-e2e --tail=200 >&2 || true
    exit 1
fi
printf "${GREEN}✓ Gateway routed mock-model without restart${NC}\n\n"

printf "${YELLOW}[7/9] Deleting InferenceService and waiting for backend removal...${NC}\n"
kubectl --context "$CONTEXT" delete inferenceservice mock-service
kubectl --context "$CONTEXT" wait --for=delete deployment/mock-service --timeout=60s
printf "${GREEN}✓ Mock backend resources removed${NC}\n\n"

printf "${YELLOW}[8/9] Waiting for Gateway to remove the stale model backend...${NC}\n"
for _ in {1..45}; do
    status="$(curl --silent --output /tmp/gateway-kind-e2e-response.txt --write-out '%{http_code}' --max-time 15 \
        -H "Authorization: Bearer $JWT" \
        -H "Content-Type: application/json" \
        --data '{"model":"mock-model","stream":true,"messages":[{"role":"user","content":"hello"}]}' \
        "http://127.0.0.1:$GATEWAY_PORT/v1/chat/completions")"
    if [[ "$status" == "404" ]]; then
        break
    fi
    sleep 1
done
if [[ "${status:-}" != "404" ]]; then
	printf "${RED}Gateway did not remove mock-model within 45 seconds; last status: %s${NC}\n" "${status:-none}" >&2
    kubectl --context "$CONTEXT" logs deployment/gateway-kind-e2e --tail=200 >&2 || true
    exit 1
fi

printf "${GREEN}[9/9] PASS: Gateway discovered, proxied, and removed the mock backend without restart${NC}\n"