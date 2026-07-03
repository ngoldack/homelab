data "sops_file" "secrets" {
  source_file = "${path.module}/secret.sops.yaml"
}

locals {
  # Full decrypted secret tree (nested maps/lists). Using jsondecode(.raw)
  # rather than the provider's flattened .data map, since .data dot-flattens
  # nested keys as separate string entries and doesn't cleanly represent lists
  # (e.g. network.nameservers, network.node_ips.<pool>) — jsondecode(.raw) is
  # the provider-documented pattern for structured values.
  secrets = jsondecode(data.sops_file.secrets.raw)

  # Network topology is treated as sensitive (IPs/subnet reveal internal
  # topology) and is sourced entirely from the secret file's `network` block —
  # never committed in plaintext. See secret.sops.yaml for the schema.
  network = local.secrets.network
}
