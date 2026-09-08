data "sops_file" "secrets" {
  source_file = "${path.module}/secret.sops.yaml"
}

locals {
  # Full decrypted secret tree (nested maps/lists). Using yamldecode(.raw)
  # rather than the provider's flattened .data map, since .data dot-flattens
  # nested keys as separate string entries. yamldecode (not jsondecode) is
  # required because the decrypted YAML carries comments.
  secrets = yamldecode(data.sops_file.secrets.raw)
}
