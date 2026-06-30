# Cloud location (Hetzner) — v1

OpenTofu for the `cloud` location lands in **v1** (see
`openspec/changes/v1-platform`): a single Hetzner VPS running its own
single-node Talos/Cilium cluster (`cloud`, cluster id 2), joined to the `home`
cluster via Cilium Cluster Mesh and acting as the public ingress edge.

Nothing is provisioned here yet.
