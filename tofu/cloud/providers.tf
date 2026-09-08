terraform {
  required_version = ">= 1.7.0"

  encryption {
    key_provider "pbkdf2" "state" {
      passphrase = var.state_encryption_passphrase
    }

    method "aes_gcm" "state" {
      keys = key_provider.pbkdf2.state
    }

    state {
      method   = method.aes_gcm.state
      enforced = true
    }
  }

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.54"
    }
    imager = {
      source  = "hcloud-talos/imager"
      version = "~> 1.0"
    }
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.11"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.3"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
    sops = {
      source  = "carlpett/sops"
      version = "~> 1.4"
    }
  }
}

provider "hcloud" {
  token = local.secrets["hcloud_api_token"]
}

provider "imager" {
  token = local.secrets["hcloud_api_token"]
}

provider "helm" {
  kubernetes = {
    host                   = local.cluster_endpoint
    client_certificate     = base64decode(talos_cluster_kubeconfig.cluster.kubernetes_client_configuration.client_certificate)
    client_key             = base64decode(talos_cluster_kubeconfig.cluster.kubernetes_client_configuration.client_key)
    cluster_ca_certificate = base64decode(talos_cluster_kubeconfig.cluster.kubernetes_client_configuration.ca_certificate)
  }
}

provider "aws" {
  access_key = local.secrets["hetzner_object_storage_access_key"]
  secret_key = local.secrets["hetzner_object_storage_secret_key"]
  region     = var.object_storage_location

  endpoints {
    s3 = "https://${var.object_storage_location}.your-objectstorage.com"
  }

  skip_credentials_validation = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
  skip_requesting_account_id  = true
  s3_use_path_style           = true
}