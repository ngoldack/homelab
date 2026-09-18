# Disaster recovery

What this cluster can actually recover from, what has been **proven** by doing
it, and what is still only designed. The value of this document is the third
column: an untested backup is a hope, not a recovery plan.

Recovery targets, dependency maps and per-service RPO/RTO live in
[`service-catalog.md`](service-catalog.md); the backup layers themselves and
their retention are described in [`data-protection.md`](data-protection.md).

## Tested today

| Scenario | Mechanism | Evidence / measured |
| --- | --- | --- |
| Restore one application database from a Barman backup | `kubernetes/infrastructure/home/cnpg-restore-drills/` (daily per-database CronJob) | Harness verified offline against a stub API client (happy path + six failure modes). **No live run yet** — the first scheduled run populates `hermes_restore_drill_*` in VictoriaMetrics; the alert `restore-drills` fires if no run succeeds in 32 days. |
| Restore authentik destructively | manual CNPG restore | Performed 2026-09-15, `django_migrations = 776` after restore (recorded in `data-protection.md`). RTO ≈ 17 min. |
| Restore Langfuse Postgres by PITR clone | manual CNPG clone | Performed 2026-09-14/15. RTO ≈ 123 s. |
| Recover Secrets encrypted at rest | Talos `secretbox` provider | Verified 2026-09-18: a marker Secret written to etcd is absent from an etcd snapshot while every stored secret carries the `k8s:enc:secretbox:v1:` prefix, with a plaintext control in the same snapshot proving the check is sensitive. |

## Designed, not yet proven

| Scenario | Mechanism | What is missing |
| --- | --- | --- |
| etcd restore onto a rebuilt control plane | `talos-backup` CronJob → age-encrypted snapshot in Hetzner Object Storage | The apid egress fix (port 50000 on the `toEntities: kube-apiserver` rule) is committed but not merged, so the last successful snapshot is older than the fix; the rebuild itself (rebuild `cp-main` from OpenTofu, restore etcd, confirm workloads return) has never been rehearsed. |
| Restore a whole application namespace | Flux + Git + the above backups | Never rehearsed end to end; each app's prerequisite chain (CNPG, valkey, storage classes) is documented per service in `service-catalog.md`. |
| Recover from a TrueNAS loss | ZFS snapshots + Hetzner Object Storage backups | A second physical copy of the dataset does not exist; snapshot restore has not been drilled. |
| Rebuild the Cluster from Git | OpenTofu + Flux | The provisioning path is exercised at node level, not as a from-zero cluster build. |
| Recover a quarantined Hermes session | `hermes-egress` reaper ledger | The quarantine procedure is documented in `hermes-egress/README.md`; clearing a false positive has not been exercised on a live claim. |

## Drill procedure

The database drills are automated; the manual path — restore the latest backup
of one database into a throwaway Cluster, assert an invariant, report, clean
up — is documented step by step in
[`../kubernetes/infrastructure/home/cnpg-restore-drills/README.md`](../kubernetes/infrastructure/home/cnpg-restore-drills/README.md),
including the invariant chosen for each database and the reason it distinguishes
a good restore from a broken one.

Two properties of the drills are deliberate and worth keeping: every drill
namespace is suffixed `-drill` with a distinct Cluster name (so a drill can
never address a production volume), and the drill StorageClasses are
`reclaimPolicy: Delete` (so a finished drill leaves no dataset on the NAS).

## Credential rotation (compromise response)

The procedure below is for the case where cluster credentials have been
exposed — a machine config, a kubeconfig, a CA key or the service-account
signing key. It exists because the first instinct ("rebuild the cluster") is
not the only option: Talos can rotate the CAs in place, and the rebuild
framing only holds if the rotation below is judged unsafe on a single
control-plane cluster.

**Order matters, and so does the prerequisite.** Take a fresh etcd snapshot
first and confirm a recent CNPG base backup exists — every step below is
recoverable only from those. Plan a maintenance window: the control plane
restarts as certificates change.

The snapshot does not have to wait for the `talos-backup` CronJob: `talosctl
etcd snapshot <file>` works directly and is what the encryption drill used, so
a rotation can start even while the CronJob's egress fix is unmerged. Two
properties to handle: such a snapshot carries Secrets ciphertext under the
**leaked** key, so age-encrypt it (or delete it once the rotation is verified),
and any snapshot taken before the rotation keeps old-key ciphertext forever —
it is a recovery artifact, not a post-rotation source of truth.

1. **Rotate the CAs.** `talosctl rotate-ca` rotates both the Talos CA and the
   Kubernetes API issuing CA (each selectable). Its `--dry-run` defaults to
   **true**, so applying requires `--dry-run=false`, and `-o <file>` writes
   the new admin talosconfig — keep that file, the old credentials stop
   working. Read the synopsis: for Kubernetes it rotates only the API-server
   issuing CA.
2. **Rotate the Kubernetes PKI the command does not cover** by applying
   machine-config changes to the control-plane node: the aggregator CA, the
   etcd CA, the front-proxy certificates, and **`cluster.serviceAccount.key`**
   (the key that mints every service-account token). The apiserver restarts;
   kubelet re-issues projected tokens automatically, but long-lived
   `kubernetes.io/service-account-token` Secrets must be recreated.
3. **Re-issue client credentials**: node certificates, admin kubeconfigs and
   talosconfigs. Verify every node is Ready and that Flux reconciles before
   moving on.
4. **Rotate the etcd encryption key** (`cluster.secretboxEncryptionSecret`)
   with the two-key sequence: the new key becomes the primary provider while
   the OLD key stays configured as a reader, every Secret is rewritten
   (`kubectl get secrets -A -o json | kubectl replace -f -`), and only then
   is the old key dropped. Talos *generates* the apiserver
   `EncryptionConfiguration` from the machine config, so confirm the generated
   file lists both keys before and after the change — a straight value swap
   makes every existing Secret undecryptable.
5. **Rotate the remaining bearer material**: the Talos bootstrap token, and
   any capability token that lives in a SOPS file (for example the external
   alerting topic URLs — a known dead-man topic can be used to forge
   heartbeats and mask a dead pipeline). Re-encrypt those files when the
   values change.

Verification after each step: apiserver healthy, all nodes Ready, a pod
restart succeeds, a fresh etcd snapshot decrypts, and alert delivery still
arrives at the external destination.

## Open items

- The first live drill run, and its measured duration, must be recorded here —
  the table above says "designed" until then.
- The control-plane rebuild rehearsal (rebuild `cp-main`, restore etcd,
  confirm workloads) is destructive and therefore an operator decision; its
  duration belongs in this document.
- The talos-backup confirmation run after the apid egress fix merges.
- Offline escrow: the age identities and the OpenTofu state passphrase must
  exist outside the repo and outside the cluster (checklist in
  [`overview.md`](overview.md#offline-escrow-and-never-commit-checklist)).
