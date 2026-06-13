# Backup & Disaster Recovery

This document describes the backup architecture for the homelab Talos/Flux cluster and
the procedures to recover from data loss or total cluster loss.

## Backup layers

| Layer | Tool | What it protects | Schedule | Destination |
|-------|------|------------------|----------|-------------|
| etcd / Kubernetes control plane | [`talos-backup`](https://github.com/siderolabs/talos-backup) CronJob (`velero` ns) | etcd snapshot (all K8s objects, Flux state) | daily 03:00 | `s3://<hetzner-bucket>/etcd-backups`, age-encrypted |
| PostgreSQL (authentik, khoj, n8n) | CloudNativePG + [Barman Cloud plugin](https://cloudnative-pg.io/plugin-barman-cloud) | continuous WAL archive + daily base backup (PITR) | WAL: continuous, base: daily 01:00 | `s3://<hetzner-bucket>/cnpg/<app>` |
| Cluster objects + non-DB PVCs | Velero (Kopia FS backup) | namespaced/cluster manifests, non-DB volumes | daily 02:00, 10-day TTL | `s3://<hetzner-bucket>/k8s-backups` |
| Talos machine secrets (PKI) | OpenTofu state (`tofu/terraform.tfstate`, encrypted) | cluster CAs / identity | on `tofu apply` | committed to Git (AES-GCM encrypted) |

All S3 backup targets live **offsite** in a single **Hetzner Object Storage** bucket
(provisioned by OpenTofu — `tofu/object-storage.tf`, default name `ngoldack-homelab-dr`,
location `fsn1`), separated by object prefixes (`etcd-backups/`, `k8s-backups/`,
`cnpg/<app>/`). This is independent of the in-cluster MinIO tenant and the TrueNAS that
serves primary storage, satisfying the offsite (3-2-1) requirement. Velero explicitly
**skips** filesystem backup of `tns-fast-nvmeof` volumes (CNPG + Valkey) via
`velero-resource-policy.yaml`, because Postgres is backed up consistently by Barman.

## What you must safeguard off-cluster

Recovery is impossible without these. Keep copies **outside** the cluster and TrueNAS:

1. **The homelab age private key** (`age1h3lgj5t47k5vx67ck9gztwa7kr8pafzlnk725p82nvlvhn8z9g0qze7alf`).
   It decrypts every SOPS secret **and** the etcd snapshots (it is the `talos-backup`
   age recipient). Without it, neither Git secrets nor etcd backups can be restored.
2. **`tofu/terraform.tfstate`** — holds `talos_machine_secrets` (etcd CA, Kubernetes CA,
   cluster identity). An etcd snapshot is useless without the matching PKI. It is committed
   to Git encrypted; ensure the Git remote and the state-encryption passphrase
   (`state_encryption_passphrase` in `tofu/secret.sops.yaml`) are both recoverable.
3. The **Git repository** itself (Flux desired state).

## Prerequisites for backups to actually run

These are one-time setup steps. Until done, the jobs exist but fail/no-op.

1. **Generate Hetzner Object Storage S3 credentials.** Hetzner has **no API/Terraform** for
   credentials — create them in the Hetzner Cloud Console (Security -> S3 Credentials ->
   Generate). The secret key is shown only once. Put both keys in `tofu/secret.sops.yaml`
   (`hetzner_s3_access_key` / `hetzner_s3_secret_key`) and re-encrypt:
   ```bash
   SOPS_AGE_KEY_FILE=age.key sops --encrypt --in-place tofu/secret.sops.yaml
   ```
2. **Provision the bucket with OpenTofu.** The bucket, versioning and lifecycle rules are
   declared in `tofu/object-storage.tf` (the AWS provider is pointed at the Hetzner S3
   endpoint). The default name is `var.hetzner_dr_bucket_name = "ngoldack-homelab-dr"` and
   location `var.hetzner_objectstorage_location = "fsn1"`:
   ```bash
   cd tofu && tofu apply
   ```
   If you change the bucket name/location, update the Kubernetes manifests in step 3 to match.
 3. **Set the bucket name, region and endpoint** (these are pre-filled for the defaults —
    `ngoldack-homelab-dr`, `fsn1`, `https://fsn1.your-objectstorage.com`) in:
    - `kubernetes/infrastructure/controllers/backup/helmrelease.yaml` (Velero BSL)
    - `kubernetes/infrastructure/controllers/backup/talos-backup.yaml`
    - the CNPG backup endpoint/bucket are centralized as `DR_S3_ENDPOINT` / `DR_S3_BUCKET`
      in `kubernetes/clusters/production/cluster-vars/cluster-secrets.sops.yaml` and consumed
      by the shared `kubernetes/apps/_components/cnpg-barman-backup` component
 4. **Fill the S3 credentials** in the SOPS secrets (replace `CHANGEME` with the same Hetzner
    keys from step 1), then re-encrypt in place with `sops --encrypt --in-place <file>`:
    - `kubernetes/infrastructure/controllers/backup/secret.sops.yaml` (Velero)
    - `kubernetes/infrastructure/controllers/backup/talos-backup-s3.sops.yaml`
    - `kubernetes/apps/{authentik,n8n}/barman-s3.sops.yaml`
5. **Apply the Talos machine-config change** that grants the backup pod in-cluster etcd
   API access (`tofu/talos.tf` -> `machine.features.kubernetesTalosAPIAccess`):
   ```bash
   cd tofu && tofu apply
   ```
   Without this the `talos-backup` pod cannot reach the Talos API and snapshots fail.

## Verifying backups

```bash
# etcd: confirm the CronJob ran and uploaded
kubectl -n velero get cronjob talos-backup
kubectl -n velero create job --from=cronjob/talos-backup talos-backup-test   # manual run

# CNPG: backup status + first recoverability point per cluster
kubectl -n authentik get cluster authentik-postgres -o wide
kubectl -n authentik get backups
kubectl cnpg backup -n n8n n8n-postgres --method=plugin \
  --plugin-name=barman-cloud.cloudnative-pg.io   # on-demand base backup

# Velero
kubectl -n velero get backups
```

## Recovery procedures

### A. Total cluster loss (rebuild + etcd restore)

1. Reprovision the VMs and Talos config with OpenTofu (recreates nodes with the **same**
   `talos_machine_secrets` from state — critical for etcd restore):
   ```bash
   cd tofu && tofu apply
   ```
2. Fetch and decrypt the latest etcd snapshot from S3, then decrypt with age:
   ```bash
   aws --endpoint-url https://fsn1.your-objectstorage.com s3 cp \
     s3://<hetzner-bucket>/etcd-backups/<snapshot>.age ./snapshot.age
   age -d -i <homelab-age-identity> -o db.snapshot snapshot.age
   ```
3. Bootstrap the first control-plane node recovering from the snapshot:
   ```bash
   talosctl bootstrap --recover-from=./db.snapshot \
     -n <controlplane-ip> --endpoints <controlplane-ip>
   ```
4. Once the API server is up, Flux reconciles the rest of the cluster from Git.
5. Restore databases per section B (etcd does not contain PVC data).

> Note: the cluster runs a **single control-plane node** by default, so there is no etcd
> quorum to fall back on — the snapshot is the only recovery path for control-plane state.

### B. PostgreSQL recovery (PITR or full)

Restore into a **new** Cluster that bootstraps from the object store, then cut the app over.
Example for n8n (adapt names/namespace):

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: n8n-postgres-restore
  namespace: n8n
spec:
  instances: 1
  storage:
    size: 10Gi
    storageClass: tns-fast-nvmeof
  bootstrap:
    recovery:
      source: n8n-source
      # omit recoveryTarget for latest; or pin a point in time:
      # recoveryTarget:
      #   targetTime: "2026-06-03 08:00:00+00"
  externalClusters:
    - name: n8n-source
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters:
          barmanObjectName: n8n-backup-store
          serverName: n8n-postgres
  # Re-enable ongoing archiving for the restored cluster:
  plugins:
    - name: barman-cloud.cloudnative-pg.io
      isWALArchiver: true
      parameters:
        barmanObjectName: n8n-backup-store
```

Verify, then repoint the app and retire the old cluster.

### C. Single namespace / app objects (Velero)

```bash
velero restore create --from-backup <backup-name> --include-namespaces <ns>
```
Use this for accidentally deleted manifests/PVCs that are **not** CNPG/Valkey volumes.

## Known gaps / follow-ups

- **Offsite copy (3-2-1): satisfied.** All backups are replicated to Hetzner Object Storage,
  independent of the in-cluster MinIO tenant and the TrueNAS serving primary storage. Ensure
  the Hetzner credentials and the age key / tfstate are themselves recoverable off-cluster.
- **No automated backup-failure alerting.** The cluster runs `victoria-metrics-single`
  (no VM Operator / `VMRule` CRDs), so backup metrics are not yet alerted on. Follow-up:
  add scraping of Velero + CNPG backup metrics and alert on missed/failed backups.
- **CNPG runs `instances: 1`** (no HA replica). Backups protect data; they do not provide
  failover. Consider `instances: 3` for the identity-critical authentik database.
