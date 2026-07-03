# homelab

GitOps-driven homelab running a [Talos OS](https://www.talos.dev/) Kubernetes cluster on [Proxmox](https://www.proxmox.com/), provisioned with [OpenTofu](https://opentofu.org/) and managed by [Flux CD](https://fluxcd.io/).

> **Full architecture with diagrams: [docs/architecture.md](docs/architecture.md).**
> A lean, **phased** self-hosted AI platform (v0–v3): three Talos/Cilium clusters
> joined by Cilium Cluster Mesh, serving LLMs via **llama.cpp**. The plan lives in
> [`openspec/`](openspec/) — each phase is an OpenSpec change.

## Stack

Built **per phase** (see [`openspec/`](openspec/)). v0 is deliberately minimal;
v1–v3 add the cloud cluster + mesh, platform, agents, and offsite.

| Layer | Tool | Phase |
|---|---|---|
| Hypervisor | Proxmox VE (home); Hetzner (cloud); TrueNAS Scale (offsite) | — |
| OS | Talos Linux | v0 |
| Provisioning | OpenTofu (`bpg/proxmox` + `siderolabs/talos`; `hcloud` in v1) | v0 |
| CNI / mesh | Cilium (eBPF, kube-proxy replacement, Gateway API, Hubble, WireGuard, **Cluster Mesh**) | v0 / mesh v1 |
| Storage | TrueNAS CSI — 4 tiers (`tns-fast-nvmeof`, `tns-fast-nfs` default, `tns-tank-nfs`, `local-path`) | v0 |
| Observability | VictoriaMetrics + VictoriaLogs + VictoriaTraces + OTel Collector + Grafana | v0 |
| LLM Serving | **llama.cpp** — `qwen3.5:9b` on the P100 (GPU) + `qwen3-coder-next` on a dedicated CPU node | v0 |
| Model gateway | agentgateway (stable aliases in front of llama.cpp) | v1 |
| Identity / SSO | Authentik (OIDC + forward-auth) | v1 |
| Databases | CloudNativePG (per-app Postgres, S3 backups) + Valkey operator | v1 |
| Backups | → Hetzner S3 (offsite) | v1 |
| Agents | n8n, kagent, agentgateway, mem0 — all via agentgateway → llama.cpp | v2 |
| GitOps | Flux CD (one per cluster) | v0 |
| Secrets | SOPS + age (`*.sops.yaml`) | v0 |
| State | OpenTofu native encryption; remote in Hetzner Object Storage (S3-compatible) | v0 |

## Cluster Layout

Three independent clusters joined by Cilium Cluster Mesh (`openspec/` for the plan):

| Cluster | Phase | Nodes |
|---|---|---|
| **home** (id 1) | v0 | 4 Talos VMs on `pmx-main`: `cp` (control plane), `wk` (general), `wk-gpu` (P100 → GPU model), `wk-cpu` (64GB → CPU model) |
| **cloud** (id 2) | v1 | 1 Hetzner VPS (cp+workloads) — public ingress edge |
| **offsite** (id 3) | v3 | 1 TrueNAS Scale VM (cp+workloads) — DR |

Node pools are a `map(object)` in `tofu/locations/home/variables.tf`.

## Repository Structure

```
.
├── openspec/               # the phased plan (v0–v3): proposals, specs, tasks
├── tofu/
│   └── locations/          # OpenTofu per location: home (Proxmox); cloud=v1; offsite=v3
└── kubernetes/
    ├── clusters/
    │   └── home/        # Flux entrypoints (infra-controllers → infra-configs → apps)
    ├── infrastructure/
    │   ├── controllers/    # Cilium, gateway-api, NVIDIA device plugin, storage CSI,
    │   │                   # observability + central sources.yaml (HelmRepositories)
    │   └── configs/        # cluster-wide config
    └── apps/               # one dir per app; kustomization.yaml is the toggle list
        └── llama-cpp/      # v0: the GPU + CPU model servers
```

See **[docs/architecture.md](docs/architecture.md)** for the full diagrammed breakdown.

## Getting Started

This walks through a **first deploy from scratch** — no cluster exists yet.

### Prerequisites

- `age`, `sops`, `tofu`, `talosctl`, `flux`, `kubectl`, `kustomize`,
  `kubeconform`, `yamllint`, `actionlint`, `trivy`, and [`task`](https://taskfile.dev)
  installed locally — or simply run `devbox shell` (see `devbox.json`)
- Proxmox VE host reachable on the network
- **Networking**: a dedicated VLAN trunked to the Proxmox host (default VLAN
  3000 / `10.30.0.0/24`, see `tofu/locations/home/variables.tf`) with a
  maintenance DHCP scope on that VLAN (Talos boots the install ISO in
  maintenance mode before its static IP is applied), and your workstation able
  to route to that subnet (tofu/talosctl/kubectl all talk to the nodes there)
- A **Hetzner Cloud** account, for the OpenTofu remote-state bucket (Object
  Storage) — no VMs are provisioned there in v0, only state storage

### 1. Generate the age key

```bash
age-keygen -o age.key
# Copy the printed public key into .sops.yaml (both age recipients must match)
echo "SOPS_AGE_KEY=$(tail -1 age.key)" > .env
```

### 2. Create the Hetzner Object Storage bucket + credentials

OpenTofu state for this location lives remotely in a Hetzner bucket (see
`backend "s3"` in `tofu/locations/home/providers.tf`). Hetzner has no
Terraform-manageable resource for Object Storage — this is a manual, one-time
step in the [Hetzner Cloud Console](https://console.hetzner.com/):

1. **Object Storage → Create Bucket** — name `ngoldack-tofu-state`, location
   `fsn1` (Falkenstein). Using a different name/location? Update `bucket` /
   `endpoints.s3` in `providers.tf` to match, or override at init time with
   `-backend-config="bucket=..."` (backend blocks can't use variables).
2. **Enable Versioning** on the bucket — a safety net for accidental state
   corruption.
3. **Object Storage → Credentials → Generate** — note the access key and
   secret key (the secret is shown only once).

### 3. Configure secrets

```bash
sops tofu/locations/home/secret.sops.yaml
```

Fill in:

- `hetzner_s3_access_key` / `hetzner_s3_secret_key` — from step 2
- `proxmox_pmx-main_api_token` — a Proxmox API token (`<user>@<realm>!<tokenid>=<uuid>`)
- `state_encryption_passphrase` — pre-filled with a strong random value; leave
  it or roll your own

`sops` re-encrypts automatically on save. Also set the TrueNAS CSI credential:

```bash
sops kubernetes/infrastructure/controllers/storage/tns-csi/secret.sops.yaml
```

### 4. Point the TrueNAS CSI driver at your NAS

This one isn't a secret — edit it directly:

```bash
# kubernetes/infrastructure/controllers/storage/tns-csi/helmrelease.yaml
# replace: url: wss://YOUR-TRUENAS-IP:443/api/current
```

### 5. Review the VERIFY-BEFORE-DEPLOY notes

A handful of values in this repo are deliberately unverified placeholders
(scan for `VERIFY-BEFORE-DEPLOY`), most importantly:

- `tofu/locations/home/variables.tf` — the real P100 host PCI id
  (`lspci -nn | grep -i nvidia`; needs IOMMU/vfio-pci on the Proxmox host) and
  the NVIDIA Talos extension tags (must match your Talos version + the
  Pascal-supporting host driver)
- `kubernetes/apps/llama-cpp/model-{chat,coder}.yaml` — the real GGUF source
  URLs/quants (the checked-in ones are placeholders until the target models exist)
- `tofu/locations/home/providers.tf` — the Hetzner S3 backend's `use_path_style`
  / `use_lockfile` settings haven't been exercised against a real bucket yet

### 6. Provision infrastructure

```bash
task tofu:plan   # review the plan
task tofu:apply  # provision the 4 VMs + bootstrap Talos/Kubernetes
```

Without `task`, the equivalent is:

```bash
export SOPS_AGE_KEY_FILE=age.key
export TF_ENCRYPTION="key_provider \"pbkdf2\" \"statekey\" { passphrase = \"$(sops -d tofu/locations/home/secret.sops.yaml | yq .state_encryption_passphrase)\" }"
export AWS_ACCESS_KEY_ID=$(sops -d tofu/locations/home/secret.sops.yaml | yq .hetzner_s3_access_key)
export AWS_SECRET_ACCESS_KEY=$(sops -d tofu/locations/home/secret.sops.yaml | yq .hetzner_s3_secret_key)

tofu -chdir=tofu/locations/home init
tofu -chdir=tofu/locations/home apply
```

The nodes come up on the dedicated subnet with static IPs: `cp` `10.30.0.10`
(the API endpoint), `wk` `.11`, `wk-gpu` `.12`, `wk-cpu` `.13`.

### 7. Bootstrap Flux

```bash
task tofu:kubeconfig  # writes kubeconfig.yaml from the new state

# Register the age key with the cluster so Flux can decrypt secrets
KUBECONFIG=kubeconfig.yaml kubectl create secret generic sops-age \
  --namespace=flux-system \
  --from-file=age.agekey=age.key

KUBECONFIG=kubeconfig.yaml flux bootstrap github \
  --owner=<your-github-username> \
  --repository=homelab \
  --branch=main \
  --path=kubernetes/clusters/home \
  --personal
```

### 8. Verify

```bash
KUBECONFIG=kubeconfig.yaml kubectl get nodes                    # 4 nodes, Ready
KUBECONFIG=kubeconfig.yaml kubectl get pods -A                   # everything Running
KUBECONFIG=kubeconfig.yaml kubectl describe node <wk-gpu node>   # nvidia.com/gpu: 1
KUBECONFIG=kubeconfig.yaml kubectl get inferenceservice -n llama-cpp
```


## CI / CD

This repo keeps automation deliberately simple:

- **GitHub Actions only lints.** [`ci.yaml`](.github/workflows/ci.yaml) runs on
  every push and PR to `main` and performs static validation only — YAML lint,
  workflow lint, a SOPS-encryption check (ciphertext inspection, no key needed),
  OpenTofu `fmt`/`validate`, and `kustomize build` + `kubeconform` for every
  overlay. It needs **no secrets, kubeconfig, or VPN, and never deploys or
  mutates anything**.
- **Everything else runs on your workstation** via the [`Taskfile`](Taskfile.yaml)
  with host-installed tools (no containers). Provisioning and reconciliation
  (`tofu apply`, `flux reconcile`) are run manually while your host is connected
  to the LAN.

### Tasks

Run `task --list` for the full set. Common ones:

| Task | What it does |
|---|---|
| `task check` | All offline checks (lint + validate), same as CI |
| `task k8s:validate` | `kustomize build` + `kubeconform` for every overlay |
| `task tofu:validate` | OpenTofu `fmt` + `init -backend=false` + `validate` |
| `task sops:check` | Assert every `*.sops.yaml` is encrypted |
| `task tofu:plan` / `tofu:apply` | Plan / apply live infra (mesh + `SOPS_AGE_KEY` required) |
| `task flux:reconcile` | Force Flux to reconcile in dependency order |
| `task secrets:edit -- <file>` | Open a SOPS file in your editor |

Live tasks read the OpenTofu state-encryption passphrase from
`tofu/locations/home/secret.sops.yaml` on demand; put `SOPS_AGE_KEY` in a gitignored `.env`.

---

## Developer and Agent Guidelines

For comprehensive cross-system checklists, custom GitOps conventions (Cilium integration and CloudNativePG storage policies), and rules of engagement (SOPS secrets and validation workflows) designed specifically for human developers and AI coding agents, please refer directly to [AGENTS.md](AGENTS.md).
