# Data protection

Everything durable in this cluster has a backup path that is independent of
Kubernetes state (an etcd snapshot / `reclaimPolicy: Retain` protects none of
this), alerting that fires when the path degrades, HA for the services whose
downtime matters, and a measured restore drill on record. This page is the
runbook and the drill log; keep the measured table current with every drill.

## What protects what

| Store | Live protection | Backup path | Independent dataset | Retention |
| --- | --- | --- | --- | --- |
| CNPG `authentik-database` | 2 instances, sync quorum | Barman Cloud plugin → S3 | `truenas-fast-nfs-authentik-database` 6h/14d | 30d (ObjectStore) |
| CNPG `immich-database` | 2 instances, sync quorum | Barman Cloud plugin → S3 | `truenas-fast-nfs-immich-database` 6h/14d | 30d |
| CNPG `langfuse-database` | 1 instance | Barman Cloud plugin → S3 | `truenas-fast-nfs-langfuse-database` 6h/14d | 30d |
| CNPG `hindsight-database` | 1 instance | Barman Cloud plugin → S3 | `truenas-fast-nfs-hindsight-database` 6h/14d | 30d |
| CNPG `matrix-database` (Synapse + MAS DBs) | 1 instance | Barman Cloud plugin → S3 (`matrix/` prefix) | `truenas-fast-nfs-matrix-database` 6h/14d | 30d |
| Synapse media repository (`matrix-synapse-media` PVC) | single PVC | TrueNAS snapshots | `truenas-fast-nfs-matrix-media` 6h/14d | 14d |
| Langfuse ClickHouse + keeper | single-node stores | TrueNAS snapshots (classes exist) | classes `truenas-fast-nfs-langfuse-{clickhouse,keeper}` 6h/14d | 14d |
| Langfuse SeaweedFS (S3 binaries) | allInOne store | TrueNAS snapshots (class exists) | class `truenas-fast-nfs-langfuse-seaweedfs` daily/1mo | 1mo |
| Immich originals | TrueNAS pool `tank` | TrueNAS Periodic Snapshot Task | `truenas-tank-nfs-immich-library` daily/1mo | 1mo |
| etcd (Talos) | single-node control plane | talos-backup CronJob → Hetzner S3, age-encrypted | n/a (object store) | bucket-side |

All CNPG backups land in the dedicated Hetzner Object Storage bucket
`home-cnpg-backups-7cd4e906` (fsn1, versioned), one prefix per app, managed by
tofu (`tofu/home/backups.tf`). Bucket lifecycle is NOT configured — Hetzner's
S3 API never converges with the aws-provider lifecycle resource (documented
bug in backups.tf); retention is enforced by barman itself (see below).

### Deliberately not backed up (and why)

- **Valkey (`redis` in authentik, `langfuse` in langfuse)** — cache/broker only
  (session cache, job queue). The valkey-operator (v1alpha1, pre-production)
  exposes no backup API and the CRDs carry no persistence in either cluster; a
  wipe costs re-logins / retried jobs, not state. Revisit only if a queue
  starts carrying durable semantics.
- **TrueNAS datasets without snapshot tasks** — `registry/zot` (images are
  rebuildable artifacts pushed from builds), `monitoring/vmsingle` (metrics
  history), `immich/cache` (ML model cache). The `llmkube/models` dataset
  (re-downloadable GGUF weights) left this list with its lane on 2026-09-22;
  the dataset itself still exists on the NAS. Their StorageClasses carry no `snapshot.*` parameters —
  parameters are immutable in-cluster and provision-time only, so adding them
  to an existing class fails reconcile and would not protect the already-bound
  PVCs anyway (the 2026-09-15 commit that tried this was reverted).
- **Langfuse ClickHouse/keeper/SeaweedFS current volumes** — the snapshot
  classes exist, but the PVCs were created on the generic class and
  `storageClassName` is immutable on a bound PVC. To adopt the scheduled
  snapshots these volumes must be recreated (like the authentik migration
  below); until then their protection is class-availability only.

## WAL archiving + backups (Barman Cloud plugin)

- CNPG operator 1.30 with `plugin-barman-cloud` v0.15.0 (chart 0.8.0,
  `cnpg-operator/plugin-barman-cloud.yaml`); the plugin chart ships the
  `ObjectStore` CRD and requires cert-manager — hence `cnpg-operator`
  dependsOn `cert-manager` in the Flux Kustomization.
- Each app namespace has one `ObjectStore` (`objectstore.yaml`):
  `destinationPath: s3://home-cnpg-backups-7cd4e906/<app>/`,
  `endpointURL: https://fsn1.your-objectstorage.com`, S3 credentials + region
  (`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_REGION`) from the
  per-app SOPS secret `<app>-backup-s3-credentials`, gzip WAL compression,
  `retentionPolicy: "30d"` **on the ObjectStore** — the Cluster's
  `spec.backup.retentionPolicy` is in-tree-Barman-only and ignored by the
  plugin.
- The sidecar carries the Hetzner checksum overrides
  (`AWS_REQUEST_CHECKSUM_CALCULATION` / `AWS_RESPONSE_CHECKSUM_VALIDATION`
  `when_required`) without which S3-compatible uploads fail.
- WAL RPO bound: every Cluster sets `postgresql.parameters.archive_timeout:
  "60"` → archived WAL trails the primary by ≤ ~1 minute regardless of write
  volume (discovered by the first drill, see below).
- Daily full backups: `ScheduledBackup` per cluster (`scheduled-backup.yaml`),
  CNPG 6-field cron, staggered 02:05 (immich) / 02:10 (authentik) / 02:17
  (langfuse) / 02:39 (hindsight) **UTC** (no timeZone field exists; the
  operator pod runs UTC).
- On-demand: `kubectl cnpg backup -n <ns> <cluster> --method=plugin
  --plugin-name=barman-cloud.cloudnative-pg.io`, or a `Backup` object with
  `method: plugin` + `pluginConfiguration: {name: barman-cloud.cloudnative-pg.io}`.

## Alerts

`monitoring/rules.yaml` (VMRule `data-protection`, loaded by vmalert):

| Alert | Signal | Threshold |
| --- | --- | --- |
| `WALArchivingStalled` | `cnpg_collector_pg_wal_archive_status{value="ready"}` | backlog > 20 for 30m |
| `BackupTooOld` | plugin `barman_cloud_cloudnative_pg_io_last_available_backup_timestamp` | age > 30h (fires also for "never backed up" — timestamp 0) |
| `BackupMetricsAbsent` | `absent(...)` on the same series | 30m — a plugin sidecar outage must not silently blind `BackupTooOld` |

The plugin metrics carry no cluster label of their own; the five CNPG
`VMPodScrape`s relabel pod label `cnpg.io/cluster` → metric label `cluster`
(so `BackupTooOld`/`WALArchivingStalled` report per cluster). The scrape port
must be referenced by NAME (`port: metrics`) — CNPG's exporter port 9187 is
named `metrics`, and a numeric `port:` renders a keep-rule that never matches
(that is how CNPG scrapes ended up silently absent until 2026-09-15).

## Credentials and keys

- Backup S3 credentials are SOPS-encrypted in git (`<app>-backup-s3-credentials.sops.yaml`),
  decryptable only with the age identities held at the repo root
  (`age.key`, `home-flux.age.key` — both gitignored). The in-cluster copies
  exist solely because the CNPG operator must run the uploads itself.
- The Talos etcd backups are age-encrypted before upload; the **private age
  key lives outside the cluster** (`tofu/home/secret.sops.yaml`, operator-side
  only). Restoring etcd requires that machine.
- Postgres/ClickHouse backups in the bucket are not client-side encrypted
  (barman has no such knob); the bucket is the trust boundary. The CNPG
  credentials are the same Hetzner keypair that manages tofu's state bucket —
  treat a leak of the SOPS secret as access to the state store and rotate
  both. Dedicated per-bucket keys are the future hardening step.

## Restore drills (quarterly, destructive)

Run `task check`, then per drill: fresh backup → restore → measure → record
here. Two procedures are proven; use the one matching the situation.

### Drill 1 — non-destructive PITR clone (langfuse, 2026-09-15)

Clone from backup into a scratch Cluster (`bootstrap.recovery` +
`recoveryTarget.targetTime`), verify content at the chosen instant, delete.

- **Measured RTO: 123 s** from applying the restore Cluster to
  `Cluster in healthy state` (single instance, 10 Gi NFS, ~42 MB base tar +
  ~5 min WAL).
- **Measured RPO: ≤ 60 s** (WAL-tail bound after `archive_timeout`; see the
  finding below) plus a content proof: two markers inserted on prod
  (00:27:33, 00:40:38), target 00:35:00 → restored cluster contained
  marker 1 and NOT marker 2, `pg_is_in_recovery() = f`.
- **Finding #1 (fixed same day):** the first drill died with
  `recovery ended before configured recovery target was reached`. On a quiet
  database the open 16 MiB WAL segment is archived only when it fills — the
  archived tail lagged real time by tens of minutes (langfuse's last archived
  WAL was 40+ minutes old at drill time). `archive_timeout: "60"` now bounds
  it; verified 60 s cadence in the bucket.
- **Finding #2:** a PITR target later than the last *commit timestamp* in the
  WAL stream cannot be reached (nothing exists to replay) — pick a target
  between two known writes and assert presence/absence.

### Drill 2 — destructive restore-into-place (authentik, 2026-09-15)

Storage-class migration done as a restore: Cluster deleted, recreated from
the Barman backup + WAL onto `truenas-fast-nfs-authentik-database`.

- **Measured RTO: ~17 min** clean restore (delete 00:48:53Z → healthy 2/2 at
  01:38:29Z includes two failed attempts + one unrecoverable reset; the pure
  restore after the spec fix was 01:21→01:38). **App outage: ~49 min**
  (authentik web/worker crash-looped until the DB returned; no manual app
  action needed afterwards, 776 migrations verified intact).
- **Measured RPO: 0 observed** (full WAL replay, no targetTime), bounded by
  the same 60 s archive tail.
- **Finding #3 (procedure gotcha):** `bootstrap.recovery` with
  `spec.plugins[].isWALArchiver: true` on the SAME serverName fails — the
  restore hook runs `barman-cloud-check-wal-archive`, which refuses setup
  while the remote archive is ahead of the restoring instance
  (`unexpected failure invoking barman-cloud-wal-archive: exit status 1`).
  Restore in TWO steps: (1) commit the recovery spec WITHOUT `spec.plugins`,
  (2) after healthy, re-add `spec.plugins` (commit ce5ee88). Archiving
  resumes on a new timeline (`00000002...` verified in the bucket).
- **Finding #4:** after repeated restore failures CNPG pins the phase to
  `Cluster is unrecoverable and needs manual intervention` and stops
  retrying — the documented manual step is `kubectl delete cluster` + let
  Flux recreate from the fixed manifest.

### Quarterly checklist (next run ~2026-12-15)

1. `task check` green; `kubectl get clusters -A` all healthy; ObjectStores +
   ScheduledBackups present; vmalert rules loaded (`/api/v1/rules`).
2. Drill 1 (PITR clone) on one cluster + marker presence/absence assert;
   record RTO/RPO row.
3. Drill 2 (destructive restore-into-place) on a *low-traffic* cluster;
   record RTO/RPO + outage window.
4. Verify `BackupTooOld`/`WALArchivingStalled`/`BackupMetricsAbsent` fire and
   clear (vmalert rules endpoint), and that the S3 bucket still lists
   per-cluster `base/` + `wals/` prefixes.
5. Check the S3 inventory: base backups within retention for all 5 clusters,
   WAL segments continuous at ≤ 60 s cadence.
6. Update the table below.

| Date | Cluster | Method | RTO | RPO | Findings |
| --- | --- | --- | --- | --- | --- |
| 2026-09-15 | langfuse-database | PITR clone → 00:35:00Z | 123 s | ≤ 60 s (marker-verified) | archive_timeout gap fixed |
| 2026-09-15 | authentik-database | destructive restore-into-place | ~17 min (outage 49 min) | 0 observed (60 s bound) | two-step archiver re-enable |

## Recovery cheat-sheet

- **PITR to time T:** copy `objectstore.yaml` + credentials secret into a
  scratch namespace (or reuse the app's), apply a Cluster with
  `bootstrap.recovery.source` + `externalClusters[].plugin` (`barmanObjectName`
  + `serverName: <prod-cluster>`) and `recoveryTarget.targetTime: "<T>"`,
  T covered by archived WAL (Finding #2). No `spec.plugins` while restoring.
- **Storage-class migration:** Drill 2 procedure — delete cluster, recreate
  with `bootstrap.recovery` + new `storageClass`, two-step archiver re-enable.
- **etcd:** `talosctl` + the talos-backup bucket; requires the age private key
  from `tofu/home/secret.sops.yaml` (outside the cluster, deliberately).
- **TrueNAS dataset roll-forward:** snapshots are TrueNAS-side Periodic
  Snapshot Tasks (the CSI driver's `snapshot.*` parameters); clone/rollback
  is a NAS operation (`zfs clone/rollback`), not a Kubernetes one.
