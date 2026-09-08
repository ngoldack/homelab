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

provider "talos" {}
