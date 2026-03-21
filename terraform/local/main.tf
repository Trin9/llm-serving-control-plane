terraform {
  required_version = ">= 1.0"
  
  required_providers {
    kind = {
      source  = "tehcyx/kind"
      version = "~> 0.2"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.0"
    }
    null = {
      source  = "hashicorp/null"
      version = "~> 3.0"
    }
  }
  
  backend "local" {
    path = "terraform.tfstate"
  }
}

provider "kind" {}

provider "kubernetes" {
  config_path = kind_cluster.local.kubeconfig_path
}

provider "null" {}

# 创建本地 Kind 集群
resource "kind_cluster" "local" {
  name = var.cluster_name
  wait_for_ready = true
  
  kind_config {
    kind        = "Cluster"
    api_version = "kind.x-k8s.io/v1alpha4"

    node {
      role = "control-plane"
      
      extra_port_mappings {
        container_port = 80
        host_port      = 8080
      }
    }

    node {
      role = "worker"
    }
  }
}

# Wait for cluster API server to be fully ready by actually testing the connection
resource "null_resource" "wait_for_api" {
  depends_on = [kind_cluster.local]
  
  provisioner "local-exec" {
    command = <<-EOT
      export KUBECONFIG="${kind_cluster.local.kubeconfig_path}"
      
      # Wait for kubeconfig file to exist
      max_wait=60
      waited=0
      while [ ! -f "${kind_cluster.local.kubeconfig_path}" ] && [ $waited -lt $max_wait ]; do
        echo "Waiting for kubeconfig file... ($waited/$max_wait seconds)"
        sleep 1
        waited=$((waited + 1))
      done
      
      if [ ! -f "${kind_cluster.local.kubeconfig_path}" ]; then
        echo "Error: kubeconfig file not found after $max_wait seconds"
        exit 1
      fi
      
      # Wait for API server to be ready
      max_attempts=60
      attempt=0
      while [ $attempt -lt $max_attempts ]; do
        if kubectl --kubeconfig="${kind_cluster.local.kubeconfig_path}" get nodes >/dev/null 2>&1; then
          # Verify API server is actually responding
          if kubectl --kubeconfig="${kind_cluster.local.kubeconfig_path}" get --raw /healthz >/dev/null 2>&1; then
            echo "Kubernetes API server is ready!"
            exit 0
          fi
        fi
        attempt=$((attempt + 1))
        echo "Waiting for Kubernetes API server... (attempt $attempt/$max_attempts)"
        sleep 2
      done
      
      echo "Error: Failed to connect to Kubernetes API after $max_attempts attempts"
      exit 1
    EOT
  }
  
  triggers = {
    cluster_id = kind_cluster.local.id
  }
}

# 创建本地 PersistentVolume (hostPath)
resource "kubernetes_persistent_volume" "model_storage" {
  metadata {
    name = "model-pv"
  }
  
  spec {
    capacity = {
      storage = var.storage_size
    }
    
    access_modes = ["ReadWriteMany"]
    persistent_volume_reclaim_policy = "Retain"
    storage_class_name = "manual"  # 显式设置，避免 Kind 自动分配默认 StorageClass
    
    persistent_volume_source {
      host_path {
        path = var.host_path
        type = "DirectoryOrCreate"
      }
    }
  }
  
  depends_on = [null_resource.wait_for_api]
}

# 创建 PersistentVolumeClaim
resource "kubernetes_persistent_volume_claim" "model_storage" {
  metadata {
    name      = "model-pvc"
    namespace = "default"
  }
  
  spec {
    access_modes = ["ReadWriteMany"]
    storage_class_name = "manual"  # 与 PV 匹配，显式设置以避免默认 SC 干扰
    
    resources {
      requests = {
        storage = var.storage_size
      }
    }
  }

  depends_on = [kubernetes_persistent_volume.model_storage]
}
