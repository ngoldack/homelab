# homelab

Talos + Cilium homelab, split across two independent clusters: a Proxmox
**home** cluster and a Hetzner Cloud **cloud** edge cluster that is the
sole public ingress point. Both are provisioned with OpenTofu and run
Flux for GitOps.

- Proxmox hosts declared as a map (`proxmox_nodes`)
- Talos clusters provisioned via OpenTofu (hand-rolled for home, the
  `hcloud-talos/talos/hcloud` registry module for cloud)
- Kubernetes bootstrapped with Cilium
- Secrets stored with SOPS + age
- Flux used as the GitOps layer for everything except each cluster's CNI/CCM

## v1 goal

Create a Talos Kubernetes cluster on Proxmox using OpenTofu, with independent
Proxmox hosts declared as a map (`proxmox_nodes`) — currently just `pmx-main`
— each with its own API endpoint, storage pool and bridge. A second,
independent OpenTofu root stands up a public Hetzner Cloud edge cluster that
terminates all internet-facing traffic.

## Repository layout

```text
.
├── .sops.yaml
├── .gitignore
├── age.key                  # local only, gitignored
├── README.md
├── Taskfile.yml
├── kubernetes/
│   ├── clusters/
│   │   ├── home/             # Flux Kustomization CRs for the home cluster
│   │   └── cloud/             # Flux Kustomization CRs for the cloud cluster
│   └── infrastructure/
│       ├── home/              # cilium, nvidia, truenas-csi
│       └── cloud/              # cert-manager, network, external-dns,
│                                 gateway-api-crds, headlamp
└── tofu/
    ├── home/                  # Proxmox + Talos, hand-rolled
    │   ├── main.tf            # Proxmox VMs, ISOs, Image Factory schematics
    │   ├── talos.tf           # Talos machine secrets/configs
    │   ├── providers.tf       # local state encryption + providers
    │   ├── variables.tf / terraform.tfvars
    │   ├── outputs.tf
    │   ├── secrets.tf         # loads secret.sops.yaml
    │   └── secret.sops.yaml
    └── cloud/                 # Hetzner Cloud + Talos
        ├── cloud.tf           # hcloud-talos/talos/hcloud module call
        ├── providers.tf
        ├── variables.tf / terraform.tfvars
        ├── secrets.tf
        └── secret.sops.yaml
```

## Prerequisites

- `age`
- `sops`
- `tofu`
- `task`
- `jq`
- `kustomize`
- `talosctl`
- `kubectl`
- `flux`
- `helm` (only if you want to render/inspect charts locally before they reconcile)
- access to the Proxmox host, for the home cluster
- a Hetzner Cloud account + API token, for the cloud cluster

## Network layout

The home network is split into dedicated VLANs, and the Kubernetes-internal
networks deliberately avoid 10.x so they can never collide with LAN ranges:

| Network              | VLAN | Subnet          | Used by                                  |
| -------------------- | ---- | --------------- | ----------------------------------------- |
| management           | 20   | 10.20.0.0/24    | Proxmox host mgmt, IPMI, access points    |
| server               | 2010 | 10.20.10.0/24   | bare-metal services, NAS, the Proxmox API |
| vms                  | 3000 | 10.30.0.0/24    | Talos VMs (primary NIC, tagged on vmbr0)  |
| k8s pods (internal)  | –    | 172.20.0.0/16   | pod CIDR, cluster-internal only           |
| k8s svc (internal)   | –    | 172.21.0.0/16   | service CIDR, cluster-internal only       |

Proxmox host NICs are trunk ports: untagged on management (VLAN 20),
tagged 2010/3000. Talos VMs get a single virtio NIC on the host's VM bridge
with the VM VLAN tag applied by Proxmox.

The VM VLAN ID, subnet prefix, gateway, nameservers and per-node static IPs
live in the public `network` variable in `tofu/home/terraform.tfvars`:

```yaml
network:
  vlan_id: 3000
  subnet_prefix: 24
  gateway: 10.30.0.1
  nameservers:
    - 10.30.0.1
    - 1.1.1.1
  node_ips:
    cp-main: 10.30.0.10
    wk-main-efficiency: 10.30.0.21
    wk-main-performance: 10.30.0.22
    wk-main-media: 10.30.0.23
```

### Home node roles and capacity

`pmx-main` (i9-13900HX: 8 P-cores/16 threads = "performance", 16 E-cores =
"efficiency", 96 GiB) is allocated to 100% by design. The host reserve is
2 GiB plus 2 efficiency threads, leaving 14 efficiency threads, 16 performance
threads and 94 GiB usable:

| node | class | threads | RAM | passthrough | taint |
| --- | --- | --- | --- | --- | --- |
| `cp-main` | efficiency | 4 (18–21) | 4 GiB | — | — |
| `wk-main-efficiency` | efficiency | 6 (22–27) | 16 GiB | — | — |
| `wk-main-media` | efficiency | 4 (28–31) | 8 GiB | Intel UHD 770 iGPU | `dedicated=media` |
| `wk-main-performance` | performance | 16 (0–15) | 64 GiB | Tesla P100 | `dedicated=ai` |

Efficiency threads sum to exactly 14/14 and memory to 92 GiB, leaving ~2 GiB
headroom. Because the host is fully allocated, **adding a node means taking
capacity from an existing one** — `wk-main-media` was carved out of
`wk-main-efficiency`, not out of the AI worker, whose P-cores and 64 GiB are
that node's entire purpose.

The two GPUs are deliberately on **separate** workers. Each node then has one
role and one taint, so a transcode cannot be starved by an inference job and
either capability can be rebooted without taking the other down; it also keeps
`i915` out of the AI node's boot image and the NVIDIA driver out of the media
node's. The media worker is intentionally small: a QuickSync transcode runs
almost entirely in the iGPU's fixed-function block, so its vCPUs only feed it
and demux/mux — which is also why E-cores are the right class for it.

Consolidating them back onto one worker would free ~4 threads and 8 GiB of VM
overhead, and is the sensible move only if the host stops being the
constraint (e.g. a second Proxmox host joins).

Each node in the `nodes` map in `terraform.tfvars` is declared individually
(name, host, `cpu_cores`, optional `cpu_affinity` pin, memory, disk, role).
The `nodes` keys must match the `node_ips` keys — one IP per node; nodes
without an entry fall back to DHCP. If `cpu_affinity` is set, it must span
exactly `cpu_cores` host cores (e.g. `cpu_cores = 6` with
`cpu_affinity = "18-23"`).

The cloud cluster's internal networking (172.30.0.0/16, subdivided into
node/pod/service ranges) is configured directly in `tofu/cloud/cloud.tf` and
does not overlap the home cluster's 172.20.0.0/16 / 172.21.0.0/16.

## Secret handling

This repo uses SOPS + age for local secret encryption. Keep credentials and the
OpenTofu state passphrases in `tofu/home/secret.sops.yaml` and
`tofu/cloud/secret.sops.yaml`; non-secret configuration belongs in each root's
`terraform.tfvars`.

1. Generate a local age key:

```bash
age-keygen -o age.key
```

2. Keep `age.key` local and ignored in Git via the repo `.gitignore`.

3. Edit encrypted files with SOPS:

```bash
task sops:edit FILE=tofu/home/secret.sops.yaml
task sops:edit FILE=tofu/cloud/secret.sops.yaml
task sops:edit FILE=kubernetes/infrastructure/cloud/cert-manager/secret.sops.yaml
task sops:edit FILE=kubernetes/infrastructure/cloud/network/secret.sops.yaml
```

The last two hold Hetzner credentials for cert-manager's DNS-01 solver and
external-dns respectively. Hetzner unified DNS zone management into the
Cloud API in November 2025 (the old standalone DNS Console can no longer
even create zones), so both now take the **same credential type** — a
Hetzner Cloud API token (console.hetzner.com) — and this repo reuses the
same `hcloud_api_token` value already in `tofu/cloud/secret.sops.yaml` for
both:

| Secret | Consumer | Credential type |
| --- | --- | --- |
| `cert-manager/secret.sops.yaml` (`hetzner`, key `token`) | official `hetzner/cert-manager-webhook-hetzner` | Hetzner Cloud API token |
| `network/secret.sops.yaml` (`hetzner-dns-token`, key `api-token`) | `external-dns-hetzner-webhook` | Hetzner Cloud API token (same value) |

If you'd rather scope DNS access to a separate, narrower token than the one
OpenTofu uses for server/network management, create a second Cloud API token
and use that instead — nothing requires reusing the exact same value, it's
just what this repo does by default.

`sops:edit` is the preferred workflow because SOPS creates and removes its
temporary plaintext copy itself. For a persistent local working copy, decrypt
only to an ignored `*.local.yaml` file, then encrypt it back into the tracked
`*.sops.yaml` file:

```bash
task sops:decrypt \
  FILE=tofu/cloud/secret.sops.yaml \
  OUTPUT=tofu/cloud/secret.local.yaml

# Edit the ignored local file, then atomically replace only the encrypted file.
task sops:encrypt \
  SOURCE=tofu/cloud/secret.local.yaml \
  FILE=tofu/cloud/secret.sops.yaml

rm tofu/cloud/secret.local.yaml
```

The Taskfile rejects plaintext paths that do not end in `.local.yaml` or
`.local.yml`, rejects encrypted targets that do not end in `.sops.yaml` or
`.sops.yml`, and never decrypts an encrypted file in place. All local working
copies are ignored by Git — remove them once you're done editing, they are
not needed afterward:

```bash
find tofu kubernetes -type f -name '*.local.yaml' -delete
```

To create local working copies for every tracked secret in one operation, run:

```bash
task sops:decrypt:all
```

It creates an ignored sibling for each encrypted file. It refuses to overwrite
any existing local copy and first verifies that all encrypted sources decrypt.

Use `task sops:check:all` to verify every encrypted OpenTofu and Kubernetes
file decrypts. Use `task sops:updatekeys:all` after changing `.sops.yaml`; it
rewrites all encrypted files using the current recipient rules.

`.sops.yaml` scopes two separate recipient sets: the `tofu/` rule (your local
key + CI) and the broader `kubernetes/` rule (your local key, CI, and both
clusters' Flux age keys — `home-flux.age.key` / `cloud-flux.age.key`). Both
clusters' Flux keys can currently decrypt **every** Kubernetes secret in the
repo, including the other cluster's — home's Flux can read cloud's Hetzner
DNS credentials and vice versa. If you want per-cluster secret isolation,
split the `kubernetes/` rule into `kubernetes/infrastructure/home/.*\.sops\.yaml$`
and `kubernetes/infrastructure/cloud/.*\.sops\.yaml$` path_regexes with
distinct recipient lists, then `task sops:updatekeys:all`.

CI's `ci.age.key` is a recipient in the `kubernetes/` rule, meant for a CI
workflow to `task sops:check:all` on every push — but as of now, **no such CI
workflow exists** (see "Validation" below); this recipient is provisioned but
unused. Print its public recipient with:

```bash
task sops:keys:ci:public
```

`ci.age.key` is *also* a recipient on the `tofu/(home|cloud)` rule — pushed as
the single `SOPS_AGE_KEY` GitHub Actions secret
(`gh secret set SOPS_AGE_KEY < ci.age.key`), it's what lets
`talos-etcd-backup.yml` decrypt everything it needs straight from these same
files at runtime, rather than duplicating individual values into separate
GitHub secrets (see "Talos etcd backups" below).

Both `tofu/home/secret.sops.yaml` and `tofu/cloud/secret.sops.yaml` now hold
the same shape of keys — each root owns its own etcd backup bucket and its
own state bucket (see "Remote encrypted state" below), so each needs its own
`hetzner_object_storage_access_key`/`_secret_key` and its own
`tailscale_auth_key` (home's `cp-main` and both cloud nodes all join the same
tailnet, for the etcd-backup workflow — see "Talos etcd backups"). Each also
carries a `<cluster>_talosconfig` key (its own exported talosconfig content)
and, home only, the two Proxmox credentials.

## OpenTofu flow

The Home and Cloud roots are fully independent: `tofu/home` manages Proxmox
and the Home Talos cluster; `tofu/cloud` manages the Hetzner Cloud edge
cluster. Run whichever root's init/plan/apply you need — they don't call
each other.

### Minimizing Proxmox host (OS) memory reservation

The tofu config pins guest RAM (`memory.dedicated` only, no ballooning) and the
capacity check lets guests consume `max_memory_gb` minus each host's
`reserved.memory` (default reserve: 2 GiB). The OS reservation itself is
**host-level** and not managed by the bpg provider — apply it once per
Proxmox host:

```bash
# Cap ZFS ARC so it cannot grow into guest memory (default can reach ~50% RAM).
# 1 GiB ARC; permanent via /etc/modprobe.d/zfs.conf
echo "options zfs zfs_arc_max=$((1 * 1024**3))" > /etc/modprobe.d/zfs.conf
echo "$((1 * 1024**3))" > /sys/module/zfs/parameters/zfs_arc_max   # live

# Discourage host swapping so the reserve stays free for guests.
sysctl -w vm.swappiness=10
echo "vm.swappiness=10" > /etc/sysctl.d/99-homelab.conf

# Disable KSM page-sharing: it costs host CPU/scan overhead and Talos guests
# share almost nothing, so it rarely pays off here.
systemctl disable --now ksmtuned 2>/dev/null || true
```

The default `reserved.memory = 2` (GiB) assumes a headless host with a capped
ARC. If you skip the ARC cap, raise the reserve (e.g. 4–8 GiB) per host in
`terraform.tfvars` so the host keeps enough for ZFS.

The Home Talos lifecycle lives directly in `tofu/home/talos.tf`, alongside the
Proxmox resources in `tofu/home/main.tf`. From a blank state:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
task tofu:home:init
task tofu:home:plan
task tofu:home:apply
```

The Cloud root, independently:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
task tofu:cloud:init
task tofu:cloud:plan
task tofu:cloud:apply
```

`task tofu:cloud:apply` provisions, in one apply: the ARM64 Talos Image
Factory snapshot, the HCloud network/firewall/servers (via
`hcloud-talos/talos/hcloud`), Talos bootstrap, Cilium and the HCloud
cloud-controller-manager (both applied once as raw manifests and left
permanently under tofu's ownership — see "Cloud ingress" below), Cloud's own
etcd backup bucket, and everything Flux needs before it can bootstrap (the
`flux-system` namespace and `sops-age` Secret — see "Flux bootstrap" below).

`task tofu:home:apply` is the same story on the Home side: Proxmox VMs, Talos
bootstrap, Cilium (tofu-owned here too, not just adopted by Flux — see "Flux
bootstrap"), Home's own etcd backup bucket, and the same `flux-system`
namespace/`sops-age` Secret pair.

**Cost note:** once Cilium's Gateway API integration creates the public
Gateway's backing `Service` (type `LoadBalancer`), the HCloud
cloud-controller-manager provisions a real, billed Hetzner Load Balancer for
it automatically — this happens the moment Flux applies
`kubernetes/infrastructure/cloud/network/gateway.yaml`, not as a visible line
in any `tofu plan`.

## Remote encrypted state

Each root stores its state in its **own** dedicated Hetzner Object Storage
bucket (`home-tofu-state-<random>` / `cloud-tofu-state-<random>`, via an `s3`
backend block pointed at Hetzner's S3-compatible endpoint) — never shared,
never local. State is still encrypted the same way as before the move: AES-GCM
with a distinct SOPS-encrypted PBKDF2 passphrase per root. OpenTofu's
`encryption { state {...} }` block operates on the state document itself,
before/after it's handed to whichever backend stores the bytes — moving from
local disk to a remote bucket needed **no change** to that block at all.

The Taskfile decrypts the passphrase (and, now, the Hetzner Object Storage
credentials the `s3` backend itself needs — backend blocks can't reference
`var.`/`local.`, so these have to come from `AWS_ACCESS_KEY_ID`/
`AWS_SECRET_ACCESS_KEY` env vars) in memory and passes them to OpenTofu
through temporary environment variables; nothing is ever written to disk or
exported as a persistent shell variable. Before running a Taskfile OpenTofu
command, export the SOPS key:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
```

Each root's own state bucket is itself a tofu-managed resource
(`state-backend.tf`), which means bootstrapping either root from nothing has
a real chicken-and-egg step: the bucket a backend points at has to exist
*before* that backend can be initialized against it. Resolved with a one-time,
two-step sequence per root — after that, `task tofu:*:init` behaves like any
other remote-backend root forever:

```bash
# 1. Create just the state bucket, before any backend block references it.
tofu -chdir=tofu/home apply \
  -target=aws_s3_bucket.home_tofu_state \
  -target=aws_s3_bucket_versioning.home_tofu_state

# 2. Add/uncomment the backend "s3" {} block in providers.tf with that
#    bucket's now-known literal name (see state-backend.tf's own comment),
#    then migrate:
task tofu:home:init  # or: tofu -chdir=tofu/home init -migrate-state
```

Repeat for `tofu/cloud`. Hetzner's endpoint has real, repeatable
eventual-consistency lag on a brand-new bucket (read-after-create, not just
write-after-create) — if step 1 or 2 fails right after the bucket is created,
wait a short while and retry rather than assuming something is misconfigured;
verify the bucket independently (`aws s3 ls --endpoint-url
https://fsn1.your-objectstorage.com`, or the Hetzner Console) before deciding
otherwise.

The Terraform inputs are intentionally minimal and are centered on:

- independent Proxmox nodes (`proxmox_nodes`: endpoint, token, storage pool,
  ISO datastore, bridge — there is no shared cluster API endpoint)
- cluster name and Talos/Kubernetes versions
- pod/service CIDRs (cluster-internal, non-10.x)
- per-node definitions (host, size, CPU class/affinity, GPU passthrough)

## Flux bootstrap

Both clusters come out of `tofu apply` fully ready for `flux bootstrap` —
nothing manual left to do first. This was not always true, and the two things
that used to require it are worth understanding:

**Cilium.** Home has no built-in CNI (Talos ships with none), and Flux's own
controllers can't schedule anything without a working CNI — so *something*
has to install Cilium before Flux ever runs. Cloud always solved this by
having tofu own Cilium permanently (`deploy_cilium = true` in the
`hcloud-talos` module, applied as raw manifests, never Flux-managed). Home now
does the exact same thing (`tofu/home/cilium.tf`, a plain `helm_release`
resource) — both clusters get their CNI from `tofu apply`, and neither has a
`kubernetes/infrastructure/*/cilium/` Flux HelmRelease at all, since there's
nothing left for Flux to adopt.

**The `sops-age` Secret.** Flux needs this Secret to exist in `flux-system`
before it can decrypt anything (`cluster-vars`, `cert-manager`'s DNS
credentials, etc.). Both roots now create it directly
(`tofu/*/flux-bootstrap.tf`: a `kubernetes_namespace` for `flux-system` + a
`kubernetes_secret` populated from that cluster's own `*-flux.age.key`) —
`flux bootstrap` is idempotent against a pre-existing namespace/secret, so it
just finds both already there.

Both are wired through `kubernetes`/`helm` providers configured directly from
`talos_cluster_kubeconfig`'s own resource attributes (the same
resource-attribute-backed provider pattern the vendored `hcloud-talos` module
already uses internally for its own post-bootstrap providers) — so these
resources only get created once the cluster is actually up and its kubeconfig
is known, in the right order, automatically.

With that done, bootstrapping Flux is just:

```bash
KUBECONFIG=kubeconfig-home.yaml flux bootstrap github \
  --owner=<your-user> \
  --repository=<your-repo> \
  --branch=main \
  --path=kubernetes/clusters/home \
  --personal
```

```bash
KUBECONFIG=kubeconfig-cloud.yaml flux bootstrap github \
  --owner=<your-user> \
  --repository=<your-repo> \
  --branch=main \
  --path=kubernetes/clusters/cloud \
  --personal
```

One thing Flux still can't do for itself: Talos's own controller can never
self-apply a node taint (Kubernetes' NodeRestriction admission plugin forbids
a node from setting its own taints — confirmed on a brand-new node's very
first join, not just pre-existing ones). Rather than a manual `kubectl taint`
step, `kubernetes/infrastructure/home/node-taints/` is a Flux-managed
one-shot Job that does it instead, selecting nodes by their
`node.kubernetes.io/instance-type` label rather than by name (Talos assigns
each node a random generated hostname, so there's no stable name to target).
This is the one place in this repo that runs `kubectl` directly rather than
through tofu or a native Kubernetes resource — because nothing else *can* set
a taint here — but it's still 100% Flux-applied code, not an operator running
anything by hand.

## Minimal cluster contents

- **Home**: Proxmox VM provisioning, Talos machine secrets/configs, an
  Image-Factory-built boot image per node's resolved extension set, Cilium
  (tofu-owned, same as Cloud — see "Flux bootstrap"), a Flux-managed Job that
  applies the two node taints Talos itself can never self-apply (see
  "Known limitations"), the NVIDIA device plugin (AI worker only),
  TrueNAS-CSI storage classes. Three distinct extension sets are in play — a
  shared base, base + `i915` for the media worker, and base + the NVIDIA
  driver/toolkit for the AI worker — so each node boots the smallest image
  that serves it.
- **Cloud**: `hcloud-talos/talos/hcloud`-provisioned HCloud network/firewall/
  servers, Cilium + HCloud CCM (owned by tofu, not Flux — see "Cloud
  ingress"), cert-manager + the Hetzner DNS webhook, the Gateway API CRDs,
  the public Gateway, external-dns, Headlamp (internal-only by default).

### System extensions: the ISO is not enough

Getting a Talos extension onto a node takes **two** references to the same
Image Factory schematic, and missing the second one fails silently:

1. `proxmox_download_file.talos_iso` uses `urls.iso` — the boot media.
2. `machine.install.image` (both patch blocks in `talos.tf`) uses
   `urls.installer` — the image that actually writes the system to disk.

The ISO's extensions live only in the live/maintenance boot. Whatever the
*installer* contains is what survives to the installed system, so if
`machine.install.image` is left unset Talos falls back to the stock
`ghcr.io/siderolabs/installer` and every extension is discarded the moment
the node installs and reboots. Nothing errors — the node comes up healthy and
joins the cluster; it just has no `qemu-guest-agent` (so `wait_for_ip` in
`main.tf` times out on every subsequent plan, adding ~10 min and eventually
nulling out `current_ip`), no NVIDIA driver (`KernelModuleSpecController`
retries `module not found` forever), and no `nfs-utils` for truenas-csi.
`urls.installer` is also version-pinned, so it is what makes `talos_version`
govern the installed OS rather than only the generated machine config.

Verify with `talosctl -n <ip> get extensions` — an empty result means the
installer reference is missing. Because this is install-time, changing it on a
running node requires re-running the installer:
`talosctl upgrade --nodes <ip> --image <urls.installer value>` (roll workers
first, control plane last).

## Storage safety policy

TrueNAS-backed dynamic provisioning is intentionally conservative by default:

- NFS shares must never be deleted by default
- NVMe-oF volumes must never be deleted by default
- Dynamic provisioning is allowed to create and expand volumes, but not remove them automatically
- Any destructive cleanup must be explicit and human-reviewed

The storage classes therefore use `reclaimPolicy: Retain` and `forceDelete: "false"` for every TrueNAS-backed class so creating workloads is easy, while accidental data loss is not.

## Topology

Two independent Talos clusters, not one cluster spanning two sites:

- **home**: `pmx-main` (Proxmox, private IPs, control plane + workers)
- **cloud**: a Hetzner Cloud Talos control plane + worker with a public IP —
  the sole public ingress point

Home's node-to-node traffic uses Talos KubeSpan (WireGuard mesh) purely for
transport encryption — it does not carry pod-to-pod traffic (that stays with
Cilium's own VXLAN overlay; see `advertiseKubernetesNetworks` in
`tofu/home/talos.tf`). Cloud does not use KubeSpan. There is currently no
cross-cluster networking (no ClusterMesh, no shared private addressing) —
each cluster is reached and operated independently.

## Validation

Before deployment, check the structure:

```bash
tofu fmt -check -recursive tofu
task tofu:home:validate
task tofu:cloud:validate
kustomize build kubernetes/clusters/home
kustomize build kubernetes/clusters/cloud
yamllint .
```

There is currently no CI workflow that runs these checks automatically (the
only workflow in `.github/workflows/` is the etcd backup cron) — running them
locally before pushing is on you for now.

Common workflows are wrapped in the Taskfile (`task --list`):

| Task                                    | Purpose                                  |
| --------------------------------------- | ---------------------------------------- |
| `task setup`                            | Create the local age key, print env      |
| `task sops:edit FILE=<path>`            | Edit any SOPS-encrypted file safely      |
| `task sops:decrypt FILE=<file> OUTPUT=<local>` | Create an ignored plaintext working copy |
| `task sops:decrypt:all`                 | Create ignored working copies for all secrets |
| `task sops:encrypt SOURCE=<local> FILE=<file>` | Atomically encrypt a working copy       |
| `task sops:check:all`                   | Verify every encrypted file decrypts     |
| `task sops:updatekeys:all`              | Rekey every encrypted file               |
| `task tofu:home:init` / `plan` / `apply` / `destroy` | Home OpenTofu lifecycle |
| `task tofu:cloud:init` / `plan` / `apply` / `destroy` | Cloud OpenTofu lifecycle |
| `task kubeconfig:home:export` / `task kubeconfig:cloud:export` | Write cluster-specific kubeconfigs |
| `task talosconfig:home:export` / `task talosconfig:cloud:export` | Write cluster-specific talosconfigs |
| `task talos-backup:home:bucket` / `task talos-backup:cloud:bucket` | Print the generated backup bucket names |

---

## Developer notes

This repo intentionally stays small and opinionated:

- keep only the bootstrap path required to bring up Talos + Cilium
- add storage and application layers only when they are needed by the cluster
- keep secrets encrypted with SOPS + age and never commit plaintext API keys
- prefer explicit human review for destructive actions such as deleting storage

If you need to add a workload later, add it in a single, purpose-built layer rather than reintroducing broader platform scaffolding.

## Cloud ingress

Home and Cloud are independent Kubernetes clusters, each operated on its own
(there is no cross-cluster networking yet — see "Topology"). Cloud has one
schedulable control plane and one worker, both public-IP HCloud nodes, and is
the only public edge. Cloud nodes run the `siderolabs/tailscale` Talos system
extension; add a reusable or ephemeral Tailscale auth key as
`tailscale_auth_key` in the encrypted `tofu/cloud/secret.sops.yaml` before the
first apply — the key is passed to Talos as `TS_AUTHKEY` and never committed
in plaintext.

Public traffic path:

```text
client → Hetzner DNS → A record → Hetzner Load Balancer (auto-provisioned
       by hcloud-ccm for the Gateway's Service)
       → Cilium Gateway (envoy) :443, TLS = Let's Encrypt wildcard
  → HTTPRoute → ClusterIP Service → pod
```

- TLS is terminated on the Gateway with a single wildcard cert
  (`*.svc.<domain>` + `svc.<domain>`) issued by cert-manager via Let's Encrypt
  **DNS-01** through `cert-manager-webhook-hetzner`.
- external-dns creates per-app A records from Gateway HTTPRoutes, scoped to
  `svc.<domain>` so it cannot touch unrelated DNS. It runs the
  `external-dns-hetzner-webhook` sidecar (external-dns 0.15.x dropped the
  built-in Hetzner provider).
- Only namespaces labeled `gateway.ngoldack.de/public-ingress=true` may
  attach an HTTPRoute to the public Gateway — nothing is labeled by default,
  so label a namespace explicitly to expose a workload:
  `kubectl label namespace <ns> gateway.ngoldack.de/public-ingress=true`.

### Cilium and the HCloud CCM are owned by tofu, permanently

Talos starts with no CNI, so `tofu/cloud/cloud.tf` bootstraps Cilium and the
HCloud CCM by rendering their charts and applying the manifests directly
(`kubectl_manifest`, `apply_only = true`) — not via a real Helm release. This
is deliberate and permanent, not a bootstrap-then-handoff step: those
manifests carry no Helm ownership metadata, so a Flux `HelmRelease` over the
same objects in `kube-system` would fail its own install with an ownership
conflict (`invalid ownership metadata`). There is accordingly **no** Flux
`HelmRelease` for Cilium or the CCM on the cloud cluster.

To upgrade either, bump `cilium_version` / `hcloud_ccm_version` (or
`cilium_values`) in `tofu/cloud/cloud.tf` and re-apply — same as any other
tofu-managed resource. Flux still manages every other cloud workload
(cert-manager, external-dns, headlamp, the Gateway/HTTPRoutes, the Gateway
API CRDs); it just never touches `kube-system`'s CNI/CCM.

**Known gap — Gateway API needs two manual steps, and timing does not fix it.**
The Gateway API CRDs are installed by Flux
(`kubernetes/infrastructure/cloud/gateway-api-crds/`), not by `tofu apply`, so
Cilium is always installed before they exist. Two independent consequences,
both confirmed against the live cluster:

1. **The operator's CRD probe is one-shot.** `cilium-operator` checks for the
   Gateway API CRDs once at startup; a missing CRD is treated as a
   non-transient error, so it marks the controller degraded and reports
   `Enabled: false` for the lifetime of the process. Bootstrapping Flux
   "promptly" does not help — the operator never re-probes. After
   `gateway-api-crds` reconciles you must restart it explicitly:
   `kubectl -n kube-system rollout restart deployment/cilium-operator`.
2. **No `GatewayClass` is ever rendered.** The `hcloud-talos` module renders
   Cilium client-side via `data "helm_template"`, so the chart's
   `gatewayClass.create: "auto"` capability check cannot see the cluster and
   never fires (`kind: GatewayClass` appears zero times in tofu state). Set
   `gatewayAPI.gatewayClass.create = "true"` in `cilium_values` explicitly —
   but only once the CRDs are present, or the manifest apply fails.

Until both are done, `kubernetes/infrastructure/cloud/network/gateway.yaml`
(`gatewayClassName: cilium`) never becomes Accepted and gets no address, so
external-dns publishes nothing. Note this fails *silently*: the `network`
Kustomization's `wait: true` still reports Ready, because kstatus treats a
custom resource with an empty status as Current.

## Talos etcd backups

`.github/workflows/talos-etcd-backup.yml` uploads Home and Cloud
control-plane etcd snapshots daily to separate Hetzner Object Storage
buckets, running in GitHub Actions rather than inside either cluster so Talos
administrative credentials are never exposed to cluster workloads.

Both jobs run on ordinary GitHub-hosted `ubuntu-24.04` runners — **no
self-hosted runner**. Each job joins the tailnet at runtime via
`tailscale/github-action`, using the same `tailscale_auth_key` tofu uses to
enroll actual cluster nodes, then reaches its control plane over its tailnet
hostname rather than a public IP. This is what makes a GitHub-hosted runner
viable for the Home job at all (its Talos API is on a private LAN) and sidesteps
Cloud's firewall entirely (`:50000` there is restricted to a single source IP —
see "Known limitations" — which the tailnet path never touches). It's why
`cp-main` carries the `siderolabs/tailscale` extension too, not just Cloud's
nodes.

Every credential either job needs (Hetzner Object Storage keys, each cluster's
talosconfig, the Tailscale auth key) is decrypted at runtime from the same
sops-encrypted files tofu itself reads
(`tofu/home/secret.sops.yaml`/`tofu/cloud/secret.sops.yaml`), via a single
`SOPS_AGE_KEY` repository secret — `ci.age.key`'s private key, added as a
recipient in `.sops.yaml` specifically for this. Nothing is duplicated into
separate per-value GitHub secrets; there is exactly one secret to manage
(`gh secret set SOPS_AGE_KEY < ci.age.key`), and rotating it is the same
`sops updatekeys` + re-push as rotating any other recipient.

Each cluster's talosconfig is stored as a value inside its own sops file too
(`home_talosconfig` / `cloud_talosconfig`) — a one-time export
(`tofu output -raw <cluster>_cluster_talosconfig`, `sops --set`) that only
needs redoing if that cluster's Talos machine secrets are ever regenerated
from scratch (they carry `prevent_destroy`, so this is rare). Each job's `TALOS_TAILNET_HOST` env var is the target node's Tailscale IP
(`talosctl -n <ip> get addresses`, the `tailscale0` interface) rather than
its hostname — deliberately, so the workflow doesn't depend on the tailnet
having MagicDNS enabled, which can't be verified from outside it. That IP is
stable for the life of the node's tailnet registration, and only needs
updating if a node is ever removed from the tailnet and rejoins as a new
device (a from-scratch reinstall, most likely).

The workflow removes its temporary talosconfig and etcd snapshot before
either job finishes, and fails fast (`: "${VAR:?...}"` guards) on a missing
secret rather than a confusing several-seconds-later parse error.

**Before either job can succeed, a newly-joined tailnet device needs manual
approval in the Tailscale admin console** (`machineAuthorized`), exactly like
any new node — this includes `cp-main` the first time it joins. Reusing the
same reusable auth key for CI runs as for real node enrollment is a
deliberate simplification: mark that key **Ephemeral** in the Tailscale admin
console so CI-joined devices clean themselves up after each run, rather than
accumulating as permanent tailnet members.

Hetzner Cloud's OpenTofu provider does not manage Object Storage buckets. This
repository uses the maintained AWS provider against Hetzner's S3-compatible API
to create both etcd backup buckets *and* both roots' own state buckets (see
"Remote encrypted state"). Before the first apply of either root, create an
Object Storage access key in the Hetzner Console for `fsn1`, then add its
values to **both** `tofu/home/secret.sops.yaml` and
`tofu/cloud/secret.sops.yaml` — each root owns its own etcd backup bucket now
(`home_talos_backups` lives in `tofu/home`, not `tofu/cloud` — it moved
there because it's Home's data, and Home has its own AWS-provider access
anyway for its own state bucket):

```yaml
hetzner_object_storage_access_key: <access-key>
hetzner_object_storage_secret_key: <secret-key>
```

Every bucket (both etcd backup buckets, both state buckets) is versioned and
protected with `prevent_destroy`. None has a lifecycle/expiration policy:
`aws_s3_bucket_lifecycle_configuration` cannot be applied against Hetzner's
S3-compatible endpoint (a confirmed upstream provider bug — see the comment
above `home_talos_backups` in `tofu/home/backups.tf`), so objects accumulate
forever until one is set manually via the Hetzner Console or
`aws s3api put-bucket-lifecycle-configuration`.

Provision everything and export each cluster's Talos configuration:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
task tofu:home:init
task tofu:home:apply
task tofu:cloud:init
task tofu:cloud:apply
task talosconfig:home:export
task talosconfig:cloud:export
```

Add each cluster's own talosconfig into its own sops file (a one-time step,
only needed again if that cluster's Talos machine secrets are ever
regenerated from scratch):

```bash
sops --set '["home_talosconfig"] '"$(python3 -c 'import json,sys;print(json.dumps(open("talosconfig-home.yaml").read()))')" \
  tofu/home/secret.sops.yaml
sops --set '["cloud_talosconfig"] '"$(python3 -c 'import json,sys;print(json.dumps(open("talosconfig-cloud.yaml").read()))')" \
  tofu/cloud/secret.sops.yaml
```

Set the single `SOPS_AGE_KEY` GitHub Actions repository secret (see "Secret
handling") and use **Run workflow** for the initial snapshot. Each bucket
stores objects named `etcd-<UTC timestamp>.snapshot`.

### Bootstrap order

1. Add a Hetzner Cloud API token to the `cert-manager` and `network`
   encrypted Secrets (see the table in "Secret handling") — reuse
   `tofu/cloud/secret.sops.yaml`'s `hcloud_api_token`, or a separate,
   narrower-scoped token if you'd rather not share one.
2. Add a Tailscale auth key to **both** `tofu/home/secret.sops.yaml` and
   `tofu/cloud/secret.sops.yaml` as `tailscale_auth_key`.
3. Add a root@pam password for `pmx-main` to `tofu/home/secret.sops.yaml` as
   `proxmox_pmx-main_root_password` (needed for `cpu.affinity`, which Proxmox
   rejects from any API-token-authenticated request).
4. Bootstrap each root's own remote state bucket (see "Remote encrypted
   state"), then `task tofu:home:apply` and `task tofu:cloud:apply` — this
   brings up both clusters fully, including Cilium and the `sops-age` Secret
   Flux needs (see "Flux bootstrap"). Nothing manual in between.
5. Bootstrap Flux independently at `kubernetes/clusters/home` and
   `kubernetes/clusters/cloud` (see "Flux bootstrap" — no pre-steps needed).
6. On Cloud, after the `gateway-api-crds` Kustomization reconciles, run
   `kubectl -n kube-system rollout restart deployment/cilium-operator` — the
   operator only probes for the Gateway API CRDs once, at startup, and will
   otherwise leave its Gateway controller disabled forever. See the "Known
   gap" note above; this step is required, not a precaution. (This is the
   one other place this repo runs `kubectl` directly instead of through tofu
   or Flux — restarting a Deployment isn't expressible as a resource to
   converge toward, only as an action to take once, after a specific event.)

### Verification

- Cloud: `kubectl --context cloud get nodes` shows both nodes Ready.
- Home: `kubectl --context home get nodes` shows all nodes Ready, and
  `kubectl -n kube-system get pods -l k8s-app=cilium` shows tofu's own Cilium
  release healthy.
- Both: `flux get all` shows every Kustomization/HelmRelease Ready, and
  `kubectl -n kube-system get job node-taints` (Home) shows `Complete`.
- Certificate: `kubectl get certificate -n network` shows `wildcard-svc-tls` Ready.
- Gateway: `kubectl get gateway -n network public` shows Programmed with an address.
- DNS: `kubectl logs -n network deploy/external-dns` and `dig app.ns.svc.<domain>` → the Load Balancer's IP.
- TLS end-to-end: `curl -v https://<app>.<ns>.svc.<domain>` presents the Let's Encrypt cert.

## Known limitations / follow-ups

- **Talos/node upgrades**: `hcloud-talos/talos/hcloud` sets
  `lifecycle { ignore_changes = [user_data, image, iso] }` on cloud's servers,
  so bumping `talos_version` alone does not upgrade running nodes — use
  `talosctl upgrade` / `talosctl upgrade-k8s` for in-place upgrades, per the
  module's own operational guidance.
- **Firmware/chipset**: every home VM uses `machine = "q35"` + `bios = "ovmf"`
  (with the required `efi_disk`), deliberately uniform across the fleet
  rather than only on the PCIe-passthrough node. Every VM also gets a
  `serial0` socket with `vga { type = "serial0" }`: once a passed-through
  GPU's driver (e.g. `i915`) loads, it takes over the emulated display, and
  Proxmox's own Console tab would otherwise freeze on the last framebuffer
  frame — pointing the console at the serial port keeps it live for every
  node, not just the one with PCIe passthrough.
- **Headlamp** has no public route by default (its pod ServiceAccount is
  bound to `cluster-admin`, and no OIDC provider is configured) — reach it via
  `kubectl -n headlamp port-forward`. Re-enable `httpRoute` only after wiring
  up `config.oidc.*` with a real identity provider.
- **Insecure TLS to the Proxmox API** (`insecure = true` by default) trusts
  Proxmox's typical self-signed certificate; if you've issued a real one,
  set `insecure = false` per host in `proxmox_nodes`.
- **Registry rate limiting**: some Hetzner IP ranges hit container registry
  rate limits; if image pulls start failing on the cloud cluster, this is a
  known cause.
- **`tofu plan` is never clean for `tofu/cloud`, and applying it disturbs the
  control-plane VIP.** The module hard-codes `alias_ips = []` on the
  control-plane server (`server.tf`, citing
  `hetznercloud/terraform-provider-hcloud#650`), but Talos claims the VIP
  `172.30.1.100` — `cidrhost(node_ipv4_cidr, 100)`, the internal control-plane
  endpoint — on that same private interface at runtime through the Hetzner API.
  Neither side yields, so every plan shows an in-place server diff wanting to
  strip the alias. Applying it *does* strip the VIP until Talos re-claims it,
  briefly breaking the internal control-plane endpoint. Nothing in this repo
  can fix it without forking the module, so: treat a non-empty cloud plan as
  expected, read the diff before applying, and expect a short VIP gap on any
  cloud apply.
- **Talos can never self-apply a node taint — this repo doesn't use
  `machine.nodeTaints` at all any more.** Talos does not pass taints through
  kubelet's `--register-with-taints`; `k8s.NodeApplyController` patches
  labels *and* taints onto the Node object afterwards, using the kubelet's
  own identity. Kubernetes' NodeRestriction admission plugin forbids a node
  from setting its own taints, so that patch is rejected — and because
  labels ride the same atomic patch, **the node also gets none of its
  labels**. This affects every node carrying `nodeTaints`, including
  brand-new ones (verified: `wk-main-media` hit it on its very first join,
  not just already-registered nodes).

  Rather than a manual `kubectl taint` workaround, `tofu/home/talos.tf`
  never sets `machine.nodeTaints` at all, and
  `kubernetes/infrastructure/home/node-taints/` is a Flux-managed one-shot
  Job that applies the two taints instead — selecting nodes by their
  `node.kubernetes.io/instance-type` label rather than by name (Talos
  assigns each node a random generated hostname, so there's no stable name
  to hard-code). This is the one place in the repo that runs `kubectl`
  directly rather than through tofu or a native Kubernetes resource, but
  it's Flux-applied code, not an operator running anything by hand — a
  fresh bootstrap needs zero manual intervention for this. Check with
  `talosctl -n <ip> logs controller-runtime | grep NodeApplyController`
  (should be quiet) and `kubectl -n kube-system get job node-taints`
  (should show `Complete`). Symptom of something actually wrong: a node
  that's `Ready` but has only the five stock `kubernetes.io/*` labels.
- **A new tailnet device needs manual approval before it's reachable at
  all.** Every node running the `siderolabs/tailscale` extension (both cloud
  nodes, and `cp-main` for the etcd-backup workflow) sits in `ext-tailscale`'s
  restart-forever loop — `talosctl -n <ip> logs ext-tailscale` shows
  `machineAuthorized=false`, `NeedsMachineAuth` — until you approve it in the
  Tailscale admin console. This is unrelated to whether the auth key itself
  is valid; it happens on every first join, cluster rebuild included.
