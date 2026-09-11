# Gateway API CRDs, installed by tofu rather than Flux.
#
# This exists to solve an ordering problem that Flux structurally cannot:
# cilium-operator checks for the Gateway API CRDs exactly once at startup and
# never retries, but Flux cannot run before the CNI, and the CNI is Cilium. On
# the old cloud cluster these were Flux-owned, the operator started 24 minutes
# before the CRDs existed, and the Gateway API controller silently never
# started — the Gateway reported "Waiting for controller" from creation and
# nobody noticed, because the Kustomization itself was green.
#
# Keeping them here puts a real dependency edge in the same graph that installs
# Cilium (helm_release.cilium depends_on this), so the ordering holds on a
# from-scratch rebuild too, not just on a cluster that happens to already have
# them.
#
# There must be exactly ONE owner. Do not add a Flux Kustomization for these as
# well: two owners doing server-side apply with prune enabled can delete the
# CRDs between them, which cascade-deletes every Gateway and HTTPRoute.
resource "helm_release" "gateway_api_crds" {
  name      = "gateway-api-crds"
  namespace = "kube-system"
  chart     = "${path.module}/charts/gateway-api-crds"

  # CRDs are cluster-scoped and slow to establish; give the API server time to
  # register all six before anything depends on them.
  wait    = true
  timeout = 300
}
