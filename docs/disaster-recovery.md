# Backup & Disaster Recovery

This document describes the recovery model for the homelab Talos/Flux cluster.

> **Phase status:** v0 (current) ships **no automated backup tooling**. Recovery
> relies on the GitOps desired state plus the Talos PKI held in the encrypted
> OpenTofu state. Automated, offsite, point-in-time backups (etcd snapshots,
> Postgres PITR, object-store replication) are planned and tracked in OpenSpec:
> see `openspec/changes/v1-platform` (in-cluster S3 + CloudNativePG backups) and
> `openspec/changes/v3-offsite` (offsite cluster / 3-2-1 copy). Do **not** treat
> the v1/v3 backup design as implemented yet.

## What you must safeguard off-cluster

Recovery is impossible without these. Keep copies **outside** the cluster and
TrueNAS (e.g. a password manager + a second offline location):

1. **The workstation age private key** (`age.key`, public key recorded in
   `.sops.yaml`). It decrypts every SOPS secret in Git. Without it, no secret —
   and therefore no cluster bootstrap — can be reconstructed.
2. **`tofu/terraform.tfstate`** — holds `talos_machine_secrets` (etcd CA,
   Kubernetes CA, cluster identity). It is committed to Git **encrypted** via
   OpenTofu native AES-GCM state encryption. Ensure both the Git remote **and**
   the `state_encryption_passphrase` (in `tofu/secret.sops.yaml`) are recoverable.
3. **The Git repository** itself — it is the single source of desired state that
   Flux reconciles.

If all three survive, the cluster can be rebuilt from scratch.

## Where the data lives (v0)

| State | Where it lives | Survives cluster loss? |
|-------|----------------|------------------------|
| Kubernetes/Flux desired state | Git repository | Yes (in Git) |
| Cluster PKI / machine secrets | `tofu/terraform.tfstate` (encrypted, in Git) | Yes (in Git) |
| Application PVCs (e.g. llama.cpp model weights) | TrueNAS, via the `tns-*` CSI StorageClasses | Yes (on TrueNAS) |
| Scratch volumes | `local-storage` (node-local) | No — disposable by design |
| etcd contents not derived from Git | in-cluster etcd | **No backup in v0** |

Because the cluster is GitOps-driven and persistent app data sits on TrueNAS
(not on the nodes), a full rebuild loses only the live etcd state, which Flux
re-derives from Git.

## Recovery: total cluster loss

1. Reprovision the VMs and Talos config with OpenTofu. Because the **same**
   `talos_machine_secrets` are read from the encrypted state, the rebuilt nodes
   keep the original cluster identity:
   ```bash
   task tofu:apply
   ```
2. Re-create the SOPS age secret Flux uses to decrypt manifests, and bootstrap
   Flux against `main`:
   ```bash
   kubectl create secret generic sops-age -n flux-system \
     --from-file=age.agekey=age.key
   flux bootstrap github \
     --owner=ngoldack --repository=homelab \
     --branch=main --path=kubernetes/clusters/homelab
   ```
3. Flux reconciles controllers, configs, and apps from Git. PVCs re-bind to the
   existing TrueNAS datasets via the CSI driver, so application data returns
   without a restore step.

> The cluster runs a **single control-plane node** in v0, so there is no etcd
> quorum and no etcd snapshot to recover from — the recovery path is "rebuild
> from Git + tfstate", not "restore etcd". Anything written directly to etcd and
> never expressed in Git is not recoverable in v0.

## Known gaps / follow-ups

- **No automated backups in v0.** etcd snapshots, database PITR, and offsite S3
  replication are deferred to v1/v3 (see OpenSpec changes above). Until then,
  treat etcd as disposable and keep the three off-cluster artifacts above safe.
- **No backup-failure alerting.** The VictoriaMetrics stack is deployed, but no
  backup jobs exist yet to alert on. Alerting lands with the v1 backup work.
- **Single control-plane node.** No HA / failover; the rebuild procedure above is
  the only control-plane recovery path until a multi-node control plane is added.
