# EKS 集群输出
output "cluster_id" {
  description = "EKS cluster ID"
  value       = module.eks.cluster_id
}

output "cluster_name" {
  description = "EKS cluster name"
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Endpoint for EKS control plane"
  value       = module.eks.cluster_endpoint
}

output "cluster_security_group_id" {
  description = "Security group ID attached to the EKS cluster"
  value       = module.eks.cluster_security_group_id
}

output "cluster_iam_role_arn" {
  description = "IAM role ARN of the EKS cluster"
  value       = module.eks.cluster_iam_role_arn
}

output "cluster_certificate_authority_data" {
  description = "Base64 encoded certificate data required to communicate with the cluster"
  value       = module.eks.cluster_certificate_authority_data
  sensitive   = true
}

# 节点组输出
output "cpu_node_group_id" {
  description = "CPU node group ID"
  value       = module.eks.eks_managed_node_groups["cpu_nodes"].node_group_id
}

output "gpu_node_group_id" {
  description = "GPU node group ID"
  value       = module.eks.eks_managed_node_groups["gpu_nodes"].node_group_id
}

# FSx 输出
output "fsx_info" {
  description = "FSx file system information"
  value = var.enable_fsx ? {
    id         = aws_fsx_lustre_file_system.model_storage[0].id
    dns_name   = aws_fsx_lustre_file_system.model_storage[0].dns_name
    mount_name = aws_fsx_lustre_file_system.model_storage[0].mount_name
    mount_path = "/fsx"
  } : null
}

output "storage_info" {
  description = "Storage configuration information"
  value = var.enable_fsx ? {
    storage_class  = "fsx-sc"
    pv_name        = "model-pv"
    pvc_name       = "model-pvc"
    capacity       = "${var.fsx_storage_capacity}Gi"
    type           = "FSx for Lustre"
  } : {
    storage_class  = "gp2"
    type           = "EBS"
    note           = "FSx disabled, use EBS-backed PV/PVC for storage"
  }
}

# 配置命令输出
output "kubeconfig_command" {
  description = "Command to configure kubectl"
  value       = "aws eks update-kubeconfig --region ${var.aws_region} --name ${module.eks.cluster_name}"
}

output "next_steps" {
  description = "Next steps to deploy applications using Helm"
  value       = <<-EOT
    ✅ EKS Cluster is ready!
    
    📋 Next Steps - Deploy Applications with Helm:
    
    1️⃣  Configure kubectl:
       aws eks update-kubeconfig --region ${var.aws_region} --name ${module.eks.cluster_name}
       kubectl get nodes
       
       # Verify GPU nodes (if enabled):
       kubectl get nodes -l nvidia.com/gpu=true
    
    2️⃣  Install the LLM Operator:
       cd helm/llm-operator
       helm install llm-operator . \
         --set image.repository=<your-ecr-repo>/llm-operator \
         --set image.tag=v1.0.0 \
         --set replicaCount=2 \
         --wait
       
       # Verify installation:
       kubectl get pods -l control-plane=controller-manager
       kubectl get crd inferenceservices.serving.trin.io
    
    3️⃣  Deploy vLLM InferenceService:
       # Create custom values file for vLLM:
       cat > vllm-values.yaml <<EOF
       apiVersion: serving.trin.io/v1
       kind: InferenceService
       metadata:
         name: vllm-qwen
       spec:
         model: Qwen/Qwen1.5-4B-Chat
         replicas: 1
         image: vllm/vllm-openai:latest
         resources:
           requests:
             nvidia.com/gpu: 1
             memory: 8Gi
           limits:
             nvidia.com/gpu: 1
             memory: 16Gi
         storage:
           volumeClaim: model-pvc
           mountPath: /mnt/qwen-models
       EOF
       
       kubectl apply -f vllm-values.yaml
       
       # Verify deployment:
       kubectl get inferenceservices vllm-qwen
       kubectl get pods -l serving.trin.io/inferenceservice=vllm-qwen
    
    4️⃣  Deploy Gate Service (API Gateway):
       # Create Helm values for Gate Service:
       cat > gate-values.yaml <<EOF
       replicaCount: 2
       image:
         repository: <your-ecr-repo>/gate-service
         tag: latest
       
       env:
         VLLM_URL: http://vllm-qwen-service.default.svc.cluster.local:8000/v1/chat/completions
         ENVIRONMENT: production
       
       ingress:
         enabled: true
         className: alb
         annotations:
           alb.ingress.kubernetes.io/scheme: internet-facing
           alb.ingress.kubernetes.io/target-type: ip
         hosts:
           - host: api.example.com
             paths:
               - path: /
                 pathType: Prefix
       
       hpa:
         enabled: true
         minReplicas: 2
         maxReplicas: 10
         targetCPUUtilizationPercentage: 70
       EOF
       
       cd helm/gate-service  # (to be created)
       helm install gate-service . -f gate-values.yaml
    
    5️⃣  Verify Deployment:
       # Check all resources:
       kubectl get all
       kubectl get inferenceservices
       
       # Check ALB Ingress (wait for provisioning):
       kubectl get ingress gate-ingress
       
       # Test via port-forward (alternative):
       kubectl port-forward service/gate-service 8080:80
       curl http://localhost:8080/health
    
    📚 Documentation:
       - Helm Chart Guide: docs/subject/helm-chart-structure.md
       - Deployment Guide: docs/deployment-guide.md (to be created)
       - FSx Integration: docs/terraform/usage-guide.md
    
    💡 Storage Information:
       ${var.enable_fsx ? "- FSx for Lustre: ${aws_fsx_lustre_file_system.model_storage[0].dns_name}\n       - Mount name: ${aws_fsx_lustre_file_system.model_storage[0].mount_name}\n       - PVC available: model-pvc" : "- FSx disabled, use EBS-backed PVC for storage"}
    
    💰 Cost Optimization Tips:
       - Set GPU node group min_size=0 to scale down when idle
       - Use Spot instances for non-critical workloads
       - Monitor with: kubectl top nodes && kubectl top pods
  EOT
}

# 成本估算提示
output "cost_estimate" {
  description = "Estimated monthly cost breakdown"
  value       = <<-EOT
    ================================
    Estimated Monthly Costs (USD)
    ================================
    
    EKS Control Plane: ~$73/month
    
    CPU Nodes (${var.cpu_desired_size}x ${var.cpu_instance_types[0]}):
      - On-Demand: ~$30/node/month
      - Spot: ~$10/node/month
      - Total: ~${var.use_spot_instances ? var.cpu_desired_size * 10 : var.cpu_desired_size * 30}
    
    GPU Nodes (${var.gpu_desired_size}x ${var.gpu_instance_types[0]}):
      - On-Demand: ~$525/node/month
      - Spot: ~$157/node/month
      - Total: ~${var.use_spot_instances ? var.gpu_desired_size * 157 : var.gpu_desired_size * 525}
    
    ${var.enable_fsx ? "FSx for Lustre (${var.fsx_storage_capacity}GB):\n      - Storage: ~$0.14/GB/month\n      - Total: ~${var.fsx_storage_capacity * 0.14}" : "FSx: Disabled"}
    
    ALB: ~$23/month (after deploying Gate Service via Helm)
    Data Transfer: Variable
    
    Estimated Total (infra only): ~$${73 + (var.use_spot_instances ? var.cpu_desired_size * 10 : var.cpu_desired_size * 30) + (var.use_spot_instances ? var.gpu_desired_size * 157 : var.gpu_desired_size * 525) + (var.enable_fsx ? var.fsx_storage_capacity * 0.14 : 0)}/month
    (+ ALB when you deploy Gate with Helm)
    
    💡 Tip: Use Spot instances to save ~70% on compute costs!
    💡 Tip: Set GPU min_size=0 to scale down when not in use
  EOT
}
