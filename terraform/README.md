# Terraform Infrastructure

Use Terraform to manage the **infrastructure layer**: creating clusters and storage, **not deploying applications**. Applications (Operator, Gate, vLLM) are deployed with Helm.

## Position in the Architecture

This repository follows a layered architecture (see [docs/architecture.md](../docs/architecture.md)):

| Layer | Tool | Responsibility | This directory |
|:---|:---|:---|:---|
| **1. Infrastructure** | Terraform | Create empty clusters, nodes, storage (PV/PVC/FSx) | `local/`, `aws/` |
| **2. Application deployment** | Helm | Install Operator, Gate, business services | `../helm/` |
| **3. Runtime** | Kubernetes | Schedule Pods, pull images, run containers | — |
| **4. Business logic** | Operator | Watch CRs, create vLLM Deployments, etc. | `../operator/` |

**Terraform only "builds the house"**: its output is an empty Kubernetes cluster + optional storage. For application deployment, follow the `terraform output next_steps` guidance using Helm.

## Quick Start

### Local environment (Kind)

```bash
cd local
terraform init
terraform apply
# Configure KUBECONFIG per the next_steps output and install Helm applications
terraform output next_steps
```

### AWS environment (EKS)

```bash
cd aws
# Configure the backend and terraform.tfvars (vpc_id, subnet_ids, etc.)
terraform init
terraform apply
# Configure kubectl per the next_steps output and install Helm applications
terraform output next_steps
```

## Directory Structure

```
terraform/
├── README.md           # this file
├── local/              # local development: Kind cluster + hostPath PV/PVC
│   ├── main.tf         # Kind cluster, wait_for_api, PV/PVC
│   ├── variables.tf
│   ├── outputs.tf      # kubeconfig_command, next_steps, storage_info
│   └── README.md
└── aws/                # production: EKS + optional FSx for Lustre
    ├── main.tf         # EKS module, FSx, StorageClass, PV/PVC
    ├── variables.tf
    ├── outputs.tf      # cluster_*, fsx_info, kubeconfig_command, next_steps
    └── README.md
```

## Resources Managed by Terraform (infrastructure only)

- **local**: Kind cluster, API readiness wait, PV (hostPath), PVC.
- **aws**: EKS cluster and node groups (CPU/GPU), optional FSx for Lustre, StorageClass, PV, PVC.

**Not included**: application resources such as Deployment, Service, Ingress, and HPA — these are managed by Helm or the Operator.

## Collaboration with Helm

1. `terraform apply` → get a usable cluster and storage.
2. `export KUBECONFIG=...` or `aws eks update-kubeconfig ...`.
3. Follow `terraform output next_steps`:
   - Install the LLM Operator: `helm install llm-operator ./helm/llm-operator`
   - Create an InferenceService CR (or apply the Helm chart).
   - Install the Gate Service etc. as needed.

See the "Collaboration with Helm" section in the [usage guide](../docs/terraform/usage-guide.md).

## Common Commands

```bash
terraform init
terraform plan
terraform apply
terraform output              # view all outputs
terraform output next_steps   # view the next steps (Helm deployment guide)
terraform destroy
```

## Documentation

- **[Usage guide](../docs/terraform/usage-guide.md)**: variable reference, Helm collaboration, troubleshooting.
- **[Architecture](../docs/architecture.md)**: layer responsibilities and data flow.
