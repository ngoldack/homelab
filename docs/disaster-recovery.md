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

## Known failure mode: control-plane VM reboots (resolved root cause — host OOM, 2026-09-20)

`talos-bno-yij` (the sole control plane, `cp-main`) rebooted repeatedly with
the worker VMs unaffected. **Root cause confirmed via the host: the Proxmox
host was critically memory-exhausted and the kernel OOM killer was reaping VM
qemu processes.** Observed signature and blast radius:

- Each reboot produces a Kubernetes `Rebooted` node event and a window where
  the API refuses connections: in-cluster clients get `connect: connection
  refused` on `172.21.0.1:443`, workstation clients a timeout on
  `10.30.0.10:6443`.
- **Cascade:** each API blip makes the CloudNativePG operator exit(1) at
  startup (`failed to get server groups ... connection refused`; 91 restarts
  accumulated), which flips the `cnpg-operator` Flux Kustomization
  not-ready and blocks its dependents for minutes.

### Confirmed evidence (host-side, 2026-09-20)

- `free`: `94 total, 91 used, **0 free**, 2 available, **0 swap**` — and
  `Committed_AS` (93.6 GiB) at ~2x the kernel commit limit.
- OOM kills are real and recurring:
  `dmesg | grep 'Out of memory: Killed'` shows qemu/kvm processes for VMs
  104/105 killed on 2026-09-11/12 (anon-rss 50–67 GiB each).
- Ballooning is OFF for every VM (`memory.dedicated` in `tofu/home/main.tf`),
  so **allocated == committed** with no reclaim — the only safe lever was the
  allocation itself.
- ZFS ARC was not capped in practice: `arcstats c_max = 4.7 GiB` despite the
  module param; `/etc/modprobe.d/zfs.conf` previously asked for 2 GiB.
- Watchdog device: **absent** (ruled out). Backup job: `mode=snapshot` every
  2 h — snapshot mode does not reboot (ruled out).
- **Separate finding:** a Proxmox API token `root@pam!k8s` (and full
  `root@pam`) issued `qmreset`/`qmstop`/`qmstart` against VM 103 on
  2026-09-18 09:33–10:12. Identify that automation and stop it from touching
  the control plane.

### Remediation applied / queued (2026-09-20)

1. **Tofu budget (drafted, unapplied):** host floor = 6 GiB
   (`reserved.memory = 6`), VMs take the remaining 88 GiB (cp 8 + eff 32 +
   perf 48); `fleet_memory_within_host_ceiling` check enforces
   `fleet <= max_memory_gb - reserved.memory`.
2. **ARC:** `/etc/modprobe.d/zfs.conf` now `options zfs zfs_arc_max=1073741824`
   (1 GiB), backup at `zfs.conf.bak.20260920`. Binds at next module load
   (**requires a host reboot**).

### Reboot/apply runbook (one maintenance window)

```sh
# 1) Reboot pmx-main (binds the 1 GiB ARC cap; VMs return via onboot=1)
reboot

# 2) After boot, verify the ARC cap took and the host is at its 6 GiB floor
cat /proc/spl/kstat/zfs/arcstats | grep -E '^c_max '
free -g                                  # expect: used ~90 (84 VMs + 6 host)

# 3) Apply the tofu budget (grows wk-main-efficiency 28 -> 32 GiB; the VM
#    must restart for dedicated memory to change)
cd tofu/home && tofu plan && tofu apply -target=proxmox_virtual_environment_vm.talos_nodes

# 4) Verify the fleet and that no VM qemu is near OOM territory
free -g                                  # expect: 88 GiB VMs + 6 GiB host
tail -20 /var/log/pve/qemu-server/*.log 2>/dev/null
```

Until then. treat control-plane availability as degraded and re-run a stalled
`flux reconcile` / `task check` after any node return.

## Open items

- The control-plane VM reboot cause is **resolved** (host OOM, see the
  failure-mode section above) with the ARC cap + 6 GiB host floor queued.
  Remaining follow-up: identify and constrain the `root@pam!k8s` Proxmox
  token observed issuing `qmreset` on VM 103.
- The first live drill run, and its measured duration, must be recorded here —
  the table above says "designed" until then.
- The control-plane rebuild rehearsal (rebuild `cp-main`, restore etcd,
  confirm workloads) is destructive and therefore an operator decision; its
  duration belongs in this document.
- The talos-backup confirmation run after the apid egress fix merges.
- Offline escrow: the age identities and the OpenTofu state passphrase must
  exist outside the repo and outside the cluster (checklist in
  [`overview.md`](overview.md#offline-escrow-and-never-commit-checklist)).
