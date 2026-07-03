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

  # Remote state: Hetzner Object Storage (S3-compatible), one bucket shared across
  # locations, keyed by path so cloud/offsite get their own object when they land.
  #
  # Partial configuration: bucket/key/endpoint are not secret and are safe to
  # commit, but backend blocks cannot reference variables or SOPS — credentials
  # are deliberately left unset here and supplied at `tofu init`/plan/apply time
  # via the AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env vars (see `task tofu:*`,
  # which exports them from tofu/locations/home/secret.sops.yaml).
  #
  # One-time prerequisite (Hetzner has no Terraform-manageable Object Storage
  # bucket resource): create the bucket + an access/secret key pair, and enable
  # versioning, via the Hetzner Cloud Console. See README.md "Getting Started".
  #
  # VERIFY-BEFORE-DEPLOY: neither `use_path_style` nor `use_lockfile` (native
  # conditional-write locking) have been exercised against a real Hetzner bucket
  # from this repo. If `tofu init` can't reach the bucket, try flipping
  # use_path_style; if lock acquisition fails, Hetzner's PutObject may not honor
  # If-None-Match — drop use_lockfile (acceptable for a single-operator homelab
  # with no concurrent applies).
  backend "s3" {
    bucket = "ngoldack-tofu-state"
    key    = "locations/home/terraform.tfstate"
    region = "fsn1" # Hetzner has no AWS regions; skip_region_validation allows any string — using the bucket's Hetzner location code for clarity.

    endpoints = {
      s3 = "https://fsn1.your-objectstorage.com"
    }

    # Hetzner Object Storage is S3-compatible but not AWS: skip AWS-only calls
    # (STS/account-id) and SDK checksum behavior non-AWS S3 servers may reject.
    skip_credentials_validation = true
    skip_region_validation      = true
    skip_requesting_account_id  = true
    skip_s3_checksum            = true
    use_path_style              = true
    use_lockfile                = true
  }

  # Enable native OpenTofu state file encryption. Backend-agnostic: the state is
  # encrypted before it's written to (and decrypted after it's read from) the S3
  # backend above, so it stays protected at rest even in the Hetzner bucket.
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
  # Per-host key convention: proxmox_<host>_api_token (host = Proxmox node name).
  api_token = local.proxmox_secrets["proxmox_pmx-main_api_token"]
  insecure  = true

  ssh {
    agent    = true
    username = var.proxmox_ssh_username
  }
}

provider "talos" {
  # Configuration parameters if needed; defaults are generally fine.
}
