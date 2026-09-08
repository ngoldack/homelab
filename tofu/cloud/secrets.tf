data "sops_file" "secrets" {
  source_file = "${path.module}/secret.sops.yaml"
}

locals {
  secrets = yamldecode(data.sops_file.secrets.raw)
}