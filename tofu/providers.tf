terraform {
  required_version = ">= 1.6.0"
  required_providers {
    proxmox = {
      source  = "bpg/proxmox"
      version = "~> 0.60"
    }
    talos = {
      source  = "siderolabs/talos"
      version = "~> 0.5"
    }
    sops = {
      source  = "carlpett/sops"
      version = "~> 1.0"
    }
  }

  # Enable native OpenTofu state file encryption
  encryption {
    key_provider "pbkdf2" "statekey" {
      passphrase = "" # Empty indicates statekey should be supplied via TF_ENCRYPTION_PASSPHRASE_statekey/TOFU_ENCRYPTION_PASSPHRASE_statekey env vars
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
