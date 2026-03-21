#!/bin/bash
# Operator Kind 集群测试脚本
# 用于验证 Finalizer、OwnerReference、Status Conditions 等功能

set -e

CLUSTER_NAME="operator-test"
CONTROLLER_PID=""

# 颜色输出
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 清理函数
cleanup() {
    echo -e "\n${YELLOW}清理测试环境...${NC}"
    if [ ! -z "$CONTROLLER_PID" ]; then
        kill $CONTROLLER_PID 2>/dev/null || true
    fi
    kind delete cluster --name $CLUSTER_NAME 2>/dev/null || true
    echo -e "${GREEN}清理完成${NC}"
}

# 注册清理函数
trap cleanup EXIT

echo -e "${GREEN}=== Operator Kind 集群测试 ===${NC}\n"

# 1. 检查依赖
echo -e "${YELLOW}[1/10] 检查依赖...${NC}"
command -v kind >/dev/null 2>&1 || { echo -e "${RED}错误: kind 未安装${NC}"; exit 1; }
command -v kubectl >/dev/null 2>&1 || { echo -e "${RED}错误: kubectl 未安装${NC}"; exit 1; }
command -v kustomize >/dev/null 2>&1 || { echo -e "${RED}错误: kustomize 未安装${NC}"; exit 1; }
command -v go >/dev/null 2>&1 || { echo -e "${RED}错误: go 未安装${NC}"; exit 1; }
echo -e "${GREEN}✓ 依赖检查通过${NC}\n"

# 2. 编译代码
echo -e "${YELLOW}[2/10] 编译 Operator 代码...${NC}"
cd "$(dirname "$0")"
go build ./...
if [ $? -ne 0 ]; then
    echo -e "${RED}✗ 编译失败${NC}"
    exit 1
fi
echo -e "${GREEN}✓ 编译成功${NC}\n"

# 3. 生成 CRD 和 RBAC
echo -e "${YELLOW}[3/10] 生成 CRD 和 RBAC...${NC}"
if [ -f ~/go/bin/controller-gen ]; then
    CONTROLLER_GEN=~/go/bin/controller-gen
elif [ -f ~/.local/bin/controller-gen ]; then
    CONTROLLER_GEN=~/.local/bin/controller-gen
else
    echo -e "${RED}错误: controller-gen 未找到${NC}"
    exit 1
fi

$CONTROLLER_GEN rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases
echo -e "${GREEN}✓ CRD 和 RBAC 生成成功${NC}\n"

# 4. 创建/重置 Kind 集群
echo -e "${YELLOW}[4/10] 创建 Kind 集群...${NC}"
kind delete cluster --name $CLUSTER_NAME 2>/dev/null || true
kind create cluster --name $CLUSTER_NAME --wait 60s
echo -e "${GREEN}✓ 集群创建成功${NC}\n"

# 5. 安装 CRD
echo -e "${YELLOW}[5/10] 安装 CRD...${NC}"
KUSTOMIZE_CMD="kustomize"
if [ -f ~/.local/bin/kustomize ]; then
    KUSTOMIZE_CMD=~/.local/bin/kustomize
fi

$KUSTOMIZE_CMD build config/crd | kubectl apply -f -
kubectl wait --for condition=established --timeout=30s crd/inferenceservices.serving.trin.io
echo -e "${GREEN}✓ CRD 安装成功${NC}\n"

# 6. 启动 Controller（后台运行）
echo -e "${YELLOW}[6/10] 启动 Controller...${NC}"

# 检查并释放端口 8081
if lsof -ti:8081 >/dev/null 2>&1; then
    echo -e "  ${YELLOW}端口 8081 被占用，尝试释放...${NC}"
    lsof -ti:8081 | xargs kill -9 2>/dev/null || true
    sleep 2
fi

# 使用环境变量指定端口（如果 Controller 支持）
export METRICS_BIND_ADDRESS=:8081
go run ./cmd/main.go > /tmp/controller.log 2>&1 &
CONTROLLER_PID=$!
sleep 5  # 等待 Controller 启动

# 检查 Controller 是否运行
if ! ps -p $CONTROLLER_PID > /dev/null; then
    echo -e "${RED}✗ Controller 启动失败，查看日志:${NC}"
    cat /tmp/controller.log
    echo -e "\n${YELLOW}提示: 如果端口被占用，请手动执行: lsof -ti:8081 | xargs kill -9${NC}"
    exit 1
fi
echo -e "${GREEN}✓ Controller 运行中 (PID: $CONTROLLER_PID)${NC}\n"

# 7. 准备测试镜像（Mock 引擎）
echo -e "${YELLOW}[7/10] 准备测试镜像...${NC}"
docker pull python:3.9-alpine
kind load docker-image python:3.9-alpine --name $CLUSTER_NAME
echo -e "${GREEN}✓ 镜像加载成功${NC}\n"

# 8. 创建测试 CR
echo -e "${YELLOW}[8/10] 创建测试 InferenceService...${NC}"
TEST_NAME="test-service"

# 创建测试 YAML（如果 samples 目录没有 mock 版本）
cat > /tmp/test-inferenceservice.yaml <<EOF
apiVersion: serving.trin.io/v1
kind: InferenceService
metadata:
  name: $TEST_NAME
  namespace: default
spec:
  modelName: "test-model"
  engine: "mock"
  replicas: 1
EOF

kubectl apply -f /tmp/test-inferenceservice.yaml
echo -e "  ${YELLOW}等待 Controller Reconcile...${NC}"
sleep 5  # 等待 Reconcile 和资源创建
echo -e "${GREEN}✓ CR 创建成功${NC}\n"

# 9. 验证功能
echo -e "${YELLOW}[9/10] 验证功能...${NC}\n"

# 9.1 验证 Finalizer
echo -e "  ${YELLOW}9.1 验证 Finalizer...${NC}"
FINALIZERS=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.metadata.finalizers[*]}')
if [[ "$FINALIZERS" == *"serving.trin.io/finalizer"* ]]; then
    echo -e "  ${GREEN}✓ Finalizer 存在: $FINALIZERS${NC}"
else
    echo -e "  ${RED}✗ Finalizer 缺失${NC}"
    exit 1
fi

# 9.2 验证 OwnerReference
echo -e "  ${YELLOW}9.2 验证 OwnerReference...${NC}"
OWNER_REF=$(kubectl get deployment $TEST_NAME -o jsonpath='{.metadata.ownerReferences[0].kind}' 2>/dev/null || echo "")
if [ "$OWNER_REF" == "InferenceService" ]; then
    echo -e "  ${GREEN}✓ OwnerReference 设置正确${NC}"
else
    echo -e "  ${RED}✗ OwnerReference 未设置或错误${NC}"
    exit 1
fi

# 9.3 验证 Status Conditions
echo -e "  ${YELLOW}9.3 验证 Status Conditions...${NC}"
CONDITION_TYPE=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.status.conditions[0].type}' 2>/dev/null || echo "")
CONDITION_STATUS=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.status.conditions[0].status}' 2>/dev/null || echo "")
if [ "$CONDITION_TYPE" == "Available" ]; then
    echo -e "  ${GREEN}✓ Condition 类型正确: $CONDITION_TYPE${NC}"
    echo -e "  ${GREEN}  Status: $CONDITION_STATUS${NC}"
else
    echo -e "  ${RED}✗ Condition 未设置或类型错误${NC}"
    exit 1
fi

# 9.4 验证 Deployment 创建
echo -e "  ${YELLOW}9.4 验证 Deployment...${NC}"
DEPLOYMENT_READY=$(kubectl get deployment $TEST_NAME -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
if [ "$DEPLOYMENT_READY" == "1" ]; then
    echo -e "  ${GREEN}✓ Deployment Ready: $DEPLOYMENT_READY/1${NC}"
else
    echo -e "  ${YELLOW}⚠ Deployment 还在启动中: $DEPLOYMENT_READY/1${NC}"
    echo -e "  ${YELLOW}  等待 30 秒后重试...${NC}"
    sleep 30
    DEPLOYMENT_READY=$(kubectl get deployment $TEST_NAME -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "$DEPLOYMENT_READY" == "1" ]; then
        echo -e "  ${GREEN}✓ Deployment Ready: $DEPLOYMENT_READY/1${NC}"
    else
        echo -e "  ${RED}✗ Deployment 未就绪${NC}"
        kubectl describe deployment $TEST_NAME
        exit 1
    fi
fi

# 9.5 验证 Service 创建
echo -e "  ${YELLOW}9.5 验证 Service...${NC}"
SERVICE_EXISTS=$(kubectl get service $TEST_NAME -o name 2>/dev/null || echo "")
if [ ! -z "$SERVICE_EXISTS" ]; then
    echo -e "  ${GREEN}✓ Service 创建成功${NC}"
else
    echo -e "  ${RED}✗ Service 未创建${NC}"
    exit 1
fi

echo -e "\n${GREEN}✓ 所有功能验证通过${NC}\n"

# 10. 测试删除（验证 Finalizer 和级联删除）
echo -e "${YELLOW}[10/10] 测试删除流程...${NC}\n"

echo -e "  ${YELLOW}删除 InferenceService...${NC}"
kubectl delete inferenceservice $TEST_NAME

# 检查删除流程（Controller 可能处理很快，所以需要立即检查）
sleep 1
DELETION_TIMESTAMP=$(kubectl get inferenceservice $TEST_NAME -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null || echo "")
EXISTS=$(kubectl get inferenceservice $TEST_NAME 2>/dev/null || echo "")

if [ ! -z "$DELETION_TIMESTAMP" ]; then
    # 进入了 Terminating 状态
    echo -e "  ${GREEN}✓ CR 进入 Terminating 状态（Finalizer 生效）${NC}"
    echo -e "  ${YELLOW}等待 Finalizer 处理完成...${NC}"
    
    # 等待 Finalizer 处理完成（最多 20 秒）
    for i in {1..10}; do
        sleep 2
        EXISTS=$(kubectl get inferenceservice $TEST_NAME 2>/dev/null || echo "")
        if [ -z "$EXISTS" ]; then
            echo -e "  ${GREEN}✓ CR 已删除（Finalizer 处理完成）${NC}"
            break
        fi
        if [ $i -eq 10 ]; then
            echo -e "  ${RED}✗ CR 删除超时（可能 Finalizer 未正确移除）${NC}"
            exit 1
        fi
    done
elif [ -z "$EXISTS" ]; then
    # Controller 处理太快，CR 已经删除（这也是正常的）
    echo -e "  ${GREEN}✓ CR 已删除（Finalizer 快速处理完成）${NC}"
    echo -e "  ${YELLOW}  (Controller 处理速度很快，这是正常的)${NC}"
else
    # 既没有 deletionTimestamp，也没有被删除（异常）
    echo -e "  ${RED}✗ CR 删除异常${NC}"
    kubectl get inferenceservice $TEST_NAME -o yaml
    exit 1
fi

# 验证级联删除（Garbage Collector 可能需要一些时间）
echo -e "  ${YELLOW}验证级联删除（等待 GC 处理）...${NC}"
for i in {1..15}; do
    sleep 2
    DEPLOYMENT_EXISTS=$(kubectl get deployment $TEST_NAME 2>/dev/null || echo "")
    SERVICE_EXISTS=$(kubectl get service $TEST_NAME 2>/dev/null || echo "")
    
    if [ -z "$DEPLOYMENT_EXISTS" ] && [ -z "$SERVICE_EXISTS" ]; then
        echo -e "  ${GREEN}✓ Deployment 和 Service 已自动删除（OwnerReference 生效）${NC}"
        break
    fi
    
    if [ $i -eq 15 ]; then
        echo -e "  ${RED}✗ 级联删除超时（30秒后仍存在）${NC}"
        [ ! -z "$DEPLOYMENT_EXISTS" ] && echo -e "    Deployment 仍存在"
        [ ! -z "$SERVICE_EXISTS" ] && echo -e "    Service 仍存在"
        echo -e "  ${YELLOW}检查 OwnerReference:${NC}"
        kubectl get deployment $TEST_NAME -o jsonpath='{.metadata.ownerReferences[*].kind}' 2>/dev/null || echo "  Deployment 不存在"
        exit 1
    fi
done

echo -e "\n${GREEN}=== 所有测试通过 ===${NC}\n"
echo -e "${GREEN}测试摘要:${NC}"
echo -e "  ✓ Finalizer 功能正常"
echo -e "  ✓ OwnerReference 级联删除正常"
echo -e "  ✓ Status Conditions 更新正常"
echo -e "  ✓ Deployment 和 Service 创建正常"
