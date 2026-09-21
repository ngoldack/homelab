# Automated CNPG restore drills

Six CronJobs that, once a month, turn the latest Barman-cloud backup of each
CloudNativePG database back into a running PostgreSQL cluster, run an
invariant query against the recovered data, publish the outcome as metrics and
delete everything again. This is the automated half of the review's
"unproven restores" gap: `docs/data-protection.md` holds two hand-run drills on
record (langfuse PITR clone, authentik destructive restore) and
`docs/service-catalog.md` marks the other clusters' RTO `[unverified]` —
these drills replace that with a recurring, unattended measurement.

The six databases are the full set of CNPG clusters in the repo: `immich`,
`paperless`, `hindsight`, `langfuse`, `authentik`, `matrix`.

## The drill contract

Every run, for one database:

1. **Deletes its own leftovers** from a previous run (idempotent start).
2. **Discovers the backup location at run time** by reading the *live* source
   `Cluster`'s `plugins[].parameters.barmanObjectName` and that `ObjectStore`'s
   `configuration.destinationPath`/`endpointURL`. Nothing about the bucket or
   the prefix is hardcoded, so the destinationPath re-initializations each app
   went through (`…/immich-v2/`, `…/paperless-v2/`, `…/langfuse-v2/`) cannot be
   missed — a drill physically cannot restore a stale prefix.
3. **Copies the S3 credentials** Secret from the source namespace into the
   drill namespace (see `rbac.yaml` for why it is a copy and not a second
   SOPS file).
4. **Creates a drill `ObjectStore` + `Cluster`**: one instance, the recovery
   source pointing at the source cluster's `serverName` via the plugin
   parameter, `bootstrap.recovery` with **no `recoveryTarget`** — i.e. the
   latest base backup plus every archived WAL segment after it. The Cluster
   runs **without `spec.plugins`**, because re-enabling WAL archiving on the
   same `serverName` makes the plugin's `barman-cloud-check-wal-archive` refuse
   the setup while the remote archive is ahead of the restoring instance
   (`docs/data-protection.md`, drill 2 / finding #3). No sidecar means the
   drill also cannot archive to, or prune, the source prefix: it is read-only
   against the archive by construction.
5. **Waits for Ready**, then runs the database's invariant query **inside the
   recovered primary over the local Unix socket**
   (`kubectl exec … psql -U postgres -d <db>`). CNPG's fixed `pg_hba.conf`
   rules are `local all all peer map=local` and
   `host all all all scram-sha-256` (read off a live instance), so the socket
   path authenticates by OS user — the drill needs no database password, no
   application secret and no network path into the database.
6. **Pushes metrics**, then **deletes the Cluster, ObjectStore, Secret and
   PVCs**. Deleting the PVCs is what actually frees storage: the drill
   StorageClasses are `reclaimPolicy: Delete` (`storageclass.yaml`) precisely
   so a finished drill leaves no dataset on the NAS. An empty drill namespace
   is the normal steady state.

Guarantees worth stating explicitly:

- **Distinct cluster name**: the drill cluster is `<app>-drill-database` in
  `<app>-drill`, never the source name/namespace.
- **No external exposure**: the drill Cluster leaves only the `-rw` Service,
  and that is a ClusterIP; `r`/`ro` are disabled via
  `managed.services.disabledDefaultServices`. Nothing in the repo can reach it
  across namespaces (`cilium-allowlist.yaml` admits only kubelet probes,
  `cnpg-system` and the drill namespace's own pods).
- **Storage-bounded**: drills start at distinct staggered minutes and are
  serialised per database (`concurrencyPolicy: Forbid`), and each is sized like
  its source database (5–20 Gi). Overlap between two *different* databases is
  still possible if one run outlasts the gap to the next start (the recovery
  budget is 45 minutes inside a 90-minute `activeDeadlineSeconds`), so the
  bound is "at most one restored copy per database", not a global one.

## Per-database parameters and invariants

Every invariant below was read off the **live** database on 2026-09-18, not
guessed from an app's documentation. Row counts are recorded so the first drill
has something to compare against.

| app | source | drill | database | invariant (asserted) | domain metric (reported) | storage |
| --- | --- | --- | --- | --- | --- | --- |
| immich | `immich/immich-database` | `immich-drill/immich-drill-database` | `app` | `kysely_migrations` ≥ 1 (96 live) | `asset` (0 live) | nvmeof 20Gi |
| paperless | `paperless/paperless-database` | `paperless-drill/…` | `paperless` | `django_migrations` ≥ 1 (242 live) | `documents_document` (0 live) | nvmeof 10Gi |
| hindsight | `hindsight/hindsight-database` | `hindsight-drill/…` | `hindsight` | `alembic_version` ≥ 1 **and** `memory_units` ≥ 1 (40,021 live) | `memory_units` | nfs 10Gi |
| langfuse | `langfuse/langfuse-database` | `langfuse-drill/…` | `langfuse` | `_prisma_migrations` ≥ 1 (438 live) **and** `projects` ≥ 1 (1 live) | `projects` | nvmeof 10Gi |
| authentik | `authentik/authentik-database` | `authentik-drill/…` | `authentik` | `django_migrations` ≥ 1 (776 live) **and** `authentik_core_user` ≥ 1 (15 live) | `authentik_core_user` | nfs 5Gi |
| matrix | `matrix/matrix-database` | `matrix-drill/…` | `synapse` | `schema_version` ≥ 1 | `room_memberships` (reported; floor 0) | nfs 10Gi |

Justification, per database — the point is that the invariant must be true of a
*correct* restore and false of a broken one:

- **immich**: 2.x moved to Kysely; its public tables are lower-case singular
  (live: `activity`, `album`, `asset`, `asset_exif`, …, `kysely_migrations`).
  The TypeORM-era `assets` table no longer exists, and `asset` is **empty** in
  this deployment, so a rows > 0 rule on it would fail on a legitimately empty
  photo library. The migration ledger proves what a restore can prove here: a
  complete, migrated immich schema.
- **paperless**: paperless-ngx is Django, so `django_migrations` is the
  schema-completeness proof; `documents_document` (app `documents`, model
  `Document`) is the document table — 0 rows live only because no documents
  have been ingested yet, hence reported but not asserted.
- **hindsight**: the only ledger table in its schema is Alembic's
  `alembic_version`, so the schema assertion rides on that; the data assertion
  rides on `memory_units` (40,021 live), the app's central table — an
  intact-but-empty schema would be a useless restore for a memory service.
- **langfuse**: Prisma manages this schema, so `_prisma_migrations` is the
  ledger; `projects` is the root of langfuse's Postgres object graph (users,
  api_keys, models, prices, dashboards all hang off it), so losing it would
  make the restore worthless. **langfuse's traces/observations are not covered**
  — they live in ClickHouse, which has its own CronJob backup
  (`../langfuse/clickhouse-backup.yaml`).
- **authentik**: `django_migrations` (776 — the count verified intact after the
  2026-09-15 destructive restore) plus a user-count floor, because a
  single-instance IdP with an empty user table is an outage whatever the
  schema says. Note the deliberate asymmetry: `RECOVERY_OWNER` is **not set**
  for authentik, so the Cluster omits `bootstrap.recovery.database`/`owner`
  exactly as the production cluster's proven recovery spec does — the
  invariant still targets the real database (`-d authentik`), since the socket
  query authenticates as `postgres` and does not depend on CNPG's default
  application database.
- **matrix**: `schema_version` is Synapse's schema-version table (defined in
  `storage/schema/common/schema_version.sql`): a one-row table whose `version`
  column holds the schema version the database is at, written by
  `prepare_database` on both fresh init and every upgrade, and part of the
  `common` schema set so it exists on every physical database Synapse uses.
  `>= 1` therefore means "Synapse bootstrapped and versioned this database" —
  the closest thing Synapse has to a migration ledger. Its sibling
  `applied_schema_deltas` (one row per delta) would work too, but
  `schema_version` is the single authoritative row. The cluster is the first
  in the repo holding two databases (`synapse` and
  `matrix_authentication_service`, separate owners because the ESS chart
  forbids sharing one), and the drill parameterises one database per CronJob
  exactly as the other five do; MAS's schema is covered by the same backup, and
  its own ledger would only be a second parameterisation, not a second
  invariant. `room_memberships` is the natural domain table (rooms and their
  members are why the homeserver exists) but its asserted floor is **0**: this
  is a fresh deployment with no predictable row count yet, so `≥ 1` would fail
  on a legitimately empty homeserver while proving nothing. The reported
  `cnpg_restore_drill_rows{app="matrix",table="room_memberships"}` sample is
  what says when a real floor is available — bump `DOMAIN_MIN_ROWS` then.

## Metrics

Pushed (not scraped — a monthly Job leaves no target to scrape, and a pushed
sample stays queryable for the whole retention window) to the VictoriaMetrics
import endpoint that vmagent itself writes to:

```
http://vmsingle-monitoring-victoria-metrics-k8s-stack.monitoring.svc.cluster.local.:8428/api/v1/import/prometheus
```

| metric | labels | meaning |
| --- | --- | --- |
| `cnpg_restore_drill_success` | `app`, `database` | 1 = last run restored and passed its invariant; 0 = failed |
| `cnpg_restore_drill_last_run_timestamp_seconds` | `app` | when the last run finished, success or failure |
| `cnpg_restore_drill_last_success_timestamp_seconds` | `app` | when the last *successful* run finished (pushed only on success) |
| `cnpg_restore_drill_duration_seconds` | `app` | wall time of the run — the measured RTO |
| `cnpg_restore_drill_tables` | `app` | public-schema table count of the restored database |
| `cnpg_restore_drill_database_size_bytes` | `app` | `pg_database_size` of the restored database |
| `cnpg_restore_drill_rows` | `app`, `table` | row count of the invariant ledger table and of the domain table |

`cnpg_restore_drill_last_success_timestamp_seconds` plus a 3-month retention is
what makes "no successful drill in 32 days" answerable at any moment.

## Alerting

Appended to `../monitoring/rules.yaml` (the CNPG data-protection file, group
`restore-drills`):

| alert | fires when | severity |
| --- | --- | --- |
| `RestoreDrillStale` | no successful drill for an app in > 32 days | warning |
| `RestoreDrillFailed` | the last recorded run for an app failed (`cnpg_restore_drill_success == 0`) | critical |
| `RestoreDrillMetricsAbsent` | the success timestamp series is absent entirely (drills never ran, or retention expired) — the companion that keeps the other two honest | warning |

32 days is deliberately one monthly cycle plus slack: a single missed or failed
monthly run is visible, not silently absorbed. All three carry
`runbook_url: …/docs/disaster-recovery.md`.

## Running a drill by hand

Read this file's contract first; a manual run is the same script, only
triggered on demand.

```sh
# 1. start it (creates a Job from the CronJob's template)
kubectl -n cnpg-restore-drills create job \
  --from=cronjob/cnpg-restore-drill-immich "immich-drill-manual-$(date -u +%Y%m%dT%H%M%SZ)"

# 2. watch the log (metrics, invariant result, cleanup all land here)
kubectl -n cnpg-restore-drills get jobs
kubectl -n cnpg-restore-drills logs -f job/<job-name>

# 3. watch the drill objects appear (and disappear again)
kubectl -n immich-drill get cluster,pods,pvc -w
```

Expected duration: **~2–20 minutes**. The repo's measured hand-run restores are
123 s for a langfuse PITR clone and ~17 minutes for the authentik restore onto
a new storage class (`docs/data-protection.md`); a drill restores a fresh
volume, which is the slow part on NFS. The Job's own budget is 45 minutes of
waiting inside a 90-minute `activeDeadlineSeconds`, so a stalled restore is
reported, not hung. `cnpg_restore_drill_duration_seconds` is the real number —
record it.

Reading the result metric (no auth on the endpoint, same URL vmagent writes
to):

```sh
kubectl -n monitoring port-forward svc/vmsingle-monitoring-victoria-metrics-k8s-stack 8428 &
curl -sG 'http://127.0.0.1:8428/api/v1/query' \
  --data-urlencode 'query=cnpg_restore_drill_last_success_timestamp_seconds'
curl -sG 'http://127.0.0.1:8428/api/v1/query' \
  --data-urlencode 'query=cnpg_restore_drill_rows'
curl -sG 'http://127.0.0.1:8428/api/v1/query' \
  --data-urlencode 'query=time() - cnpg_restore_drill_last_success_timestamp_seconds'
```

Verifying the invariant by hand against a *running* drill (before the job
finishes it), i.e. the same query the script runs:

```sh
kubectl -n immich-drill exec immich-drill-database-1 -c postgres -- \
  psql -U postgres -d app -tAc 'select count(*) from kysely_migrations'
```

Keeping a failed drill for inspection: the default cleanup runs on success
*and* failure, because a failed drill must not leave a volume behind. When you
need the wreckage, run the Job with `KEEP_ON_FAILURE=1`:

```sh
kubectl -n cnpg-restore-drills create job --from=cronjob/cnpg-restore-drill-immich \
  immich-drill-keep --dry-run=client -o json \
  | jq '.spec.template.spec.containers[0].env += [{"name":"KEEP_ON_FAILURE","value":"1"}]' \
  | kubectl -n cnpg-restore-drills create -f -
```

Cleanup by hand (the same commands the script runs, in the same order):

```sh
kubectl -n immich-drill delete cluster immich-drill-database
kubectl -n immich-drill delete job -l cnpg.io/cluster=immich-drill-database
kubectl -n immich-drill delete objectstore immich-drill-backup-store
kubectl -n immich-drill delete secret immich-backup-s3-credentials
kubectl -n immich-drill delete pvc --all
```

A Job left `Failed` in `cnpg-restore-drills` after a failed run is expected
(`failedJobsHistoryLimit: 3`); read its log, the failure reason is the last
`drill FAILED (rc=…)` line and is followed by the cluster conditions and the
`--all-containers` logs of the recovery pods (recovery pods have no `postgres`
container, so `-c postgres` there would show nothing).

## First real drill: what must be confirmed

Nothing below could be verified without running a restore, which this change
deliberately does not do. Each item is a thing a first run either proves or
fails loudly on:

1. **Drill volumes provision at all** under the generic dataset parents
   (`fast/k8s/nvmeof`, `fast/k8s/nfs`). The parameter sets are copied from the
   working application classes, but no drill PVC has ever been provisioned; a
   failure here is a loud `ProvisioningFailed` on the PVC.
2. **`reclaimPolicy: Delete` + `forceDelete: "true"` really delete the volume**
   on TrueNAS when the drill deletes its PVC. If the driver keeps the dataset
   (the app classes are `Retain`+`forceDelete: false`, so this combination is
   new), the drill still frees the Kubernetes objects but a dataset per run
   leaks on the NAS — check
   `kubectl get pv -o custom-columns=PV:.metadata.name,NS:.spec.claimRef.namespace,STATUS:.status.phase`
   after the first run and confirm nothing is left `Released` in a `*-drill`
   namespace, then check the NAS.
3. **Backup paths per cluster are actually restorable**: the drill reads the
   prefix from the live ObjectStore and hands it to barman as
   `serverName=<source cluster>`; the `barman-cloud-restore` lines in the
   recovery pod's log are the proof that `base/<server-name>/` was found under
   the discovered `destinationPath`.
4. **`bootstrap.recovery` with `database`/`owner` set** (immich, paperless,
   hindsight, langfuse, matrix) is accepted — only authentik's variant (both
   omitted) has ever run. If CNPG rejects an owner that does not match the
   restored data directory, the fix is to omit the fields for that app exactly
   as authentik does.
5. **The `pods/exec` path works from the drill ServiceAccount** — the query
   itself was proven by hand against five live clusters on 2026-09-18, but the
   RBAC is new, so a first run is the first time the role is exercised.
6. **The metric push reaches VictoriaMetrics** through the new
   `monitoring-vmsingle-restore-drill-import` policy (the endpoint needs no
   auth — vmagent writes to the same URL unauthenticated — but the vmsingle pod
   denies cross-namespace peers unless named).
7. **Restore duration and storage headroom**, per database, on the home node
   and NAS: the drill sizes its volume like the source (5–20 Gi) and each
   database is serialised with itself, but a first run is what measures the
   wall clock and the actual data size
   (`cnpg_restore_drill_database_size_bytes`).
8. **immich's extension gap is benign**: the drill deliberately does not mount
   the `vchord` OCI extension image or preload `vchord.so`, so the vector index
   path is *not* exercised; the run must confirm the instance starts and the
   non-vector invariant passes (expected, since CNPG regenerates the instance
   configuration from the Cluster spec rather than from the restored data
   directory).

## Deliberately out of scope

- **Application-level connectivity**: the drill proves the *data* is
  restorable, not that an app can connect (that would need the app's own
  credentials and an app-namespace exec grant, which is a bigger privilege than
  this should hold). The `/` services of each app cover that separately.
- **ClickHouse (langfuse traces)**, the media/document libraries on TrueNAS and
  the etcd snapshot path: separate mechanisms with their own jobs —
  this directory covers CloudNativePG only.
- **PITR to a chosen point in time**: the drill always restores to "latest".
  Point-in-time selection is what `docs/data-protection.md`'s hand drills
  exercise; automating the marker-insert protocol would need writes to the
  production database, which a monthly unattended job must not do.
- **No freshness assertion**: the drill does not assert *how recent* the
  restored data is. Backup age is already covered by `BackupTooOld`,
  `WALArchivingStalled` and `BackupMetricsAbsent` in
  `../monitoring/rules.yaml`, and `pg_last_xact_replay_timestamp()` — the one
  obvious candidate — returns NULL on a promoted CNPG instance (checked live on
  the recovered authentik cluster), so no metric is shipped for it rather than
  shipping one that cannot be trusted.

## Files

| file | purpose |
| --- | --- |
| `namespace.yaml` | control namespace + six `*-drill` namespaces |
| `storageclass.yaml` | drill-only `reclaimPolicy: Delete` classes (nvmeof + nfs) |
| `serviceaccounts.yaml`, `rbac.yaml` | one SA per database; two namespaces each |
| `drill-config.yaml` | the harness (one script, `substitute: disabled`) |
| `cronjob-<app>.yaml` | per-database parameters, invariant and schedule |
| `cilium-default-deny.yaml`, `cilium-allowlist.yaml` | zero-trust baseline + allowlists + the vmsingle import rule |
| `kustomization.yaml` | everything above |

## Flux wiring (to add in `kubernetes/clusters/home/`)

This directory is inert until one Kustomization references it. The drill
manifests contain no CRs of their own, so the only ordering that matters is
that the CNPG operator (and with it the CRDs the *script* uses at run time) is
up before a drill can succeed:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: cnpg-restore-drills
  namespace: flux-system
spec:
  interval: 10m
  path: ./kubernetes/infrastructure/home/cnpg-restore-drills
  prune: true
  wait: true
  timeout: 10m
  retryInterval: 2m
  dependsOn:
    - name: cnpg-operator
  sourceRef:
    kind: GitRepository
    name: flux-system
  decryption:
    provider: sops
    secretRef:
      name: sops-age
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: cluster-vars
```
