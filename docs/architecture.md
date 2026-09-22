# Architecture

One Talos + Cilium Kubernetes cluster spanning two sites: three Proxmox VMs on
the home LAN plus a single Hetzner Cloud worker joined over Talos KubeSpan.
Everything the cluster runs is declared in this repository and reconciled by
Flux; the CNI and the Gateway API CRDs are the deliberate exceptions, installed
by OpenTofu before Flux exists.

Per-service detail (owner, exposure, state, backup, RPO/RTO, dependencies) is in
[`docs/service-catalog.md`](service-catalog.md). The Hermes/Agent-Sandbox
isolation model is in [`docs/hermes-agent-sandbox.md`](hermes-agent-sandbox.md);
backup and restore procedures are in
[`docs/data-protection.md`](data-protection.md).

## Physical topology

One Proxmox host (`pmx-main`, i9-13900HX: 8 P-cores / 16 threads + 16 E-cores,
96 GiB installed / 94 GiB usable, `local-zfs`) carries the whole LAN fleet. The
Intel UHD 770 iGPU is passed through to the worker that uses it; the Tesla P100
and its passthrough were removed on 2026-09-22 with the local-LLM lane. Declared in `tofu/home/terraform.tfvars` (`proxmox_nodes`,
`nodes`); host reserve policy is 4 GiB RAM + 2 efficiency threads.

| Node | Site | Class | Threads | RAM | Passthrough | Placement labels / taint |
| --- | --- | --- | --- | --- | --- | --- |
| `cp-main` | home | efficiency (E) | 4 | 6 GiB | — | control plane |
| `wk-main-efficiency` | home | efficiency (E) | 10 | 28 GiB | Intel UHD 770 iGPU | `hardware/igpu`, `workload/media` (no taint, on purpose) |
| `wk-main-performance` | home | performance (P) | 16 | 48 GiB | nested KVM (Kata) | `workload.hermes.io/sandbox`, historical `node.kubernetes.io/instance-type=gpu-worker` (BuildKit selects on it) — explicitly untainted since 2026-09-15 |
| `ingress-fsn1` | Hetzner fsn1 | `cax11` (2 vCPU Ampere, ARM64) | 2 | 4 GiB | — | `dedicated=ingress:NoSchedule` |

Thread allocation is exact and intentional: 14/14 efficiency threads
(4 control plane + 10 worker, plus the 2 host-reserved) and 16/16 performance
threads on the performance worker. Fleet memory totals 82 GiB committed against the
host's 94 GiB usable, which is what leaves the host its real slack. Adding a
node means taking capacity from an existing one.

The Hetzner worker is an ordinary member of the same cluster — there is no
second cluster and no ClusterMesh. The cluster endpoint stays
`https://10.30.0.10:6443` (LAN), so nothing is forwarded on the home router:
all LAN nodes sit behind one NAT and dial out to the public node, which is
why inbound UDP 51820 on that node's firewall is load-bearing
(`tofu/home/ingress.tf`). All real workloads stay on the LAN; the cloud node
terminates TLS and proxies.

Cluster versions: Talos `v1.13.4`, Kubernetes `1.36.2` (`tofu/home/variables.tf`),
Cilium `1.20.1` as a tofu `helm_release` (`tofu/home/cilium.tf`).

## Networks

| Network | VLAN | Subnet | Used by |
| --- | --- | --- | --- |
| management | 20 | 10.20.0.0/24 | Proxmox host mgmt, IPMI, access points |
| server | 2010 | 10.20.10.0/24 | bare-metal services, NAS, the Proxmox API |
| vms | 3000 | 10.30.0.0/24 | Talos VMs (primary NIC; `cp-main` .10, `wk-main-performance` .22, `wk-main-efficiency` .23) |
| pods | — | 172.20.0.0/16 | pod CIDR, cluster-internal only |
| services | — | 172.21.0.0/16 | service CIDR, cluster-internal only |
| svc VIP | 3000 | 10.30.0.200–.250 | LAN LoadBalancer IPs, Cilium L2-announced |
| media secondary | 2080 | — | Multus macvlan attachment for the media download pods (VPN-routed egress) |

Pod MTU is **1370**: KubeSpan's WireGuard tunnel is 1420 on the WAN leg
(confirmed live) and Cilium's VXLAN adds ~50 bytes on top; at the default the
large-payload-blackhole failure mode appears across the WAN hop
(`tofu/home/cilium.tf`). Cilium datapath encryption is deliberately left off —
KubeSpan already encrypts node-to-node traffic, so Cilium's WireGuard/IPsec
would only double-encrypt.

LAN LoadBalancer IPs are allocated from the pool in
`kubernetes/infrastructure/home/network/lb-ipam.yaml` and ARP-announced by a
`CiliumL2AnnouncementPolicy` restricted to `topology.homelab/site=home`
(`network/l2-announcement.yaml`). The edge Gateway uses a separate cloud-side
pool (`network/edge.yaml`) on the public node.

## Repository layers

| Layer | Path | Owns |
| --- | --- | --- |
| Provisioning | `tofu/home/` | Proxmox VMs, the Hetzner worker, Talos machine configs + Image Factory boot images, the Gateway API CRDs (`gateway-api-crds.tf`), Cilium (`cilium.tf`), both Hetzner Object Storage buckets (tofu state, etcd backups) |
| Infrastructure | `kubernetes/infrastructure/home/` | Every workload namespace: one directory per service, each with its own Kustomization, SOPS secrets, Cilium default-deny + allowlist pair (e.g. `kubernetes/infrastructure/home/authentik/cilium-default-deny.yaml`) and namespace PSA labels |
| Cluster graph | `kubernetes/clusters/home/` | The Flux Kustomizations that wire the above in, with explicit `dependsOn` edges |

Ordering rules encoded in `kubernetes/clusters/home/kustomization.yaml`:
CRDs before CRs (so `*-crds` Kustomizations and operator Kustomizations
precede their consumers: `cert-manager` → `network`, `monitoring-crds` →
`monitoring`, `cnpg-operator` → backend apps, `clickhouse-operator` /
`seaweedfs-operator` → `langfuse`, `valkey-operator` → `authentik`,
`agentgateway-crds` → `agentgateway`, `kyverno` → `kyverno-policies`,
`tetragon` → `tetragon-policies`), and base before dependents
(`agent-sandbox` → `hermes-sandbox` → `hermes` → `hermes-egress`).
`hermes-sandbox` carries health checks on the warm pool, so a half-provisioned
sandbox blocks the gateway instead of reporting Ready.

## Trust boundaries

### Ingress

There is one public door and one LAN-only door, both terminated by Cilium's
Envoy data plane (`kubernetes/infrastructure/home/network/`):

- **Edge** (`network/edge.yaml`): the Hetzner worker's public IP
  (`2.28.31.116`) → Cilium Gateway `edge` → for SSO'd apps, the authentik
  **edge outpost** (Rust proxy outpost pinned to the cloud node by
  `nodeSelector`/toleration) → the app pod over KubeSpan. Authentik itself is
  reachable at `authentik.ngoldack.de` through the same path.
- **LAN** (`network/gateway.yaml`): the LoadBalancer VIP `10.30.0.200` →
  Cilium Gateway `public` → HTTPRoute → pod, with no auth hop. Only the
  LAN-only names use it (`immich.ngoldack.de`, `registry.ngoldack.de`, the
  media stack, the LAN hindsight route), and they are black holes from the
  internet by design.

A namespace may attach a route to a Gateway only if it carries the matching
label — `gateway.ngoldack.de/public-ingress: "true"` or
`gateway.ngoldack.de/edge-ingress: "true"`; nothing is labeled by default, and
`external-dns` has one instance per Gateway (`--gateway-name`) so an edge route
can never overwrite a LAN record. TLS is a wildcard
`*.ngoldack.de` / `*.svc.ngoldack.de` certificate issued by cert-manager via
Let's Encrypt DNS-01 through `cert-manager-webhook-hetzner`.

Envoy runs as an ordinary pod (`gatewayAPI.hostNetwork.enabled = false`) with
`externalTrafficPolicy: Local`; host networking on the cloud node could not
reach LAN pod IPs and produced 503s for cross-site backends.

Cross-site pod reachability over KubeSpan is known-unreliable for gateway
paths, so anything that must serve both sites follows the same-node bridge
pattern (`authentik-server` runs 2 anti-affine replicas, each fronted by a
same-node `portal-bridge` nginx that dials the `-local` Service). See
"Every request path stays site-local" in [`overview.md`](overview.md).

### Identity

Authentik is the single identity provider (`kubernetes/infrastructure/home/authentik/`),
seeded declaratively from
`kubernetes/infrastructure/home/authentik/seed.yaml`. An app takes exactly one of two paths:
the **edge outpost proxy** (headlamp, grafana, hindsight-ui — session check at
the edge) or **OIDC federation** (grafana, hermes dashboard via its own
`dashboard_auth/self_hosted` public-PKCE client, langfuse, paperless, Headlamp's
kube-apiserver OIDC chain). CNPG backs authentik's database; the Rust outpost is
the only component pinned to the cloud node.

### Workload isolation

- **Cilium per-namespace default-deny.** Each app directory ships its own
  default-deny + allowlist pair (for example
  `kubernetes/infrastructure/home/authentik/cilium-default-deny.yaml` and the
  `kubernetes/infrastructure/home/authentik/cilium-allowlist.yaml` beside it).
  There is deliberately **no** cluster-wide default-deny floor: a
  `CiliumClusterwideNetworkPolicy` was trialled on 2026-09-17 and rolled back
  the same day because Cilium evaluates cluster-wide policy inside the
  Gateway's Envoy as well, which made every HTTP route answer 403
  (`security-baseline/kustomization.yaml`). The operational consequence is
  explicit there: a new namespace starts open and must ship its own policy
  pair.
- **Pod Security Admission.** Talos enforces `baseline` cluster-wide by
  default. Namespaces that need more state it explicitly: `restricted` in
  `hermes`, `hermes-sandbox`, `hermes-egress`, `github-runner`;
  `privileged` where a host capability is genuinely required (`buildkit`,
  `crowdsec`, `media`, `monitoring-crds`, `tetragon`,
  `truenas-csi`); everything else carries `audit: restricted` + `warn:
  restricted` only. `flux-system` is a known gap (its Namespace object is
  generated by `flux bootstrap` and carries only `warn: restricted`).
- **Kyverno (audit-first).** `kyverno-policies/policies.yaml` holds
  `disallow-latest-tag`, `restrict-host-namespaces`, `disallow-host-path`,
  `disallow-privileged-containers` and the Hermes-specific
  `hermes-sandbox-profile-allowlist` + `hermes-session-quarantine`.
  `validationFailureAction` is `Audit` for all of them today; enforcement is a
  deliberate per-namespace decision, not a side effect of a rollout.
- **No API audit policy.** Talos leaves API audit logging off and nothing in
  `tofu/home/` sets `cluster.apiServer.auditPolicy`, so PSA `audit:` labels and
  Kyverno PolicyReports are the only drift signals (recorded in
  `security-baseline/kustomization.yaml`).

### The Hermes sandbox boundary

Hermes' model-generated commands never run in the gateway container. The
gateway (StatefulSet in namespace `hermes`) creates `SandboxClaim`s through a
scoped Role in `hermes-sandbox` only (claims + sandboxes, no pods/secrets/exec),
execs over gRPC into adopted sandboxes, and the `secure-hermes-sandbox`
ValidatingAdmissionPolicy (`agent-sandbox/policies/validating-admission-policy.yaml`,
binding `validationActions: [Deny]`, `failurePolicy: Fail`) admits only the
fixed sandbox shape: RuntimeClass `kata`, `automountServiceAccountToken: false`,
non-root, no host mounts. Three profiles exist today (`hermes-core`,
`hermes-go`, `hermes-offline`) and are image-identical by design — there is no
Python profile because the runtime image ships no interpreter; the profile
label is the key the per-profile Cilium egress classes select on and the
Kyverno allowlist (`hermes-sandbox-profile-allowlist`) enforces membership of
the fixed set. Hermes selects a
profile by name only — never image, runtime class, namespace, ServiceAccount,
mounts or network policy. Kata microVMs pin to the performance worker by label.

Sandbox internet access is mediated, not direct: a forward proxy
(`hermes-egress`, Envoy + deterministic authorizer + reaper) is the only path
out, session identity is an HMAC token minted by the gateway, violations
quarantine the session hash and delete its claims, and Cilium allows sandboxes
nothing but DNS + proxy `:3128`. See
[`docs/hermes-agent-sandbox.md`](hermes-agent-sandbox.md) and
`kubernetes/infrastructure/home/hermes-egress/README.md`.

## Failure domains

| Failure | Blast radius | Contained by | Recovery |
| --- | --- | --- | --- |
| Pod / container | One workload (or one replica) | Per-namespace default-deny, PSA, Kyverno policy reports, no cluster credentials in sandboxes | kubelet restart; Flux re-reconciles a drifted object |
| Worker node (`wk-main-*`) | Workloads on that node | Anti-affinity for authentik; Cilium L2 VIP leadership is per-service and Lease-backed, so the VIP fails over between home nodes | Drain/reboot; Talos machine config is reproducible from `tofu/home` |
| Control plane (`cp-main`) | API/etcd unavailable — cluster-wide write outage | Single-node control plane by design; no HA claim | Rebuild from `tofu/home` + restore etcd from the talos-backup bucket (needs the age key held outside the cluster — Unit 5.4 rehearsal not yet run) |
| Proxmox host (`pmx-main`) | **Everything on the LAN**: all three VMs, so all home workloads | Off-cluster backups (Hetzner Object Storage: CNPG barman, etcd, tofu state) | Rebuild host + VMs from tofu; restore etcd and the CNPG clusters. Single-host hardware is a known accepted risk (excluded from the review's repo-scoped plan) |
| Home internet / LAN | Public ingress still works (cloud node), but cross-site pod traffic and LAN clients | Edge routes that terminate on the cloud node (`authentik`, headlamp, grafana) keep serving until they need a home backend — none of them can reach it | Link restoration; the cloud worker still dials KubeSpan outbound |
| Hetzner worker (`ingress-fsn1`) | All edge/public names; LAN names keep working on the VIP | Nothing HA — single ingress node, and `externalTrafficPolicy: Local` means the VIP is not cloud-served | Rebuild via `tofu/home/ingress.tf` (Image Factory snapshot + KubeSpan join); DNS records are `external-dns`-owned and republished |
| TrueNAS (`tank`/fast pools) | All PVs: CNPG databases, media/document libraries, registry, monitoring | Storage classes are `reclaimPolicy: Retain` and never delete by default | ZFS/TrueNAS-side rollback or clone — a NAS operation, not a Kubernetes one |
| Git / Flux | No new state reaches the cluster; running workloads unaffected | Nothing depends on Git at runtime except reconciliation | Restore the repo, re-run `flux reconcile`; the self-hosted runner performs the post-merge reconcile from inside the cluster |
| Hetzner Object Storage | Backup and tofu-state paths only (no live traffic) | Backups are additive to TrueNAS/etcd; tofu state is versioned | Restore/promote from the bucket's versioning; offline escrow holds the state passphrase |

Data-protection specifics — which store has which backup path, retention and
measured restore timings — live in [`docs/data-protection.md`](data-protection.md).

## Observability limits and the Kata blind spot

Tetragon runs on the **hosts** (`kubernetes/infrastructure/home/tetragon/`,
chart 1.7.1, privileged DaemonSet with the host `/proc`), so its eBPF sensors
see node-level process and file events. Kata sandbox workloads are QEMU/Kata
microVMs with their own guest kernel: from the host, the sandbox workload is a
VMM process, and nothing in this repository runs a sensor inside the guest — no
tracing policy targets sandbox pods, and the one that attempted to (a
service-account-token read monitor for `hermes-sandbox`) was removed on
2026-09-17 because sandboxes carry no mounted token, so it could never fire
(`kubernetes/infrastructure/home/tetragon-policies/tracingpolicies.yaml`).
Guest-level syscall visibility for
Kata workloads is therefore **not** covered by the current sensor; the host-side
Kata/QEMU escape risk and the mixed-use sandbox node are documented in
[`docs/hermes-agent-sandbox.md`](hermes-agent-sandbox.md) ("Capacity").

**The guest-Tetragon prototype is not viable with the shipped Kata kernel.**
The plan gated it on the guest having BTF (any BPF CO-RE sensor needs
`/sys/kernel/btf/vmlinux`). The Kata kernel this node actually boots was
inspected directly — `talosctl -n 10.30.0.22 read
/usr/local/share/kata-containers/vmlinux.container` (the path named by
`configuration.toml`'s `kernel =`) — and the 40.6 MB ELF contains **no `.BTF`
section name and no BTF magic**, i.e. it was built without
`CONFIG_DEBUG_INFO_BTF`. It also carries no embedded kernel config
(`IKCFG_ST` absent), so the option cannot be re-read from the image. The
prototype therefore aborts per its own contingency rather than proceeding to a
sidecar that could never load a CO-RE program; a future attempt needs a Kata
kernel with BTF enabled (a rebuilt guest kernel, i.e. an image change on the
node), and until then "the sandbox is observed at runtime" stays
`[unverified]` — host Tetragon remains the sensor of record and sees the VMM,
not the guest.

Other visibility limits worth knowing before trusting a dashboard: Tetragon's
tracing policies are `Post`-only (nothing blocks —
`kubernetes/infrastructure/home/tetragon-policies/tracingpolicies.yaml`), no
Kubernetes API audit policy exists (above), and external alert delivery through
Alertmanager is configured with ntfy receivers but had delivered **zero**
notifications in the retention window as of the survivability dashboard's
2026-09-18T07:17Z data point
(`monitoring/dashboards/survivability.json`) — the Watchdog heartbeat is the
signal that distinguishes "no alerts" from "no delivery".

## See also

- [`docs/service-catalog.md`](service-catalog.md) — per-service owner, exposure,
  state, backup, RPO/RTO, dependencies.
- [`docs/data-protection.md`](data-protection.md) — backup paths, retention,
  alerts, restore drills and measured RTO/RPO.
- [`docs/hermes-agent-sandbox.md`](hermes-agent-sandbox.md) — Hermes isolation,
  claim RBAC, profile set, dashboard exposure and egress guard wiring.
- [`overview.md`](overview.md) — network layout, ingresses, SSO paths, tofu flow, bootstrap order.
- `kubernetes/clusters/home/kustomization.yaml` — the authoritative Flux
  ordering with the reasoning behind each edge.
