# AWS 基础配置
variable "aws_region" {
  description = "AWS region"
  type        = string
  default     = "ap-northeast-1"
}

variable "environment" {
  description = "Environment name (dev, staging, prod)"
  type        = string
  default     = "production"
}

# EKS 集群配置
variable "cluster_name" {
  description = "Name of the EKS cluster"
  type        = string
  default     = "llm-inference-cluster"
}

variable "cluster_version" {
  description = "Kubernetes version for EKS cluster"
  type        = string
  default     = "1.28"
}

variable "vpc_id" {
  description = "VPC ID where EKS cluster will be created"
  type        = string
}

variable "subnet_ids" {
  description = "List of subnet IDs for EKS cluster"
  type        = list(string)
}

# CPU 节点组配置
variable "cpu_instance_types" {
  description = "Instance types for CPU node group"
  type        = list(string)
  default     = ["t3.medium", "t3a.medium"]
}

variable "cpu_min_size" {
  description = "Minimum number of CPU nodes"
  type        = number
  default     = 1
}

variable "cpu_max_size" {
  description = "Maximum number of CPU nodes"
  type        = number
  default     = 5
}

variable "cpu_desired_size" {
  description = "Desired number of CPU nodes"
  type        = number
  default     = 2
}

# GPU 节点组配置
variable "gpu_instance_types" {
  description = "Instance types for GPU node group"
  type        = list(string)
  default     = ["g4dn.xlarge"]
}

variable "gpu_min_size" {
  description = "Minimum number of GPU nodes"
  type        = number
  default     = 0
}

variable "gpu_max_size" {
  description = "Maximum number of GPU nodes"
  type        = number
  default     = 3
}

variable "gpu_desired_size" {
  description = "Desired number of GPU nodes"
  type        = number
  default     = 1
}

variable "use_spot_instances" {
  description = "Use Spot instances for cost savings"
  type        = bool
  default     = false
}

# FSx 配置
variable "enable_fsx" {
  description = "Enable FSx for Lustre file system"
  type        = bool
  default     = true
}

variable "fsx_storage_capacity" {
  description = "Storage capacity for FSx in GB (must be 1200, 2400, or increments of 2400)"
  type        = number
  default     = 1200
}

variable "fsx_deployment_type" {
  description = "FSx deployment type (SCRATCH_1, SCRATCH_2, PERSISTENT_1, PERSISTENT_2)"
  type        = string
  default     = "SCRATCH_2"
}

variable "fsx_throughput" {
  description = "Per unit storage throughput in MB/s/TiB"
  type        = number
  default     = 200
}

variable "fsx_s3_bucket" {
  description = "S3 bucket to associate with FSx (e.g., vllm-model-assets-for-eks-trin9)"
  type        = string
  default     = ""
}
