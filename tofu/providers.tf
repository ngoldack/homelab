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
    sops = {
      source  = "carlpett/sops"
      version = "~> 1.4"
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
  # API token auth: "<user>@<realm>!<tokenid>=<uuid>" (read from SOPS). The user is
  # embedded in the token, so no separate username/password is needed. SSH (below)
  # still handles the operations the Proxmox API token cannot perform on its own.
  api_token = local.proxmox_secrets.proxmox_api_token
  insecure  = true

  ssh {
    agent    = true
    username = var.proxmox_ssh_username
  }
}

provider "talos" {
  # Configuration parameters if needed; defaults are generally fine.
}
