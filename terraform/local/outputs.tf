output "cluster_name" {
  description = "Name of the Kind cluster"
  value       = kind_cluster.local.name
}

output "kubeconfig_path" {
  description = "Path to the kubeconfig file"
  value       = kind_cluster.local.kubeconfig_path
}

output "kubeconfig_command" {
  description = "Command to configure kubectl"
  value       = "export KUBECONFIG=${kind_cluster.local.kubeconfig_path}"
}

output "storage_info" {
  description = "Storage configuration information"
  value = {
    pv_name        = kubernetes_persistent_volume.model_storage.metadata[0].name
    pvc_name       = kubernetes_persistent_volume_claim.model_storage.metadata[0].name
    storage_class  = "manual"
    capacity       = var.storage_size
    host_path      = var.host_path
  }
}

output "next_steps" {
  description = "Next steps to deploy applications using Helm"
  value       = <<-EOT
    ✅ Kubernetes cluster is ready!
    
    📋 Next Steps - Deploy Applications with Helm:
    
    1️⃣  Configure kubectl:
       export KUBECONFIG=${kind_cluster.local.kubeconfig_path}
       kubectl get nodes
    
    2️⃣  Install the LLM Operator:
       cd helm/llm-operator
       helm install llm-operator . --wait
       
       # Verify installation:
       kubectl get pods -l control-plane=controller-manager
       kubectl get crd inferenceservices.serving.trin.io
    
    3️⃣  Deploy InferenceService (Mock or Real vLLM):
       # For testing (Mock service):
       kubectl apply -f operator/config/samples/serving_v1_inferenceservice_mock.yaml
       
       # Verify deployment:
       kubectl get inferenceservices
       kubectl get pods -l serving.trin.io/inferenceservice
    
    4️⃣  Deploy Gate Service (API Gateway):
       cd helm/gate-service  # (to be created in future)
       helm install gate-service .
    
    📚 Documentation:
       - Helm Chart Guide: docs/subject/helm-chart-structure.md
       - Deployment Guide: docs/deployment-guide.md (to be created)
       - Operator README: operator/README.md
    
    💡 Tips:
       - Storage is ready: PV '${kubernetes_persistent_volume.model_storage.metadata[0].name}' bound to '${var.host_path}'
       - Use 'helm list' to see all installed releases
       - Use 'kubectl get all' to view all resources
  EOT
}
