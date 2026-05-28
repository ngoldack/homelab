# Homelab — Agent Instructions

## Architecture

This repo manages a Proxmox-hosted Talos OS Kubernetes cluster using OpenTofu for VM provisioning and Flux CD for GitOps delivery. Two main directories:

- `tofu/` — OpenTofu infrastructure code (Proxmox VMs, Talos machine configs, secrets). Uses `bpg/proxmox` and `siderolabs/talos` providers.
- `kubernetes/` — Kubernetes manifests reconciled by Flux. Core infrastructure (`cilium`, `cert-manager`, `crowdsec`) lives under `kubernetes/infrastructure/`; user apps (`langfuse`, `authentik`) go under `kubernetes/apps/`.

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
- Continuous Integration (`.github/workflows/validate.yaml`) enforces secure encryption on all metadata: files matching raw `.sops.yaml` without proper `sops:` block structural indicators fail build runs.

### Commit Messages
- Follow the **Conventional Commits** specification: `<type>(<scope>): <description>`
- Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `ci`, `revert`
- Scopes (optional): `tofu`, `kubernetes`, `cilium`, `cert-manager`, `flux`, `ci`, `crowdsec`, `authentik`
- Examples:
  - `feat(tofu): add worker-large node pool`
  - `fix(cilium): correct kube-proxy replacement flag`
  - `chore(ci): update trivy action version`
- Breaking changes: append `!` after scope — `feat(tofu)!: rename node pool variable`

## Integration Matrix Task For AI Agents

When installing or modifying any application in this cluster context, AI agents **MUST** respect and implement the following cross-system integration guidelines:

### 1. Storage Considerations
- **No Local Storage**: Do not use `hostPath` or local volume claims unless explicitly requested.
- **Provider Storage Class**: All persistent configurations, database states, and active key storage must target the cluster's high-speed TrueNAS NFS volume manager utilizing `storageClassName: tns-csi-fast-nfs`.
- **Stateless/Stateful Separation**: Whenever possible, avoid deploying databases (like Postgres, Redis, Valkey) as Helm sub-charts. Instead, deploy external cloud-native operator clusters (e.g., using `CloudNativePG` or `Valkey-Operator`) alongside the application namespaces, and inject host references.

### 2. Security & Policy (Cilium & CrowdSec)
- **Log Scraping Ingest**: If the application has public ingress, is an authentication provider, or handles sensitive routing, its logs must be fed into the CrowdSec engine. 
- **Acquisition Additions**: Add matching target parameters inside `kubernetes/infrastructure/crowdsec/helmrelease.yaml` under `values.agent.acquisition` to target target namespaces and parse application pods.
- **Cilium Ingress Engine Linkages**: Ensure that custom application routing maps to Cilium's eBPF components. When possible, deploy Cilium-specific annotations to integrate and capture traffic drops.

### 3. Identity Provider (Authentik) Checks
- **External Security Interlocks**: Ensure applications that use Authentik utilize the custom **CrowdSec IP Reputation Check** python expression policy blueprint (`crowdsec-ip-rep`). This guarantees that login interfaces fail-closed if targeted by malicious actors.
- **Environment API Integration**: All application outposts or bouncers must reference unified environment variables mapping back to encrypted `secrets.sops.yaml` (e.g., `CROWDSEC_BOUNCER_KEY` maps to the corresponding registered Local API bouncer identity).

### 4. Code compliance & Validation
- **Dry-run Validations**: All configurations must build cleanly using Kustomize overlays: `kustomize build kubernetes/apps` and `kustomize build kubernetes/infrastructure`.
- **Linter Compliance**: Newly created templates must pass `yamllint -c .yamllint` checks cleanly before committing.
- **Secrets Encryption**: When declaring secrets, make sure you write them to `.sops.yaml` files, configure encryption path rules under `.sops.yaml`, and then encrypt them instantly in place using active workstation tooling (`sops --encrypt --in-place ...`).

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

- `.github/workflows/validate.yaml` — runs on every push/PR; validates kustomize, secrets encryption status, and YAML.
- Local custom cluster and VM operations are automated completely on the native workstation using a unified `Taskfile.yaml` runner configuration (no active cloud deployment runner workflow needed).
