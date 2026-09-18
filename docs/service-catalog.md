# Service catalog

One row per service in the cluster: who owns it, how it is exposed, what state
it holds, how that state is protected, and what it depends on. Everything is
owned by `self` — this is a single-operator homelab, and "owner" is only useful
as a hook for the parts that are not repository-managed (see
[External dependencies](#external-dependencies)).

Read this with [`docs/architecture.md`](architecture.md) for topology, trust
boundaries and failure domains, [`docs/data-protection.md`](data-protection.md)
for backup paths and measured restore timings, and
[`docs/hermes-agent-sandbox.md`](hermes-agent-sandbox.md) for the Hermes/Kata
isolation model.

**Exposure** means:

- **edge** — published on the Hetzner edge Gateway (public DNS), behind
  authentik SSO (edge outpost or OIDC) unless the row says otherwise.
- **LAN** — published on the internal Gateway at VIP `10.30.0.200`
  (`immich.ngoldack.de`, `registry.ngoldack.de`, `*.media.svc.…`); unreachable
  from the internet by design.
- **internal** — cluster-internal Service only (or a LAN VIP for scraping).
- **none** — no Service exposed beyond its own namespace.

`[unverified]` marks anything this repository does not settle; it is not a
placeholder for a guess.

## Services

| Service | Owner | Exposure | State | Backup | RPO / RTO | Dependencies |
| --- | --- | --- | --- | --- | --- | --- |
| `cilium` (CNI + Gateway data plane) | self | **is** the edge and LAN ingress datapath | none — `helm_release` in tofu state (`tofu/home/cilium.tf`, 1.20.1) | tofu state (Hetzner S3, encrypted) | n/a — rebuildable | Talos, Gateway API CRDs (tofu-owned), Hetzner worker |
| `flux` (`flux-system`) | self | none | Kustomizations/HelmReleases in etcd | etcd snapshot via `talos-backup` | RPO ≤24h; RTO `[unverified]` | Cilium, tofu-owned CRDs, GitHub (`ngoldack/homelab`) |
| `network` (Gateways `edge` + `public`, LB-IPAM, L2 announcements, wildcard `Certificate`, `ClusterIssuer`) | self | edge + LAN public door | CRs + TLS Secret in etcd | etcd; certificates re-issued by cert-manager | n/a | cert-manager, Cilium, external-dns |
| `cert-manager` + `cert-manager-webhook-hetzner` | self | none | CRs/Secrets in etcd | etcd; certs re-issued | n/a | Hetzner Cloud DNS API, Cilium |
| `external-dns` (two instances: `edge`, LAN) | self | none | TXT ownership registry in the Hetzner zone | the zone is the live state; no independent backup `[unverified]` | n/a | Hetzner Cloud DNS API, Gateways (`--gateway-name`), namespace labels |
| `truenas-csi` | self | none | PVs live on TrueNAS; all classes `reclaimPolicy: Retain` | storage layer itself (TrueNAS snapshots) | n/a | TrueNAS, NFS / NVMe-oF, privileged namespace |
| `cnpg-operator` + `plugin-barman-cloud` | self | none | operator CRs in etcd | etcd | n/a | cert-manager (plugin needs its CRDs), Hetzner Object Storage |
| `valkey-operator` | self | none | operator CRs (no persistence in either cluster) | deliberately not backed up — cache/broker only | n/a | — (v1alpha1, pre-production; no backup API) |
| `clickhouse-operator` | self | none | operator CRs | etcd | n/a | — |
| `seaweedfs-operator` | self | none | operator CRs | etcd | n/a | — |
| `monitoring-crds` | self | none | CRDs + HelmRelease | etcd | n/a | — |
| `monitoring` (VictoriaMetrics k8s-stack: vmsingle, vmagent, vmalert, `vmalertmanager`/Alertmanager, VictoriaLogs, Grafana, node-exporter, kube-state-metrics, blackbox-exporter) | self | Grafana: edge (`grafana.ngoldack.de`, outpost SSO); metrics/logs APIs internal | PVs 50 GiB + 20 GiB `truenas-fast-nfs-monitoring-vmsingle`; metric history deliberately **not** backed up | none — history is rebuildable, Alertmanager config is a SOPS Secret in git | n/a | `monitoring-crds`, Cilium, ntfy receivers (egress allowlisted for `vmalertmanager` only) |
| `homelab-infra-metrics` (unpoller + `vmsingle-push`) | self | internal — LAN VIP `10.30.0.202` (`:8428` Influx/remote-write, `:2003` graphite) | stateless | none — rebuildable | n/a | `monitoring` vmsingle, UDM Pro, Proxmox VE, PBS, TrueNAS |
| `security-baseline` (default-SA token projection, `flux-system` CNP, intra-namespace allow) | self | none | manifests only | etcd | n/a | Cilium |
| `kyverno` + `kyverno-policies` | self | none (admission webhook) | four Pod-shape policies + Hermes profile/quarantine policies, all `validationFailureAction: Audit` | etcd | n/a | Cilium, read RBAC into `hermes-sandbox` for the profile allowlist |
| `tetragon` + `tetragon-policies` | self | none — host-level eBPF observability (chart 1.7.1, privileged DaemonSet, host `/proc`) | TracingPolicies, all `Post`-only (nothing blocks) | etcd | n/a | Cilium, immutable Talos host (no rthooks daemonset) |
| `crowdsec` | self | none — detection only (no bouncer; Cilium-Gateway stack has no remediation path) | engine state `[unverified]` | `[unverified]` | `[unverified]` | Cilium, privileged namespace |
| `headlamp` | self | edge (`headlamp.ngoldack.de`, authentik proxy outpost) | stateless; token Secret | none — rebuildable | n/a | authentik outpost, kube-apiserver OIDC (authentik issuer) |
| `authentik` (server + worker + Rust proxy outpost + `portal-bridge` + LDAP outpost) | self | edge `authentik.ngoldack.de` + LAN route (`public-ingress` and `edge-ingress` labels) | CNPG `authentik-database` (2 instances, sync quorum) + valkey (no persistence) | Barman → S3 (ObjectStore, retention 30d); TrueNAS snapshot class `truenas-fast-nfs-authentik-database` 6h/14d | RPO ≤60s (WAL `archive_timeout`); measured RTO ~17 min clean restore, ~49 min app outage (2026-09-15 drill) | CNPG, valkey-operator, `network` (edge Gateway + outpost pinning), cert-manager |
| `agent-sandbox` (controller + authenticated Router) | self | none — Router is a ClusterIP | CRs in etcd | etcd | n/a | sandbox CRDs, Cilium router policy, router auth keys Secret |
| `hermes-sandbox` (`RuntimeClass kata`, three `SandboxTemplate` profiles, `hermes-go` warm pool replicas 1, per-profile Cilium classes, `secure-hermes-sandbox` VAP) | self | none | definitions in etcd; warm sandbox pod is ephemeral and holds no credentials | etcd (definitions only) — sandbox workspaces are disposable by design | n/a | `agent-sandbox`, Kata extension + nested virt on the P100 node, ValidatingAdmissionPolicy |
| `hermes` (gateway StatefulSet + supervised dashboard + stream relay) | self | edge `hermes.ngoldack.de` (dashboard OIDC via `dashboard_auth/self_hosted`, public PKCE client) | PVC 10 GiB `truenas-fast-nfs` (agent data); SOPS secrets for model key, HMAC, dashboard | none in-repo — agent workspace is disposable; checkpoint/artifact durability `[unverified]` | n/a | `agent-sandbox`, `hermes-sandbox`, `agentgateway` (model), `langfuse` (traces), `hermes-egress` (proxy HMAC secret) |
| `hermes-egress` (forward proxy `:3128`, `egress-authorizer`, `sandbox-reaper`) | self | none — ClusterIP; only `hermes-sandbox` pods may reach `:3128` | `hermes-quarantine` ConfigMap in `hermes-sandbox` (session hash → reason/strikes/TTL) | none — ephemeral ledger, single-key clear is the documented procedure | n/a | `hermes-sandbox` Cilium policy, `agent-sandbox` CRDs (reaper deletes claims), `kyverno` `hermes-session-quarantine`, HMAC secret shared with `hermes` |
| `langfuse` (web + worker) | self | edge `langfuse.ngoldack.de` (SSO) | CNPG `langfuse-database` (1 instance) + the ClickHouse/keeper/SeaweedFS/valkey rows below | CNPG Barman → S3, retention 30d | Postgres RPO ≤60s; measured PITR clone RTO 123 s (2026-09-15 drill) | CNPG, `clickhouse-operator`, `seaweedfs-operator`, standalone valkey, authentik, `monitoring` (self-instrumentation) |
| `langfuse-clickhouse` + keeper | self | none | PVs 5 GiB / 1 GiB on `truenas-fast-nfs` | TrueNAS snapshot classes exist, but these PVCs were created on the generic class and `storageClassName` is immutable → protection is class-availability only | effectively none until the volumes are recreated; `[unverified]` | `clickhouse-operator`, truenas-csi |
| `langfuse-seaweedfs` (S3 binaries) | self | none | PV on `truenas-fast-nfs` | same caveat as ClickHouse — snapshots not adopted on the bound PVC | effectively none until recreated; `[unverified]` | `seaweedfs-operator`, truenas-csi |
| `langfuse-valkey` (standalone Deployment, deliberately **not** the valkey-operator) | self | none | PVC 2 GiB `truenas-fast-nfs` | deliberately not backed up — cache/broker only | n/a | — |
| `hindsight` (+ local LLM) | self | edge `hindsight-ui.ngoldack.de` (outpost SSO) + LAN `hindsight.<domain>` | CNPG `hindsight-database` (1 instance, pgvector) + model weights | CNPG Barman → S3, retention 30d | RPO ≤60s; RTO `[unverified]` (no drill recorded) | CNPG, `agentgateway`/`llmkube` for the model, authentik |
| `immich` (+ machine-learning on the iGPU) | self | LAN only — `immich.ngoldack.de` at VIP `.200`, own local accounts (no OIDC, no outpost) | CNPG `immich-database` (2 instances) + library PVC 4 TiB RWX `truenas-tank-nfs-immich-library` + ML cache (not backed up) | Barman → S3, retention 30d; TrueNAS Periodic Snapshot Task on `tank` daily/1mo | DB RPO ≤60s; library RPO 24h; RTO `[unverified]` | CNPG, truenas-csi, `hardware/igpu` label, `network` (LAN Gateway) |
| `paperless` (ngx + tika/gotenberg stack) | self | edge `paperless.ngoldack.de` (SSO) + LAN label | CNPG `paperless-database` + valkey (PVC 2 GiB `truenas-fast-nfs-paperless-data`) + media/data classes | Barman → S3, retention 30d; document/media snapshot tasks `[unverified]` | DB RPO ≤60s; documents `[unverified]` | CNPG, valkey-operator, authentik, truenas-csi |
| `media` (jellyfin, jellyseerr, sabnzbd, sonarr, radarr, prowlarr, bazarr) | self | LAN only — `*.media.svc.ngoldack.de` at VIP `.200` | library 4 TiB RWX `truenas-tank-nfs-media-library` + config PVCs on `truenas-fast-nfs-media-config` | snapshot tasks `[unverified]` — the media library is **not** in the data-protection table | `[unverified]` | Multus + VLAN 2080 VPN egress, truenas-csi, authentik (jellyfin LDAP provider) |
| `registry` (Zot) | self | LAN only — `registry.ngoldack.de` at VIP `.200`, anonymous pull | PVC 100 GiB `truenas-fast-nfs-registry-zot`; pull-through cache for docker.io, registry.k8s.io, ghcr.io, quay.io | **reproducible, not backed up** — images are rebuilt by `image-builds` | n/a — rebuild | truenas-csi, Cilium egress to upstreams, `buildkit` (pushes) |
| `buildkit` (rootless) | self | none — mTLS client endpoint | `emptyDir` node-local cache; durable build cache is exported back to the registry | none — cache is disposable | n/a | cert-manager PKI (`Issuer`/CA in `kubernetes/infrastructure/home/buildkit/pki.yaml`), registry push credential (SOPS) |
| `image-builds` (BuildKit Jobs: hermes agent-sandbox plugin, hermes sandbox runtime, hermes-egress guard/authorizer/reaper, llama-p100, ik-llama, llama-kv-broker, github-runner) | self | none | one-shot Jobs (immutable; a rebuild is a new Job name) | none — artifacts are images in the registry | n/a | `buildkit`, `registry`, SOPS registry secret |
| `llmkube` + `llmkube-models` (`qwen36-35b` serving, `qwen3-27b` dormant at 0 replicas, `nomic-embed-v15` embeddings, `llama-kv-broker`) | self | internal — OpenAI-compatible API behind the gateway; gateway ingress allowed only to broker/qwen3/nomic on `:8080` | weights + KV-cache PVCs (`truenas-fast-nfs-llmkube-models` 100 GiB, `llama-kv-cache`) — deliberately disposable | **not backed up** — GGUF weights are re-downloadable | n/a | nvidia device plugin (time-slicing), P100 node label, `agentgateway` |
| `nvidia` device plugin | self | none | stateless DaemonSet | none — rebuildable | n/a | P100 node, privileged namespace, Talos NVIDIA extensions (LTS branch for Pascal) |
| `multus` (secondary CNI meta-plugin) | self | none | DaemonSet | none — rebuildable | n/a | Talos CNI config dir ownership, media NAD (VLAN 2080) |
| `agentgateway` + `otel-collector` | self | edge — `llm.aigateway.svc.ngoldack.de` with JWT auth (authentik issuer) | stateless — routes/backends/policies | none — rebuildable | n/a | authentik (JWT issuer), `llmkube` backends, Synthetic upstreams, `langfuse` (OTLP traces) |
| `talos-backup` | self | none | CronJob (daily 03:17 Europe/Berlin, `concurrencyPolicy: Forbid`) → Hetzner Object Storage, age-encrypted | **is** the etcd backup path | RPO ≤24h; RTO `[unverified]` until the control-plane rebuild rehearsal (Unit 5.4) | Talos API-access feature, S3 bucket, age private key in `tofu/home/secret.sops.yaml` (outside the cluster) |
| `github-runner` | self | none — egress to GitHub only | ephemeral: one job per pod, `emptyDir` workspace | none — rebuildable | n/a | GitHub repo + PAT Secret, `flux-system` Role (reconcile only), home-site node selector |

## Notes and gaps

- **Restore drills cover two of five CNPG clusters** (langfuse PITR clone,
  authentik destructive restore). `immich`, `hindsight` and `paperless` have the
  same Barman path and the same ≤60 s WAL bound but no measured RTO yet —
  `[unverified]`.
- **Base backups after the 2026-09-16 cluster re-creations**: `immich` and
  `paperless` produced no daily base backup on 09-17/09-18 even though their
  `ScheduledBackup` ticked (`lastScheduleTime` advanced), and every completed
  `Backup` object they still hold predates the re-creation, so it is worthless
  to the current cluster. Their barman sidecars logged
  `dial tcp 172.21.0.1:443: connect: connection refused` — the in-cluster
  API VIP — i.e. the failures line up with the control-plane reboot window, not
  with a backup configuration defect (`immich`'s Barman conditions read
  `ContinuousArchiving=True`, `LastBackupSucceeded=True`). On-demand base
  backups triggered on 2026-09-18 completed within a minute for both clusters,
  so the path works; the open item is the control-plane instability that makes
  a schedule tick miss, plus the fact that no catch-up exists.
- **etcd encryption at rest: verified configured.** `cluster.secretboxEncryptionSecret`
  is not written in `tofu/home/*.tf` — Talos *generates* it (`talos_machine_secrets`,
  `prevent_destroy = true`) and it lives in the encrypted OpenTofu state. The
  apiserver runs with `--encryption-provider-config` and a `secretbox` provider
  over `secrets`; a marker Secret was written, an etcd snapshot taken, and the
  marker was absent from the snapshot while every stored secret carried the
  `k8s:enc:secretbox:v1:` prefix — a plaintext control in the same snapshot
  proved the scan could see values that were not encrypted (2026-09-18 drill).
  ConfigMaps and CRs remain plaintext in etcd by design; etcd snapshots are
  themselves age-encrypted by `talos-backup`.
- **ClickHouse / keeper / SeaweedFS snapshots** are configured as storage
  classes but not adopted by the bound PVCs (immutable `storageClassName`);
  recreating those volumes is the published prerequisite.
- **External alert delivery: verified by observation.** Alertmanager routes to
  ntfy through SOPS config, and delivery was measured on 2026-09-18: alert
  notifications (BackupTooOld, KyvernoPolicyViolations, ContainerRestartLoop,
  PublicIngressProbeFailing, TalosBackupStale) arrived, and the Watchdog
  dead-man heartbeat arrived on its hourly cadence (five heartbeats in six
  hours, 65-minute gaps). Earlier reads that showed no deliveries were taken
  during the control-plane incident window, before the current Alertmanager
  pod existed — the delivery path is proven, not merely configured. The
  remaining weakness is the *receiver*: an ntfy topic is evidence a human
  looks at, not a page; a ping-based check URL is the stronger form (the
  config file documents the swap).
- **No Kubernetes API audit policy** exists (Talos default, nothing set in
  `tofu/home/`), so `pod-security.kubernetes.io/audit` labels and Kyverno
  PolicyReports are the only PSA drift signals.
- **Kata guest observability** is a documented gap: host Tetragon sees the VMM,
  not guest syscalls — see the Kata blind-spot section of
  [`docs/architecture.md`](architecture.md).
- **`crowdsec` persistence** and the **media/paperless library snapshot tasks**
  are `[unverified]` — not read out of the manifests for this catalog.

## External dependencies

Not repository-managed, but they are load-bearing; a failure in any of them is
a failure domain in [`docs/architecture.md`](architecture.md).

| Dependency | Used for | Repo-managed? |
| --- | --- | --- |
| Proxmox host `pmx-main` (`10.20.10.21:8006`) | All three LAN VMs and both passthrough devices | Declared in tofu; the host OS itself is not |
| TrueNAS (`tank`, fast pools) | Every PVC, including all CNPG databases | Storage classes in `kubernetes/infrastructure/home/truenas-csi/storageclasses.yaml`; pool/snapshot tasks are NAS-side |
| Hetzner Cloud | The `ingress-fsn1` worker + the public IPv4 serving all edge names | `tofu/home/ingress.tf` |
| Hetzner DNS API | Public zone records and the Let's Encrypt DNS-01 challenge | external-dns + cert-manager Hetzner webhook |
| Hetzner Object Storage | CNPG backups (`home-cnpg-backups-…`), etcd snapshots, tofu state | Buckets created by tofu (`tofu/home/backups.tf`, `state-backend.tf`); lifecycle is bucket-side |
| GitHub (`ngoldack/homelab`) | Source of truth for Flux, and the runner's job queue | Workflows + runner Deployment in-repo; the repo itself is not |
| UDM Pro / home network | VLANs, routing, the `.1` resolver | No repo footprint (the split-horizon resolver was retired 2026-09-13) |
| Authentik edge outpost chain | Login for every edge-served app | In-repo (`authentik/`, `network/edge.yaml`) but depends on the cloud node being up |
