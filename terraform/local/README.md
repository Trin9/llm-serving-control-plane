# Terraform Local Environment

Use **Kind** (Kubernetes in Docker) to create a local development cluster and configure the **infrastructure**: cluster + storage (PV/PVC). **Applications are not deployed**; they are deployed with Helm.

## Resources Created by This Module

| Resource | Description |
|:---|:---|
| Kind cluster | 1 control-plane + 1 worker, port 80→8080 mapping |
| PV (model-pv) | hostPath, used to mount models or data |
| PVC (model-pvc) | default namespace, bound to the PV above |

**Not included**: applications such as Operator, vLLM, and Gate Service — install them with Helm following `terraform output next_steps`.

## Quick Start

```bash
cd terraform/local

terraform init
terraform apply
```

## Next Step: Deploy Applications with Helm

After the deployment completes, run:

```bash
# 1. Configure kubectl
export KUBECONFIG=$(terraform output -raw kubeconfig_path)
kubectl get nodes

# 2. View the full next steps (install the Operator, create InferenceService, etc.)
terraform output next_steps
```

Follow the output guidance in order, for example:

1. Install the LLM Operator: `helm install llm-operator ../../helm/llm-operator --wait`
2. Create a Mock InferenceService: `kubectl apply -f ../../operator/config/samples/serving_v1_inferenceservice_mock.yaml`
3. (Optional) Deploy the Gate Service: install it once the `helm/gate-service` chart is ready

## Outputs

| Output | Description |
|:---|:---|
| `cluster_name` | Kind cluster name |
| `kubeconfig_path` | Path to the kubeconfig file |
| `kubeconfig_command` | Command to export KUBECONFIG |
| `storage_info` | PV/PVC names, capacity, host_path |
| `next_steps` | Helm deployment step guide |

## Accessing Deployed Applications (requires Helm deployment per next_steps)

If you deployed the Gate Service with Helm, you can access it via port-forwarding:

```bash
kubectl port-forward service/gate-service-entry 8083:80
curl http://localhost:8083/health
```

If you only deployed the Operator + InferenceService, inspect the services and Pods:

```bash
kubectl get inferenceservices
kubectl get pods -l serving.trin.io/inferenceservice
```

## Troubleshooting

- **Kind or the API not ready**: the script waits up to about 2 minutes; if it fails, check the Docker and Kind versions.
- **Image pull failure**: application images must be available inside the cluster; with Kind you can load them from the host using `kind load docker-image <image>`.
- More details: [usage guide](../../docs/terraform/usage-guide.md), [local deployment troubleshooting](../../docs/terraform/troubleshooting-local-deploy.md) (if present).
