# Homelab — Agent Instructions

## Architecture

This repo manages a Proxmox-hosted Talos OS Kubernetes cluster using OpenTofu for VM provisioning and Flux CD for GitOps delivery. Two main directories:

- `tofu/` — OpenTofu infrastructure code (Proxmox VMs, Talos machine configs, secrets). Uses `bpg/proxmox` and `siderolabs/talos` providers.
- `kubernetes/` — Kubernetes manifests reconciled by Flux. Core infrastructure (`cilium`, `cert-manager`) lives under `kubernetes/infrastructure/`; user apps go under `kubernetes/apps/`.

## Conventions

### OpenTofu (`tofu/`)
- Sensitive values (API passwords, passphrases) are **never** declared as `variable` blocks. They are read exclusively from `tofu/secret.sops.yaml` via the `carlpett/sops` provider (`data "sops_file" "secrets"`).
- All Talos machine configs must disable the default CNI (`cni.name = none`) and kube-proxy (`proxy.disabled = true`) — Cilium replaces both.
- `tofu/terraform.tfstate` is committed to Git — it is encrypted at rest via OpenTofu native AES-GCM state encryption. Do not move it to a remote backend.
- Node pools are defined as a single `map(object)` variable in `variables.tf`. Do not add individual variables per node type.

### Kubernetes (`kubernetes/`)
- All HelmRelease resources reference a `HelmRepository` defined in `kubernetes/infrastructure/configs/sources.yaml`. Do not inline `chart.spec.url`.
- Encrypted secrets must use the `.sops.yaml` filename suffix and be encrypted with the age key declared in `.sops.yaml`.
- New applications belong in `kubernetes/apps/` as a subdirectory with their own `kustomization.yaml`. Add a `resources:` entry in `kubernetes/apps/kustomization.yaml`.
- Both Flux `Kustomization` objects (`infrastructure` and `apps`) include a `decryption.provider: sops` block. Preserve this on edits.

### Secrets & Encryption
- Encryption uses **age** via **SOPS**. The public key anchor is `homelab_age_key` in `.sops.yaml`. Do not add a second key provider without updating both path rules.
- The state file passphrase is stored as `state_encryption_passphrase` inside `tofu/secret.sops.yaml`.

### Commit Messages
- Follow the **Conventional Commits** specification: `<type>(<scope>): <description>`
- Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `ci`, `revert`
- Scopes (optional): `tofu`, `kubernetes`, `cilium`, `cert-manager`, `flux`, `ci`
- Examples:
  - `feat(tofu): add worker-large node pool`
  - `fix(cilium): correct kube-proxy replacement flag`
  - `chore(ci): update trivy action version`
- Breaking changes: append `!` after scope — `feat(tofu)!: rename node pool variable`

## Build & Validate

```bash
# Validate OpenTofu config (no credentials needed)
cd tofu && tofu init -backend=false && tofu validate

# Validate Kubernetes manifests
kustomize build kubernetes/clusters/production
kustomize build kubernetes/infrastructure
kustomize build kubernetes/apps

# Lint YAML
yamllint -c .yamllint kubernetes/
```

## CI/CD

- `.github/workflows/validate.yaml` — runs on every push/PR; validates tofu, kustomize, and YAML.
- `.github/workflows/tofu-run.yaml` — manual `workflow_dispatch`; requires a self-hosted runner on the same LAN as Proxmox, and two GitHub Actions secrets: `SOPS_AGE_KEY` and `STATE_ENCRYPTION_PASSPHRASE`.
