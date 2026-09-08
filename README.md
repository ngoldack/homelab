# homelab

Minimal green-field Talos + Cilium setup for a Proxmox homelab.

This repo is intentionally stripped down to the smallest useful v1 state:

- Proxmox hosts declared as a list
- Talos cluster provisioned via OpenTofu
- Kubernetes bootstrapped with Cilium
- Secrets stored with SOPS + age
- Flux used only as a lightweight GitOps bootstrap layer

## v1 goal

Create a Talos Kubernetes cluster on Proxmox using OpenTofu, with the
independent Proxmox hosts declared as a map (`proxmox_nodes`) covering
`pmx-main`, `pmx-infra` and `pmx-ai` — each with its own API endpoint,
storage pool and bridge.

The repo does not include future-phase AI, observability, storage, offsite, or cloud layers.

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
│   │   ├── cluster-vars.yaml
│   │   ├── infrastructure.yaml
│   │   ├── network-config.yaml
│   │   ├── kustomization.yaml
│   │   └── cluster-vars/
│   └── infrastructure/
│       └── controllers/
│           ├── cilium/
│           ├── truenas-csi/
│           ├── kustomization.yaml
│           └── sources.yaml
└── tofu/
    ├── main.tf              # Proxmox VMs/ISOs + talos module call
    ├── cloud.tf             # Hetzner Cloud ingress worker
    ├── cloudflare.tf        # Cloudflare zone/DNS/bot settings + scoped token
    ├── providers.tf         # local state encryption + providers
    ├── variables.tf / terraform.tfvars
    ├── outputs.tf
    ├── secrets.tf           # loads secret.sops.yaml
    ├── secret.sops.yaml
    └── modules/
        └── talos/           # reusable Talos bootstrap module (secrets, configs, kubeconfig)
```

## Prerequisites

- `age`
- `sops`
- `tofu`
- `jq`
- `talosctl`
- `kubectl`
- `flux`
- access to the Proxmox cluster

## Network layout

The home network is split into dedicated VLANs, and the Kubernetes-internal
networks deliberately avoid 10.x so they can never collide with LAN ranges:

| Network              | VLAN | Subnet          | Used by                                  |
| -------------------- | ---- | --------------- | ---------------------------------------- |
| management           | 20   | 10.20.0.0/24    | Proxmox host mgmt, IPMI, access points   |
| server               | 2010 | 10.20.10.0/24   | bare-metal services, NAS                 |
| vms                  | 2011 | 10.20.11.0/24   | Talos VMs (primary NIC, tagged on vmbr0) |
| k8s pods (internal)  | –    | 172.20.0.0/16   | pod CIDR, cluster-internal only          |
| k8s svc (internal)   | –    | 172.21.0.0/16   | service CIDR, cluster-internal only      |

Proxmox host NICs are trunk ports: untagged on management (VLAN 20),
tagged 2010/2011. Talos VMs get a single virtio NIC on the host's VM bridge
with the VM VLAN tag applied by Proxmox.

The VM VLAN ID, subnet prefix, gateway, nameservers and per-node static IPs
live in the public `network` variable in `tofu/terraform.tfvars`:

```yaml
network:
  vlan_id: 3000
  subnet_prefix: 24
  gateway: 10.30.0.1
  nameservers:
    - 10.30.0.1
    - 1.1.1.1
  node_ips:
    cp: 10.30.0.10
    wk-main-efficiency: 10.30.0.21
    wk-main-performance: 10.30.0.22
    wk-infra: 10.30.0.23
```

Each node in the `nodes` map in `terraform.tfvars` is declared individually
(name, host, `cpu_cores`, optional `cpu_affinity` pin, memory, disk, role).
The `nodes` keys must match the `node_ips` keys (`cp`, `wk-main-efficiency`, `wk-main-performance`,
`wk-infra`) — one IP per node; nodes without an entry fall back to DHCP.
If `cpu_affinity` is set, it must span exactly `cpu_cores` host cores
(e.g. `cpu_cores = 6` with `cpu_affinity = "18-23"`).

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
task sops:edit FILE=kubernetes/infrastructure/cloud/network/secret.sops.yaml
```

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
copies are ignored by Git.

To create local working copies for every tracked secret in one operation, run:

```bash
task sops:decrypt:all
```

It creates an ignored sibling for each encrypted file, such as
`tofu/cloud/secret.local.yaml` and
`kubernetes/clusters/cloud/sops-age.local.yaml`. It refuses to overwrite any
existing local copy and first verifies that all encrypted sources decrypt. When
you finish, remove all plaintext working copies with:

```bash
find tofu kubernetes -type f -name '*.local.yaml' -delete
```

Use `task sops:check:all` to verify every encrypted OpenTofu and Kubernetes
file decrypts. Use `task sops:updatekeys:all` after changing `.sops.yaml`; it
rewrites all encrypted files using the current recipient rules.

CI uses a separate ignored `ci.age.key`. Its public recipient is included only
in Kubernetes `*.sops.yaml` files, never OpenTofu secrets. Add the complete
contents of `ci.age.key` to the CI secret `SOPS_AGE_KEY`, then export it as the
`SOPS_AGE_KEY` environment variable in the CI job. Print its public recipient
with:

```bash
task sops:keys:ci:public
```

## OpenTofu flow

The Home and Cloud roots are independent: `tofu/home` manages Proxmox and the
Home Talos cluster; `tofu/cloud` manages the single HCloud Talos edge cluster.

### Minimizing Proxmox host (OS) memory reservation

The tofu config pins guest RAM (`memory.dedicated` only, no ballooning) and the
capacity check lets guests consume `max_memory_gb - os_reserved_memory_gb`
(default reserve: 2 GiB). The OS reservation itself is **host-level** and not
managed by the bpg provider — apply it once per Proxmox host:

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

The default `os_reserved_memory_gb = 2` assumes a headless host with a capped
ARC. If you skip the ARC cap, raise the reserve (e.g. 4–8 GiB) in
`terraform.tfvars` so the host keeps enough for ZFS.


The Home Talos lifecycle lives directly in `tofu/home/talos.tf`, alongside the
Proxmox resources in `tofu/home/main.tf`.

From a blank state, provision the Talos cluster with:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
task tofu:home:init
task tofu:home:plan
task tofu:home:apply
```

Or via the Taskfile:

```bash
task tofu:cloud:init
task tofu:cloud:plan
task tofu:cloud:apply
```

## Local encrypted state

OpenTofu stores state locally in `tofu/home/terraform.tfstate` and
`tofu/cloud/terraform.tfstate`, encrypting each with AES-GCM and a distinct
SOPS-encrypted PBKDF2 passphrase. Keep both state files and all age private
keys out of Git, with offline backups.

The Taskfile decrypts only the passphrase in memory and passes it to OpenTofu
through the temporary `TF_VAR_state_encryption_passphrase` environment
variable; it never writes the passphrase to disk or exports it as a persistent
shell variable. Before running a Taskfile OpenTofu command, export the SOPS key:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
```

Initialize the local state:

```bash
task tofu:home:init
task tofu:cloud:init
```

The Terraform inputs are intentionally minimal and are centered on:

- independent Proxmox nodes (`proxmox_nodes`: endpoint, token, storage pool,
  ISO datastore, bridge — there is no shared cluster API endpoint)
- cluster name and Talos/Kubernetes versions
- pod/service CIDRs (cluster-internal, non-10.x)
- Talos node pool definitions (per-pool host, size, GPU passthrough)

## Flux bootstrap

After the cluster is up:

```bash
KUBECONFIG=kubeconfig.yaml kubectl create secret generic sops-age \
  --namespace=flux-system \
  --from-file=age.agekey=age.key

KUBECONFIG=kubeconfig.yaml flux bootstrap github \
  --owner=<your-user> \
  --repository=<your-repo> \
  --branch=main \
  --path=kubernetes/clusters \
  --personal
```

## Minimal cluster contents

This repo intentionally keeps only the components required to stand up a working Cilium-based Talos cluster:

- Proxmox VM provisioning
- Talos machine secrets and machine configs
- Cilium HelmRepository and HelmRelease
- Flux bootstrap entrypoints for the cluster

Everything else is deliberately removed to keep the project understandable and easy to extend later.

## Storage safety policy

TrueNAS-backed dynamic provisioning is intentionally conservative by default:

- NFS shares must never be deleted by default
- NVMe-oF volumes must never be deleted by default
- Dynamic provisioning is allowed to create and expand volumes, but not remove them automatically
- Any destructive cleanup must be explicit and human-reviewed

The storage classes therefore use `reclaimPolicy: Retain` and `forceDelete: "false"` for every TrueNAS-backed class so creating workloads is easy, while accidental data loss is not.

## Topology

One Talos cluster spanning two sites:

- home: `pmx-main`, `pmx-infra`, `pmx-ai` (Proxmox, private IPs, control plane)
- cloud: a Hetzner Cloud Talos worker with a public IP — the sole public ingress point

Node-to-node traffic uses Talos KubeSpan (WireGuard mesh). The sites are not a
flat L2 network and share no private addressing; ingress enters only via the
cloud node and reaches home workloads over the mesh.

## Validation

Before deployment, check the structure:

```bash
cd /Users/ngoldack/Projects/homelab
tofu fmt -check -recursive tofu
task tofu:home:validate
task tofu:cloud:validate
kustomize build kubernetes/infrastructure/home
kustomize build kubernetes/infrastructure/cloud
```

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
| `task tofu:home:init` / `task tofu:home:plan` | Init / plan Home infrastructure |
| `task tofu:cloud:init` / `task tofu:cloud:plan` | Init / plan Cloud infrastructure |
| `task kubeconfig:home:export` / `task kubeconfig:cloud:export` | Write cluster-specific kubeconfigs |

---

## Developer notes

This repo intentionally stays small and opinionated:

- keep only the bootstrap path required to bring up Talos + Cilium
- add storage and application layers only when they are needed by the cluster
- keep secrets encrypted with SOPS + age and never commit plaintext API keys
- prefer explicit human review for destructive actions such as deleting storage

If you need to add a workload later, add it in a single, purpose-built layer rather than reintroducing broader platform scaffolding.

## Cloud ingress

Home and Cloud are independent Kubernetes clusters. Cloud has one schedulable
HCloud Talos control plane and is the public edge. The UDM initiates a WireGuard
tunnel to that node for Cloud-to-Home and ClusterMesh traffic; no Tailscale,
Cloudflare, Home port forwarding, or DDNS is used.

Public traffic path:

```text
client → Hetzner DNS → A record → Hetzner cloud node
       → Cilium Gateway (envoy) :443, TLS = Let's Encrypt wildcard
  → HTTPRoute → ClusterIP Service → pod
```

- TLS is terminated on the Gateway with a single wildcard cert
  (`*.svc.<domain>` + `svc.<domain>`) issued by cert-manager via Let's Encrypt
  **DNS-01** through `cert-manager-webhook-hetzner`.
- external-dns creates per-app A records from Gateway HTTPRoutes, scoped to
  `svc.<domain>` so it cannot touch unrelated DNS.

## Talos etcd backups

Home and Cloud control-plane etcd snapshots are uploaded daily to separate,
generated Hetzner Object Storage buckets by
`.github/workflows/talos-etcd-backup.yml`. Backups run in GitHub Actions,
outside the clusters, so Talos administrative credentials are not available to
workloads in the clusters being backed up.

The Cloud job runs on GitHub-hosted `ubuntu-24.04`. The Home Talos API is on a
private network, so its job requires a self-hosted GitHub Actions runner with
the `self-hosted` and `homelab` labels and network access to the Home control
plane. The workflow removes its temporary Talos configuration and etcd snapshot
before either runner finishes.

Hetzner Cloud's OpenTofu provider does not manage Object Storage buckets. This
repository uses the maintained AWS provider against Hetzner's S3-compatible API
to create the bucket instead. Before the first Cloud apply, create an Object
Storage access key in the Hetzner Console for `fsn1`, then add its values to
`tofu/cloud/secret.sops.yaml`:

```yaml
hetzner_object_storage_access_key: <access-key>
hetzner_object_storage_secret_key: <secret-key>
```

Provision the bucket and export the Cloud Talos configuration:

```bash
export SOPS_AGE_KEY_FILE="$PWD/age.key"
task tofu:cloud:init
task tofu:cloud:apply
task talosconfig:home:export
task talosconfig:cloud:export
task talos-backup:home:bucket
task talos-backup:cloud:bucket
```

The Cloud root owns the HCloud network, firewall, primary IPs, ARM64 Talos
image, `cax11` control plane and worker, and Talos bootstrap directly through
the HCloud and Talos providers. A normal `task tofu:cloud:apply` creates the
image before the servers; no targeted apply or exclusion is required.

Talos starts with no CNI, so the Cloud root seeds Cilium through a bounded
OpenTofu Helm release after Talos bootstrap. Flux subsequently adopts the
same release in `kube-system`. After Flux reports its Cilium HelmRelease
Ready, relinquish OpenTofu state without uninstalling Cilium:

```bash
task tofu:cloud:handoff-cilium
```

Set these GitHub Actions repository secrets. The two bucket commands print the
corresponding bucket names. `HOME_TALOSCONFIG` and `CLOUD_TALOSCONFIG` are the
complete contents of the generated, ignored `talosconfig-home.yaml` and
`talosconfig-cloud.yaml` files.

| Secret | Value |
| --- | --- |
| `HETZNER_OBJECT_STORAGE_ACCESS_KEY` | Object Storage access key |
| `HETZNER_OBJECT_STORAGE_SECRET_KEY` | Object Storage secret key |
| `HETZNER_OBJECT_STORAGE_LOCATION` | `fsn1` (or the configured Object Storage location) |
| `HOME_TALOS_BACKUP_BUCKET` | `home_talos_backup_bucket` output |
| `CLOUD_TALOS_BACKUP_BUCKET` | `cloud_talos_backup_bucket` output |
| `HOME_TALOSCONFIG` | Home Talos configuration |
| `CLOUD_TALOSCONFIG` | Cloud Talos configuration |

Use **Run workflow** for the initial snapshot. Each bucket stores objects named
`etcd-<UTC timestamp>.snapshot`; both have `force_destroy = false`, so deleting
an OpenTofu resource never removes stored backups.

### Bootstrap order

1. Add a Hetzner DNS API token to the encrypted Cloud cert-manager and
  external-dns Secrets.
2. Bootstrap Flux independently at `kubernetes/clusters/home` and
  `kubernetes/clusters/cloud`; manually apply the corresponding encrypted
  `sops-age.sops.yaml` Secret before Flux reconciles encrypted manifests.
3. Let Flux reconcile the Cloud Cilium HelmRelease and adopt the seed release,
  then run `task tofu:cloud:handoff-cilium`.

### Verification

- WireGuard: the UDM peer reports a handshake and both node endpoint networks
  are reachable in each direction.
- Cloud: `kubectl --context cloud get nodes` shows its edge control plane.
- Certificate: `kubectl get certificate -n network` shows `wildcard-svc-tls` Ready.
- Gateway: `kubectl get gateway -n network public` shows Programmed with an address.
- DNS: `kubectl logs -n network deploy/external-dns` and `dig app.ns.svc.<domain>` → cloud IPv4.
- TLS end-to-end: `curl -v https://<app>.<ns>.svc.<domain>` presents the Let's Encrypt cert.
