data "sops_file" "secrets" {
  source_file = "${path.module}/secret.sops.yaml"
}

locals {
  proxmox_secrets = data.sops_file.secrets.data
}
