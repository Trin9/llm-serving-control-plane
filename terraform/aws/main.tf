terraform {
  required_version = ">= 1.0"
  
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.0"
    }
  }
  
  backend "s3" {
    # Configure this before use:
    # bucket = "your-terraform-state-bucket"
    # key    = "gate-service/terraform.tfstate"
    # region = "ap-northeast-1"
    # 
    # Or temporarily use a local backend (not recommended for production):
    # Comment out the s3 configuration above and uncomment the block below
    # backend "local" {
    #   path = "terraform.tfstate"
    # }
  }
}

provider "aws" {
  region = var.aws_region
}

data "aws_eks_cluster_auth" "cluster" {
  name = module.eks.cluster_name
}

provider "kubernetes" {
  host                   = module.eks.cluster_endpoint
  cluster_ca_certificate = base64decode(module.eks.cluster_certificate_authority_data)
  token                  = data.aws_eks_cluster_auth.cluster.token
}

# EKS cluster
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "~> 19.0"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  vpc_id     = var.vpc_id
  subnet_ids = var.subnet_ids

  # Enable EKS add-ons
  cluster_addons = {
    coredns = {
      most_recent = true
    }
    kube-proxy = {
      most_recent = true
    }
    vpc-cni = {
      most_recent = true
    }
    aws-ebs-csi-driver = {
      most_recent = true
    }
  }

  # CPU node group
  eks_managed_node_groups = {
    cpu_nodes = {
      name = "${var.cluster_name}-cpu"
      
      instance_types = var.cpu_instance_types
      capacity_type  = var.use_spot_instances ? "SPOT" : "ON_DEMAND"
      
      min_size     = var.cpu_min_size
      max_size     = var.cpu_max_size
      desired_size = var.cpu_desired_size
      
      labels = {
        role = "cpu"
      }
      
      tags = {
        Environment = var.environment
        NodeType    = "CPU"
      }
    }

    # GPU node group
    gpu_nodes = {
      name = "${var.cluster_name}-gpu"
      
      instance_types = var.gpu_instance_types
      capacity_type  = var.use_spot_instances ? "SPOT" : "ON_DEMAND"
      
      min_size     = var.gpu_min_size
      max_size     = var.gpu_max_size
      desired_size = var.gpu_desired_size
      
      # GPU taint: ensure only Pods that need a GPU are scheduled to these nodes
      taints = [{
        key    = "nvidia.com/gpu"
        value  = "true"
        effect = "NoSchedule"
      }]
      
      labels = {
        role = "gpu"
        "nvidia.com/gpu" = "true"
      }
      
      tags = {
        Environment = var.environment
        NodeType    = "GPU"
      }
    }
  }

  tags = {
    Environment = var.environment
    Project     = "gate-service"
    ManagedBy   = "Terraform"
  }
}

# FSx for Lustre file system (used to store large models)
resource "aws_fsx_lustre_file_system" "model_storage" {
  count = var.enable_fsx ? 1 : 0

  storage_capacity            = var.fsx_storage_capacity
  subnet_ids                  = [var.subnet_ids[0]]
  deployment_type             = var.fsx_deployment_type
  per_unit_storage_throughput = var.fsx_throughput

  security_group_ids = [aws_security_group.fsx[0].id]

  # Link an S3 bucket
  data_repository_associations {
    s3_bucket = var.fsx_s3_bucket
    import_path = "s3://${var.fsx_s3_bucket}"
    auto_import_policy = "NEW_CHANGED"
  }

  tags = {
    Name        = "${var.cluster_name}-fsx"
    Environment = var.environment
    ManagedBy   = "Terraform"
  }
}

# FSx security group
resource "aws_security_group" "fsx" {
  count = var.enable_fsx ? 1 : 0
  
  name_prefix = "${var.cluster_name}-fsx-"
  vpc_id      = var.vpc_id
  
  ingress {
    from_port   = 988
    to_port     = 988
    protocol    = "tcp"
    cidr_blocks = [data.aws_vpc.selected.cidr_block]
  }
  
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
  
  tags = {
    Name = "${var.cluster_name}-fsx-sg"
  }
}

data "aws_vpc" "selected" {
  id = var.vpc_id
}

data "aws_subnet" "fsx_subnet" {
  count = var.enable_fsx ? 1 : 0
  id    = var.subnet_ids[0]
}

# Install the FSx CSI Driver
resource "kubernetes_storage_class" "fsx" {
  count = var.enable_fsx ? 1 : 0
  
  metadata {
    name = "fsx-sc"
  }
  
  storage_provisioner = "fsx.csi.aws.com"
  reclaim_policy      = "Retain"
  
  parameters = {
    subnetId          = var.subnet_ids[0]
    securityGroupIds  = aws_security_group.fsx[0].id
    deploymentType    = var.fsx_deployment_type
  }
}

# PersistentVolume for FSx
resource "kubernetes_persistent_volume" "fsx_pv" {
  count = var.enable_fsx ? 1 : 0
  
  metadata {
    name = "model-pv"
  }
  
  spec {
    capacity = {
      storage = "${var.fsx_storage_capacity}Gi"
    }
    
    access_modes = ["ReadWriteMany"]
    persistent_volume_reclaim_policy = "Retain"
    storage_class_name = kubernetes_storage_class.fsx[0].metadata[0].name
    
    persistent_volume_source {
      csi {
        driver        = "fsx.csi.aws.com"
        volume_handle = aws_fsx_lustre_file_system.model_storage[0].id
        volume_attributes = {
          dnsname    = aws_fsx_lustre_file_system.model_storage[0].dns_name
          mountname  = aws_fsx_lustre_file_system.model_storage[0].mount_name
        }
      }
    }
    
    node_affinity {
      required {
        node_selector_term {
          match_expressions {
            key      = "topology.fsx.csi.aws.com/zone"
            operator = "In"
            values   = [data.aws_subnet.fsx_subnet[0].availability_zone]
          }
        }
      }
    }
  }
}

# PersistentVolumeClaim for FSx
resource "kubernetes_persistent_volume_claim" "model_storage" {
  count = var.enable_fsx ? 1 : 0
  
  metadata {
    name      = "model-pvc"
    namespace = "default"
  }
  
  spec {
    access_modes = ["ReadWriteMany"]
    storage_class_name = kubernetes_storage_class.fsx[0].metadata[0].name
    
    resources {
      requests = {
        storage = "${var.fsx_storage_capacity}Gi"
      }
    }
  }
}
