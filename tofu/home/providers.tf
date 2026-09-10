terraform {
  required_version = ">= 1.7.0"

  # Remote state, this root's own dedicated bucket (see state-backend.tf for
  # the bootstrap sequence that created it). Credentials come from
  # AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY env vars at every invocation, not
  # from this block — backend blocks can't reference var./local.
  backend "s3" {
    bucket                      = "home-tofu-state-f4696113"
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
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.108"
    }
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.11"
    }
    sops = {
      source  = "carlpett/sops"
      version = "~> 1.4"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.34"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 2.16"
    }
  }
}

provider "proxmox" {
  alias    = "node"
  for_each = local.proxmox_provider_nodes

  endpoint = each.value.endpoint
  api_token = coalesce(
    each.value.api_token,
    try(local.secrets["proxmox_${each.key == "__provider_keepalive" ? var.proxmox_node : each.key}_api_token"], null)
  )
  insecure = try(each.value.insecure, true)

  ssh {
    agent    = true
    username = try(each.value.ssh_username, var.proxmox_ssh_username)
  }
}

# Proxmox rejects `cpu.affinity` config changes from ANY API-token-
# authenticated request — checked as a literal identity-string match against
# "root@pam", which a token's own identity (always "<user>@<realm>!<token>")
# can never equal, regardless of which user owns the token or how it's
# privileged. proxmox_virtual_environment_vm.talos_nodes therefore
# authenticates through THIS password-based alias instead of provider.node
# (api_token above) — every field on that resource goes through password
# auth as a result, not just affinity: Terraform binds a whole resource to
# one provider configuration, it can't split fields across two.
# provider.node (token auth) still handles every other Proxmox resource
# (proxmox_download_file, etc.).
provider "proxmox" {
  alias    = "node_password"
  for_each = local.proxmox_provider_nodes

  endpoint = each.value.endpoint
  username = "root@pam"
  password = try(local.secrets["proxmox_${each.key == "__provider_keepalive" ? var.proxmox_node : each.key}_root_password"], null)
  insecure = try(each.value.insecure, true)

  ssh {
    agent    = true
    username = try(each.value.ssh_username, var.proxmox_ssh_username)
  }
}

provider "talos" {}

# Hetzner Object Storage, S3-compatible — used for this cluster's own etcd
# backup bucket and its own remote state bucket. Same config shape as
# tofu/cloud's identical provider block (Hetzner's endpoint needs these
# skip_* / path-style flags since it isn't real AWS).
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

# kubernetes/helm, configured directly from talos_cluster_kubeconfig.this —
# the same "provider configured from a managed resource's own attributes"
# pattern the vendored hcloud-talos module uses internally for its own
# post-bootstrap kubectl/helm providers (tofu/cloud/.terraform/modules/talos/
# terraform.tf). This is what lets tofu itself own the sops-age Secret and
# Cilium HelmRelease below, instead of requiring a manual kubectl/helm step
# before Flux can run — both only get created once the cluster is actually
# bootstrapped and kubeconfig is known, which OpenTofu can defer to safely.
provider "kubernetes" {
  host                   = talos_cluster_kubeconfig.this.kubernetes_client_configuration.host
  cluster_ca_certificate = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.ca_certificate)
  client_certificate     = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.client_certificate)
  client_key             = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.client_key)
}

provider "helm" {
  kubernetes {
    host                   = talos_cluster_kubeconfig.this.kubernetes_client_configuration.host
    cluster_ca_certificate = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.ca_certificate)
    client_certificate     = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.client_certificate)
    client_key             = base64decode(talos_cluster_kubeconfig.this.kubernetes_client_configuration.client_key)
  }
}
