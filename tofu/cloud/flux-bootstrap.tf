# Everything Flux needs BEFORE its own `flux bootstrap` can run, owned by
# tofu instead of a manual `kubectl create secret` step. `flux bootstrap` is
# idempotent against a pre-existing flux-system namespace/secret, so this
# fully replaces that manual step — the whole cluster, including its Flux
# prerequisites, comes up from `tofu apply` alone.
resource "kubernetes_namespace" "flux_system" {
  metadata {
    name = "flux-system"
  }

  # `flux bootstrap` itself adds app.kubernetes.io/{instance,part-of,version}
  # labels to this namespace on every run (version tracks whatever flux CLI
  # version last bootstrapped it) — ignore label drift here rather than have
  # tofu fight flux over them on every plan.
  lifecycle {
    ignore_changes = [metadata[0].labels]
  }
}

resource "kubernetes_secret" "sops_age" {
  metadata {
    name      = "sops-age"
    namespace = kubernetes_namespace.flux_system.metadata[0].name
  }

  # Same key name flux's own decryption.secretRef expects. This cluster's
  # own age identity — cloud-flux.age.key, gitignored, local to the
  # operator's machine — not the shared personal/CI keys in .sops.yaml's
  # recipient list.
  data = {
    "age.agekey" = file("${path.module}/../../cloud-flux.age.key")
  }

  type = "Opaque"
}
