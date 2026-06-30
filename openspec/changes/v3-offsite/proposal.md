## Why

A third, geographically-separate cluster gives the platform disaster-recovery and remote presence: if the homelab site is down, a small offsite cluster (already meshed) can hold critical state and a minimal service footprint. It is the last phase because it depends on the Cluster Mesh (v0) and the platform/backup pieces (v1).

## What Changes

- **Cluster `offsite` (id 3) joins the mesh.** A single Talos VM on a TrueNAS Scale box (6GB) = a one-node cluster (control plane that also runs workloads).
- **ADD the offsite cluster** to the existing Cilium Cluster Mesh (homelab=1, cloud=2, offsite=3) — shared CA, unique id/name, clustermesh-apiserver. No change to homelab/cloud beyond authorizing the new peer.
- **Provision the VM by hand.** There is no clean OpenTofu provider for TrueNAS Scale VMs, so the VM is created manually on TrueNAS; Talos + Flux + Cilium are then applied the usual way (its own `clusters/offsite/` Flux entrypoint).
- **Minimal footprint.** offsite runs only what DR needs (Cilium + mesh + a small set of global/critical services or backup targets) — not the LLMs or the full app set.

## Capabilities

### New Capabilities
- `offsite-cluster`: the single-node TrueNAS-hosted Talos cluster and its membership in the Cluster Mesh for DR / remote presence.

### Modified Capabilities
- (none at spec level — extends the v0 `cluster-mesh` design, which was built to accept a third peer.)

## Impact

- **kubernetes/**: new `clusters/offsite/` Flux entrypoint (Cilium + clustermesh + the minimal DR workload set); offsite Cilium values (cluster.id 3).
- **tofu/**: none for the VM (manual on TrueNAS); optionally Talos config generation if scripted later.
- **Mesh:** authorize offsite as a peer on homelab + cloud; verify `cilium clustermesh status`.
- **DR scope:** decide what offsite holds (e.g., a backup landing zone, or a few global services) — kept minimal for the 6GB box.
