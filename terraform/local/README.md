# Terraform 本地环境

使用 **Kind**（Kubernetes in Docker）创建本地开发集群，并配置**基础设施**：集群 + 存储（PV/PVC）。**不部署应用**，应用由 Helm 部署。

## 本模块创建的资源

| 资源 | 说明 |
|:---|:---|
| Kind 集群 | 1 control-plane + 1 worker，端口 80→8080 映射 |
| PV (model-pv) | hostPath，用于模型或数据挂载 |
| PVC (model-pvc) | default 命名空间，绑定上述 PV |

**不包含**：Operator、vLLM、Gate Service 等应用，请按 `terraform output next_steps` 使用 Helm 安装。

## 快速开始

```bash
cd terraform/local

terraform init
terraform apply
```

## 下一步：用 Helm 部署应用

部署完成后执行：

```bash
# 1. 配置 kubectl
export KUBECONFIG=$(terraform output -raw kubeconfig_path)
kubectl get nodes

# 2. 查看完整下一步（安装 Operator、创建 InferenceService 等）
terraform output next_steps
```

按输出指引依次执行即可，例如：

1. 安装 LLM Operator：`helm install llm-operator ../../helm/llm-operator --wait`
2. 创建 Mock InferenceService：`kubectl apply -f ../../operator/config/samples/serving_v1_inferenceservice_mock.yaml`
3. （可选）部署 Gate Service：待 `helm/gate-service` Chart 就绪后安装

## 输出说明

| 输出 | 说明 |
|:---|:---|
| `cluster_name` | Kind 集群名称 |
| `kubeconfig_path` | kubeconfig 文件路径 |
| `kubeconfig_command` | 导出 KUBECONFIG 的命令 |
| `storage_info` | PV/PVC 名称、capacity、host_path |
| `next_steps` | Helm 部署步骤说明 |

## 访问已部署的应用（需先按 next_steps 用 Helm 部署）

若已通过 Helm 部署了 Gate Service，可端口转发访问：

```bash
kubectl port-forward service/gate-service-entry 8083:80
curl http://localhost:8083/health
```

若仅部署了 Operator + InferenceService，可查看服务与 Pod：

```bash
kubectl get inferenceservices
kubectl get pods -l serving.trin.io/inferenceservice
```

## 故障排除

- **Kind 或 API 未就绪**：脚本会等待最多约 2 分钟，若失败请检查 Docker 与 Kind 版本。
- **镜像拉取失败**：应用镜像需在集群内可用；Kind 可使用 `kind load docker-image <image>` 从宿主机加载。
- 更多说明见：[使用指南](../../docs/terraform/usage-guide.md)、[本地部署故障排除](../../docs/terraform/troubleshooting-local-deploy.md)（若存在）。
