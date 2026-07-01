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
| State Encryption | OpenTofu native AES-GCM | v0 |

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

### Prerequisites

- `age`, `sops`, `tofu`, `talosctl`, `flux`, `kubectl`, `kustomize`,
  `kubeconform`, `yamllint`, `actionlint`, `trivy`, and [`task`](https://taskfile.dev)
  installed locally — or simply run `devbox shell` (see `devbox.json`)
- Proxmox VE host reachable on the network

### 1. Generate Age Key

```bash
age-keygen -o age.key
# Copy the printed public key into .sops.yaml
```

### 2. Configure Secrets

```bash
# Fill in proxmox_api_password and state_encryption_passphrase, then encrypt:
SOPS_AGE_KEY_FILE=age.key sops --encrypt --in-place tofu/locations/home/secret.sops.yaml
```

### 3. Provision Infrastructure

```bash
export SOPS_AGE_KEY_FILE=age.key
export TOFU_ENCRYPTION_PASSPHRASE_statekey=$(sops -d tofu/locations/home/secret.sops.yaml | yq .state_encryption_passphrase)

cd tofu
tofu init && tofu apply
```

### 4. Bootstrap Flux

```bash
# Register the age key with the cluster so Flux can decrypt secrets
kubectl create secret generic sops-age \
  --namespace=flux-system \
  --from-file=age.agekey=age.key

flux bootstrap github \
  --owner=<your-github-username> \
  --repository=homelab \
  --branch=main \
  --path=kubernetes/clusters/home \
  --personal
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
