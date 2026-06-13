# homelab

GitOps-driven homelab running a [Talos OS](https://www.talos.dev/) Kubernetes cluster on [Proxmox](https://www.proxmox.com/), provisioned with [OpenTofu](https://opentofu.org/) and managed by [Flux CD](https://fluxcd.io/).

## Stack

| Layer | Tool |
|---|---|
| Hypervisor | Proxmox VE |
| OS | Talos Linux |
| Provisioning | OpenTofu (`bpg/proxmox` + `siderolabs/talos`) |
| CNI | Cilium (eBPF, kube-proxy replacement) |
| Storage | TrueNAS CSI (`tns-csi`, NFS + NVMe-oF) + local-path |
| VPN Overlay | NetBird (Operator + Node Extension) |
| TLS | cert-manager + Let's Encrypt |
| Observability | VictoriaMetrics + Loki + OTel Collector + Grafana |
| Security / Policy | Kyverno (Best Practices Pod Security Standards) |
| GitOps | Flux CD |
| Secrets | SOPS + age |
| State Encryption | OpenTofu native AES-GCM |

## Cluster Layout

| Node | Role | vCPU | RAM | Disk |
|---|---|---|---|---|
| master-0/1/2 | controlplane | 2 | 4 GB | 32 GB |
| worker-default-0/1 | worker | 6 | 8 GB | 64 GB |
| worker-large-0 | worker | 12 | 48 GB | 128 GB |

## Repository Structure

```
.
├── tofu/                   # OpenTofu — VM provisioning & Talos bootstrap
└── kubernetes/
    ├── clusters/
    │   └── production/     # Flux entrypoint for your Proxmox/Talos cluster
    ├── infrastructure/      # Cilium, cert-manager
    └── apps/                # Homelab applications (managed by Flux)
```

## Getting Started

### Prerequisites

- `age`, `sops`, `tofu`, `talosctl`, `flux`, `kubectl`, `kustomize`,
  `kubeconform`, `yamllint`, `actionlint`, `trivy`, and [`task`](https://taskfile.dev)
  installed locally
- Proxmox VE host reachable on the network

### 1. Generate Age Key

```bash
age-keygen -o age.key
# Copy the printed public key into .sops.yaml
```

### 2. Configure Secrets

```bash
# Fill in proxmox_api_password and state_encryption_passphrase, then encrypt:
SOPS_AGE_KEY_FILE=age.key sops --encrypt --in-place tofu/secret.sops.yaml
```

### 3. Provision Infrastructure

```bash
export SOPS_AGE_KEY_FILE=age.key
export TOFU_ENCRYPTION_PASSPHRASE_statekey=$(sops -d tofu/secret.sops.yaml | yq .state_encryption_passphrase)

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
  --path=kubernetes/clusters/production \
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
  to the private mesh (`netbird up`).

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
`tofu/secret.sops.yaml` on demand; put `SOPS_AGE_KEY` in a gitignored `.env`.

---

## Developer and Agent Guidelines

For comprehensive cross-system checklists, custom GitOps conventions (Cilium integration and CloudNativePG storage policies), and rules of engagement (SOPS secrets and validation workflows) designed specifically for human developers and AI coding agents, please refer directly to [AGENTS.md](AGENTS.md).
