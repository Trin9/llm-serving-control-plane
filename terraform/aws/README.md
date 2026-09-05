# Terraform AWS Environment

Create a **production-grade EKS cluster** on AWS plus optional storage (FSx for Lustre). **Applications are not deployed**; they are deployed with Helm.

## Resources Created by This Module

| Resource | Description |
|:---|:---|
| EKS cluster | Control plane, CoreDNS, kube-proxy, VPC CNI, EBS CSI |
| Node groups | CPU node group, GPU node group (optional Spot) |
| FSx for Lustre (optional) | Large shared storage, e.g. for models |
| StorageClass / PV / PVC (optional) | Pairs with FSx, mounted by Pods |

**Not included**: vLLM Deployment, Gate Service, Ingress, HPA, etc. — install them with Helm following `terraform output next_steps`.

## Quick Start

```bash
cd terraform/aws

# 1. Configure the backend (S3) and variables
cat > terraform.tfvars <<EOF
vpc_id     = "vpc-xxxxx"
subnet_ids = ["subnet-xxxxx", "subnet-yyyyy"]
use_spot_instances = true
EOF

# 2. Deploy the infrastructure
terraform init
terraform apply

# 3. Configure kubectl and install Helm applications per the next steps
terraform output kubeconfig_command
terraform output next_steps
```

## Next Step: Deploy Applications with Helm

```bash
# Configure kubectl
aws eks update-kubeconfig --region <region> --name <cluster_name>
kubectl get nodes

# View the full guide (install Operator, vLLM, Gate, etc.)
terraform output next_steps
```

Follow the steps in the output to install `helm/llm-operator`, create InferenceService, and (optionally) the Gate Service, etc.

## Outputs

| Output | Description |
|:---|:---|
| `cluster_name`, `cluster_endpoint` | Cluster information |
| `kubeconfig_command` | Command to configure kubectl |
| `fsx_info` | FSx id, dns_name, mount_name (when enable_fsx=true) |
| `storage_info` | Storage class, PV/PVC names, capacity |
| `next_steps` | Helm deployment step guide |
| `cost_estimate` | Estimated monthly infrastructure cost |

## Detailed Documentation

- Variables, Helm collaboration, FSx usage: [usage guide](../../docs/terraform/usage-guide.md)
- Architecture layers: [architecture](../../docs/architecture.md)
