# homelab

GitOps-driven homelab running a [Talos OS](https://www.talos.dev/) Kubernetes cluster on [Proxmox](https://www.proxmox.com/), provisioned with [OpenTofu](https://opentofu.org/) and managed by [Flux CD](https://fluxcd.io/).

## Stack

| Layer | Tool |
|---|---|
| Hypervisor | Proxmox VE |
| OS | Talos Linux |
| Provisioning | OpenTofu (`bpg/proxmox` + `siderolabs/talos`) |
| CNI | Cilium (eBPF, kube-proxy replacement) |
| TLS | cert-manager + Let's Encrypt |
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
    │   ├── production/     # Flux entrypoint for your Proxmox/Talos cluster
    │   └── local-dev/      # Flux entrypoint for local testing (Kind/Docker Desktop)
    ├── infrastructure/      # Cilium, cert-manager
    └── apps/                # Homelab applications (managed by Flux)
```

## Getting Started

### Prerequisites

- `age`, `sops`, `tofu`, `talosctl`, `flux`, `kubectl` installed locally
- Proxmox VE host reachable on the network
- A self-hosted GitHub Actions runner on the same LAN as Proxmox

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

| Workflow | Trigger | Purpose |
|---|---|---|
| `validate.yaml` | push / PR | Lint, validate, and security-scan all configs |
| `tofu-run.yaml` | manual | Run `plan`, `apply`, or `destroy` via GitHub Actions UI |

> **Note:** The `tofu-run.yaml` workflow requires a self-hosted runner with LAN access to Proxmox. Add `SOPS_AGE_KEY` and `STATE_ENCRYPTION_PASSPHRASE` as GitHub Actions secrets.
