#!/bin/bash
# Operator Kind cluster test script
# Verifies features such as Finalizer, OwnerReference, and Status Conditions

set -e

CLUSTER_NAME="operator-test"
CONTROLLER_PID=""

# Color output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Cleanup function
cleanup() {
    echo -e "\n${YELLOW}Cleaning up test environment...${NC}"
    if [ ! -z "$CONTROLLER_PID" ]; then
        kill $CONTROLLER_PID 2>/dev/null || true
    fi
    kind delete cluster --name $CLUSTER_NAME 2>/dev/null || true
    echo -e "${GREEN}Cleanup complete${NC}"
}

# Register the cleanup function
trap cleanup EXIT

echo -e "${GREEN}=== Operator Kind Cluster Test ===${NC}\n"

# 1. Check dependencies
echo -e "${YELLOW}[1/10] Checking dependencies...${NC}"
command -v kind >/dev/null 2>&1 || { echo -e "${RED}Error: kind is not installed${NC}"; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo -e "${RED}Error: kubectl is not installed${NC}"; exit 1; }
command -v kustomize >/dev/null 2>&1 || { echo -e "${RED}Error: kustomize is not installed${NC}"; exit 1; }
command -v go >/dev/null 2>&1 || { echo -e "${RED}Error: go is not installed${NC}"; exit 1; }
echo -e "${GREEN}✓ Dependency check passed${NC}\n"

# 2. Compile the code
echo -e "${YELLOW}[2/10] Compiling Operator code...${NC}"
cd "$(dirname "$0")"
GOTOOLCHAIN=auto go build ./...
if [ $? -ne 0 ]; then
    echo -e "${RED}✗ Compilation failed${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Compilation succeeded${NC}\n"

# 3. Generate CRDs and RBAC
echo -e "${YELLOW}[3/10] Generating CRDs and RBAC...${NC}"
if [ -f ~/go/bin/controller-gen ]; then
    CONTROLLER_GEN=~/go/bin/controller-gen
elif [ -f ~/.local/bin/controller-gen ]; then
    CONTROLLER_GEN=~/.local/bin/controller-gen
else
    echo -e "${RED}Error: controller-gen not found${NC}"
    exit 1
fi

$CONTROLLER_GEN rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
echo -e "${GREEN}✓ CRD and RBAC generated successfully${NC}\n"

# 4. Create/reset the Kind cluster
echo -e "${YELLOW}[4/10] Creating Kind cluster...${NC}"
kind delete cluster --name $CLUSTER_NAME 2>/dev/null || true
kind create cluster --name $CLUSTER_NAME --wait 60s
echo -e "${GREEN}✓ Cluster created successfully${NC}\n"

# 5. Install CRDs
echo -e "${YELLOW}[5/10] Installing CRDs...${NC}"
KUSTOMIZE_CMD="kustomize"
if [ -f ~/.local/bin/kustomize ]; then
    KUSTOMIZE_CMD=~/.local/bin/kustomize
fi

$KUSTOMIZE_CMD build config/crd | kubectl apply -f -
kubectl wait --for condition=established --timeout=30s crd/inferenceservices.serving.trin.io
echo -e "${GREEN}✓ CRDs installed successfully${NC}\n"

# 6. Start the Controller (running in the background)
echo -e "${YELLOW}[6/10] Starting Controller...${NC}"

# Check and free port 8081
if lsof -ti:8081 >/dev/null 2>&1; then
    echo -e "  ${YELLOW}Port 8081 is in use, attempting to free it...${NC}"
    lsof -ti:8081 | xargs kill -9 2>/dev/null || true
    sleep 2
fi

# Set the port via environment variable (if the Controller supports it)
export METRICS_BIND_ADDRESS=:8081
GOTOOLCHAIN=auto go run ./cmd/main.go > /tmp/controller.log 2>&1 &
CONTROLLER_PID=$!
sleep 5  # wait for the Controller to start

# Check whether the Controller is running
if ! ps -p $CONTROLLER_PID > /dev/null; then
    echo -e "${RED}✗ Controller failed to start, check the log:${NC}"
    cat /tmp/controller.log
    echo -e "\n${YELLOW}Hint: if the port is occupied, run manually: lsof -ti:8081 | xargs kill -9${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Controller is running (PID: $CONTROLLER_PID)${NC}\n"

# 7. Build and load the test image (Mock engine)
echo -e "${YELLOW}[7/10] Preparing test image...${NC}"
docker build -t mock-vllm:latest -f ../Dockerfile.mock ..
kind load docker-image mock-vllm:latest --name $CLUSTER_NAME
echo -e "${GREEN}✓ Image loaded successfully${NC}\n"

# 8. Create the test CR
echo -e "${YELLOW}[8/10] Creating test InferenceService...${NC}"
TEST_NAME="mock-service"
kubectl apply -f config/samples/serving_v1_inferenceservice_mock.yaml
echo -e "  ${YELLOW}Waiting for Controller reconcile...${NC}"
sleep 5  # wait for reconcile and resource creation
echo -e "${GREEN}✓ CR created successfully${NC}\n"

# 9. Verify functionality
echo -e "${YELLOW}[9/10] Verifying functionality...${NC}\n"

# 9.1 Verify the Finalizer
echo -e "  ${YELLOW}9.1 Verifying Finalizer...${NC}"
FINALIZERS=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.metadata.finalizers[*]}')
if [[ "$FINALIZERS" == *"serving.trin.io/finalizer"* ]]; then
    echo -e "  ${GREEN}✓ Finalizer present: $FINALIZERS${NC}"
else
    echo -e "  ${RED}✗ Finalizer missing${NC}"
    exit 1
fi

# 9.2 Verify the OwnerReference
echo -e "  ${YELLOW}9.2 Verifying OwnerReference...${NC}"
OWNER_REF=$(kubectl get deployment $TEST_NAME -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null || echo "")
if [ "$OWNER_REF" == "InferenceService" ]; then
    echo -e "  ${GREEN}✓ OwnerReference set correctly${NC}"
else
    echo -e "  ${RED}✗ OwnerReference not set or incorrect${NC}"
    exit 1
fi

# 9.3 Verify the Status Conditions
echo -e "  ${YELLOW}9.3 Verifying Status Conditions...${NC}"
CONDITION_TYPE=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.status.conditions[0].type}' 2>/dev/null || echo "")
CONDITION_STATUS=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.status.conditions[0].status}' 2>/dev/null || echo "")
if [ "$CONDITION_TYPE" == "Available" ]; then
    echo -e "  ${GREEN}✓ Condition type correct: $CONDITION_TYPE${NC}"
    echo -e "  ${GREEN}  Status: $CONDITION_STATUS${NC}"
else
    echo -e "  ${RED}✗ Condition not set or wrong type${NC}"
    exit 1
fi

# 9.4 Verify the Deployment was created
echo -e "  ${YELLOW}9.4 Verifying Deployment...${NC}"
if kubectl rollout status deployment/$TEST_NAME --timeout=120s; then
    DEPLOYMENT_READY=$(kubectl get deployment $TEST_NAME -o jsonpath='{.status.readyReplicas}')
    echo -e "  ${GREEN}✓ Deployment Ready: $DEPLOYMENT_READY/1${NC}"
else
    echo -e "  ${RED}✗ Deployment not ready within 120 seconds${NC}"
    kubectl describe deployment $TEST_NAME
    kubectl describe pod -l serving.trin.io/inferenceservice=$TEST_NAME || true
    kubectl logs -l serving.trin.io/inferenceservice=$TEST_NAME --all-containers --tail=100 || true
    exit 1
fi

# 9.5 Verify the Service was created
echo -e "  ${YELLOW}9.5 Verifying Service...${NC}"
SERVICE_EXISTS=$(kubectl get service $TEST_NAME -o name 2>/dev/null || echo "")
if [ ! -z "$SERVICE_EXISTS" ]; then
    echo -e "  ${GREEN}✓ Service created successfully${NC}"
else
    echo -e "  ${RED}✗ Service not created${NC}"
    exit 1
fi

echo -e "\n${GREEN}✓ All feature checks passed${NC}\n"

# 10. Test deletion (verifies Finalizer and cascading deletion)
echo -e "${YELLOW}[10/10] Testing deletion flow...${NC}\n"

echo -e "  ${YELLOW}Deleting InferenceService...${NC}"
kubectl delete inferenceservice $TEST_NAME

# Check the deletion flow (the Controller may finish quickly, so check immediately)
sleep 1
DELETION_TIMESTAMP=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || echo "")
EXISTS=$(kubectl get inferenceservice $TEST_NAME 2>/dev/null || echo "")

if [ ! -z "$DELETION_TIMESTAMP" ]; then
    # Entered the Terminating state
    echo -e "  ${GREEN}✓ CR entered Terminating state (Finalizer active)${NC}"
    echo -e "  ${YELLOW}Waiting for the Finalizer to finish...${NC}"
    
    # Wait for the Finalizer to finish (up to 20 seconds)
    for i in {1..10}; do
        sleep 2
        EXISTS=$(kubectl get inferenceservice $TEST_NAME 2>/dev/null || echo "")
        if [ -z "$EXISTS" ]; then
            echo -e "  ${GREEN}✓ CR deleted (Finalizer handled)${NC}"
            break
        fi
        if [ $i -eq 10 ]; then
            echo -e "  ${RED}✗ CR deletion timed out (Finalizer may not have been removed)${NC}"
            exit 1
        fi
    done
elif [ -z "$EXISTS" ]; then
    # The Controller handled it too fast and the CR is already gone (this is also normal)
    echo -e "  ${GREEN}✓ CR deleted (Finalizer handled quickly)${NC}"
    echo -e "  ${YELLOW}  (The Controller handled it very fast, which is normal)${NC}"
else
    # Neither a deletionTimestamp nor deletion happened (unexpected)
    echo -e "  ${RED}✗ CR deletion failed${NC}"
    kubectl get inferenceservice $TEST_NAME -o yaml
    exit 1
fi

# Verify cascading deletion (the Garbage Collector may need some time)
echo -e "  ${YELLOW}Verifying cascading deletion (waiting for GC)...${NC}"
for i in {1..15}; do
    sleep 2
    DEPLOYMENT_EXISTS=$(kubectl get deployment $TEST_NAME 2>/dev/null || echo "")
    SERVICE_EXISTS=$(kubectl get service $TEST_NAME 2>/dev/null || echo "")
    
    if [ -z "$DEPLOYMENT_EXISTS" ] && [ -z "$SERVICE_EXISTS" ]; then
        echo -e "  ${GREEN}✓ Deployment and Service auto-deleted (OwnerReference active)${NC}"
        break
    fi
    
    if [ $i -eq 15 ]; then
        echo -e "  ${RED}✗ Cascading deletion timed out (still present after 30s)${NC}"
        [ ! -z "$DEPLOYMENT_EXISTS" ] && echo -e "    Deployment still exists"
        [ ! -z "$SERVICE_EXISTS" ] && echo -e "    Service still exists"
        echo -e "  ${YELLOW}Check OwnerReference:${NC}"
        kubectl get deployment $TEST_NAME -o jsonpath='{.metadata.ownerReferences[*].kind}' 2>/dev/null || echo "  Deployment does not exist"
        exit 1
    fi
done

echo -e "\n${GREEN}=== All tests passed ===${NC}\n"
echo -e "${GREEN}Test summary:${NC}"
echo -e "  ✓ Finalizer works correctly"
echo -e "  ✓ OwnerReference cascading deletion works"
echo -e "  ✓ Status Conditions update correctly"
echo -e "  ✓ Deployment and Service created correctly"
