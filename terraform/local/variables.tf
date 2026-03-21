variable "cluster_name" {
  description = "Name of the Kind cluster"
  type        = string
  default     = "gate-dev"
}

variable "host_path" {
  description = "Host path for local persistent volume"
  type        = string
  default     = "/tmp/mock-fsx"
}

variable "storage_size" {
  description = "Storage size for persistent volume"
  type        = string
  default     = "100Gi"
}
