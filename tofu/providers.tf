terraform {
  required_version = ">= 1.6.0"
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.108"
    }
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.11"
    }
    # Hetzner Cloud — provisions the edge VPS worker node (tofu/edge.tf).
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.49"
    }
    # Imports a Talos Image Factory image into Hetzner as a snapshot (ALPHA).
    imager = {
      source  = "hcloud-talos/imager"
      version = "~> 1.0"
    }
    sops = {
      source  = "carlpett/sops"
      version = "~> 1.4"
    }
    # Used only to provision Hetzner Object Storage (S3-compatible) buckets. The
    # Hetzner Cloud provider has no Object Storage resources, and Hetzner exposes
    # no API for buckets/credentials — only the S3 API itself — so we drive it
    # through the standard AWS S3 resources pointed at the Hetzner endpoint.
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
  }

  # Enable native OpenTofu state file encryption
  encryption {
    key_provider "pbkdf2" "statekey" {
      passphrase = "" # Supplied at runtime via the TF_ENCRYPTION env var (key_provider.pbkdf2.statekey passphrase); never hardcoded.
    }

    method "aes_gcm" "aes" {
      keys = key_provider.pbkdf2.statekey
    }

    state {
      method = method.aes_gcm.aes
    }

    plan {
      method = method.aes_gcm.aes
    }
  }
}

provider "proxmox" {
  endpoint = var.proxmox_api_endpoint
  username = var.proxmox_api_username
  password = local.proxmox_secrets.proxmox_api_password
  insecure = true

  ssh {
    agent    = true
    username = var.proxmox_ssh_username
  }
}

provider "talos" {
  # Configuration parameters if needed; defaults are generally fine.
}

# Hetzner Cloud — edge node provisioning (edge.tf).
provider "hcloud" {
  token = local.proxmox_secrets.hcloud_token
}

# The imager provider authenticates via the HCLOUD_TOKEN environment variable
# (export it for the OpenTofu run); it takes no token argument.
provider "imager" {}

# AWS provider aimed at Hetzner Object Storage (not AWS). All AWS-specific
# metadata/credential validation is disabled because the endpoint is S3-compatible
# but not actually AWS. Credentials are the Hetzner Console-generated S3 keys.
provider "aws" {
  region     = var.hetzner_objectstorage_location # cosmetic; Hetzner ignores it
  access_key = local.proxmox_secrets.hetzner_s3_access_key
  secret_key = local.proxmox_secrets.hetzner_s3_secret_key

  skip_credentials_validation = true # no AWS STS
  skip_region_validation      = true # fsn1/nbg1/hel1 are not AWS regions
  skip_requesting_account_id  = true # no IAM/STS GetCallerIdentity
  skip_metadata_api_check     = true

  s3_use_path_style = true

  endpoints {
    s3 = "https://${var.hetzner_objectstorage_location}.your-objectstorage.com"
  }
}
