# Terraform 基础设施

使用 Terraform 管理**基础设施层**：创建集群与存储，**不部署应用**。应用（Operator、Gate、vLLM）由 Helm 部署。

## 架构中的位置

本仓库采用分层架构（参见 [docs/architecture.md](../docs/architecture.md)）：

| 层次 | 工具 | 职责 | 本目录 |
|:---|:---|:---|:---|
| **1. 基础设施** | Terraform | 创建空集群、节点、存储（PV/PVC/FSx） | `local/`、`aws/` |
| **2. 应用部署** | Helm | 安装 Operator、Gate、业务服务 | `../helm/` |
| **3. 运行时** | Kubernetes | 调度 Pod、拉取镜像、运行容器 | — |
| **4. 业务逻辑** | Operator | 监听 CR、创建 vLLM Deployment 等 | `../operator/` |

**Terraform 只做「建房子」**：输出为空的 Kubernetes 集群 + 可选存储。应用部署请按 `terraform output next_steps` 的指引用 Helm 完成。

## 快速开始

### 本地环境（Kind）

```bash
cd local
terraform init
terraform apply
# 按输出 next_steps 配置 KUBECONFIG 并安装 Helm 应用
terraform output next_steps
```

### AWS 环境（EKS）

```bash
cd aws
# 配置 backend 与 terraform.tfvars（vpc_id, subnet_ids 等）
terraform init
terraform apply
# 按输出 next_steps 配置 kubectl 并安装 Helm 应用
terraform output next_steps
```

## 目录结构

```
terraform/
├── README.md           # 本文件
├── local/              # 本地开发：Kind 集群 + hostPath PV/PVC
│   ├── main.tf         # Kind 集群、wait_for_api、PV/PVC
│   ├── variables.tf
│   ├── outputs.tf      # kubeconfig_command, next_steps, storage_info
│   └── README.md
└── aws/                # 生产：EKS + 可选 FSx for Lustre
    ├── main.tf         # EKS 模块、FSx、StorageClass、PV/PVC
    ├── variables.tf
    ├── outputs.tf      # cluster_*, fsx_info, kubeconfig_command, next_steps
    └── README.md
```

## Terraform 管理的资源（仅基础设施）

- **local**：Kind 集群、API 就绪等待、PV（hostPath）、PVC。
- **aws**：EKS 集群与节点组（CPU/GPU）、可选 FSx for Lustre、StorageClass、PV、PVC。

**不包含**：Deployment、Service、Ingress、HPA 等应用资源，均由 Helm 或 Operator 管理。

## 与 Helm 的协作

1. `terraform apply` → 得到可用集群与存储。
2. `export KUBECONFIG=...` 或 `aws eks update-kubeconfig ...`。
3. 按 `terraform output next_steps` 执行：
   - 安装 LLM Operator：`helm install llm-operator ./helm/llm-operator`
   - 创建 InferenceService CR（或应用 Helm Chart）。
   - 按需安装 Gate Service 等。

详见：[使用指南](../docs/terraform/usage-guide.md) 中的「与 Helm 协作」章节。

## 常用命令

```bash
terraform init
terraform plan
terraform apply
terraform output              # 查看所有输出
terraform output next_steps   # 查看下一步（Helm 部署指引）
terraform destroy
```

## 文档

- **[使用指南](../docs/terraform/usage-guide.md)**：变量说明、与 Helm 协作、故障排除。
- **[架构说明](../docs/architecture.md)**：分层职责与数据流。
