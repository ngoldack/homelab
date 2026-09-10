terraform {
  required_version = ">= 1.7.0"

  # Remote state, this root's own dedicated bucket (see state-backend.tf for
  # the bootstrap sequence that created it). Credentials come from
  # AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY env vars at every invocation, not
  # from this block — backend blocks can't reference var./local.
  backend "s3" {
    bucket                      = "cloud-tofu-state-f429558f"
    key                         = "terraform.tfstate"
    region                      = "fsn1"
    endpoints                   = { s3 = "https://fsn1.your-objectstorage.com" }
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_metadata_api_check     = true
    use_path_style              = true
  }

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
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.34"
    }
  }
}

provider "hcloud" {
  token = local.secrets["hcloud_api_token"]
}

provider "imager" {
  token = local.secrets["hcloud_api_token"]
}


# Configured from the module's own kubeconfig_data output — the same
# resource-attribute-backed provider pattern the module uses internally for
# its own helm/kubectl providers (see .terraform/modules/talos/terraform.tf).
# This is what lets tofu itself own the sops-age Secret (secrets.tf) instead
# of requiring a manual `kubectl create secret` before Flux can run.
provider "kubernetes" {
  host                   = module.talos.kubeconfig_data.host
  cluster_ca_certificate = module.talos.kubeconfig_data.cluster_ca_certificate
  client_certificate     = module.talos.kubeconfig_data.client_certificate
  client_key             = module.talos.kubeconfig_data.client_key
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