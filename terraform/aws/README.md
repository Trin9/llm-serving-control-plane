# Terraform AWS 环境

在 AWS 上创建**生产级 EKS 集群**及可选存储（FSx for Lustre）。**不部署应用**，应用由 Helm 部署。

## 本模块创建的资源

| 资源 | 说明 |
|:---|:---|
| EKS 集群 | 控制平面、CoreDNS、kube-proxy、VPC CNI、EBS CSI |
| 节点组 | CPU 节点组、GPU 节点组（可选 Spot） |
| FSx for Lustre（可选） | 大容量共享存储，用于模型等 |
| StorageClass / PV / PVC（可选） | 与 FSx 配套，供 Pod 挂载 |

**不包含**：vLLM Deployment、Gate Service、Ingress、HPA 等，请按 `terraform output next_steps` 使用 Helm 安装。

## 快速开始

```bash
cd terraform/aws

# 1. 配置 backend（S3）和变量
cat > terraform.tfvars <<EOF
vpc_id     = "vpc-xxxxx"
subnet_ids = ["subnet-xxxxx", "subnet-yyyyy"]
use_spot_instances = true
EOF

# 2. 部署基础设施
terraform init
terraform apply

# 3. 配置 kubectl 并按下一步安装 Helm 应用
terraform output kubeconfig_command
terraform output next_steps
```

## 下一步：用 Helm 部署应用

```bash
# 配置 kubectl
aws eks update-kubeconfig --region <region> --name <cluster_name>
kubectl get nodes

# 查看完整指引（安装 Operator、vLLM、Gate 等）
terraform output next_steps
```

按输出中的步骤安装 `helm/llm-operator`、创建 InferenceService、以及（可选）Gate Service 等。

## 输出说明

| 输出 | 说明 |
|:---|:---|
| `cluster_name`, `cluster_endpoint` | 集群信息 |
| `kubeconfig_command` | 配置 kubectl 的命令 |
| `fsx_info` | FSx 的 id、dns_name、mount_name（enable_fsx=true 时） |
| `storage_info` | 存储类、PV/PVC 名称、容量 |
| `next_steps` | Helm 部署步骤说明 |
| `cost_estimate` | 基础设施月成本估算 |

## 详细文档

- 变量说明、与 Helm 协作、FSx 使用：[使用指南](../../docs/terraform/usage-guide.md)
- 架构分层：[架构说明](../../docs/architecture.md)
