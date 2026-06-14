# Homelab — Agent Instructions

## Architecture

This repo manages a Proxmox-hosted Talos OS Kubernetes cluster using OpenTofu for VM provisioning and Flux CD for GitOps delivery. Two main directories:

- `tofu/` — OpenTofu infrastructure code (Proxmox VMs, Talos machine configs, secrets). Uses `bpg/proxmox` and `siderolabs/talos` providers.
- `kubernetes/` — Kubernetes manifests reconciled by Flux, following the canonical Flux layout:
  - `kubernetes/clusters/production/` — Flux `Kustomization` entrypoints (`infra-controllers`, `infra-configs`, `apps`).
  - `kubernetes/infrastructure/controllers/` — operators, agents, CNI/CSI, and the central Helm `sources.yaml` (e.g. `cilium`, `cert-manager`).
  - `kubernetes/infrastructure/configs/` — cluster-wide config that depends on controllers (`cluster-issuers`, `kyverno-policies`).
  - `kubernetes/apps/<app>/` — one directory per application (its own `kustomization.yaml`). A single flat layout (no base/overlay split) since this is a single cluster. `kubernetes/apps/kustomization.yaml` is the toggle list: add/remove an `<app>` entry to enable/disable it. Shared building blocks live in `kubernetes/apps/_components/`.

## Conventions

### OpenTofu (`tofu/`)
- Sensitive values (API passwords, passphrases) are **never** declared as `variable` blocks. They are read exclusively from `tofu/secret.sops.yaml` via the `carlpett/sops` provider (`data "sops_file" "secrets"`).
- All Talos machine configs must disable the default CNI (`cni.name = none`) and kube-proxy (`proxy.disabled = true`) — Cilium replaces both.
- `tofu/terraform.tfstate` is committed to Git — it is encrypted at rest via OpenTofu native AES-GCM state encryption. Do not move it to a remote backend.
- Node pools are defined as a single `map(object)` variable in `variables.tf`. Do not add individual variables per node type.

### Kubernetes (`kubernetes/`)
- All HelmRelease resources reference a `HelmRepository` defined in `kubernetes/infrastructure/controllers/sources.yaml`. Do not inline `chart.spec.url`.
- Encrypted secrets must use the `.sops.yaml` filename suffix and be encrypted with the age key declared in `.sops.yaml`.
- New applications belong in `kubernetes/apps/<app>/` with their own `kustomization.yaml`. Add an `<app>` entry to `kubernetes/apps/kustomization.yaml` to enable it.
- All three Flux `Kustomization` objects (`infra-controllers`, `infra-configs`, `apps`) include a `decryption.provider: sops` block. Preserve this on edits.

### Secrets & Encryption
- Encryption uses **age** via **SOPS**. The public key anchor is `homelab_age_key` in `.sops.yaml`. Do not add a second key provider without updating both path rules.
- The state file passphrase is stored as `state_encryption_passphrase` inside `tofu/secret.sops.yaml`.
- Continuous Integration (`.github/workflows/ci.yaml`) lints only: it enforces secure encryption on all metadata (files matching raw `.sops.yaml` without proper `sops:` block structural indicators fail the run), plus YAML/workflow lint, OpenTofu validate, and kustomize/kubeconform. It never deploys or mutates infrastructure.

### Commit Messages
- Follow the **Conventional Commits** specification: `<type>(<scope>): <description>`
- Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `ci`, `revert`
- Scopes (optional): `tofu`, `kubernetes`, `cilium`, `cert-manager`, `flux`, `ci`, `authentik`
- Examples:
  - `feat(tofu): add worker-large node pool`
  - `fix(cilium): correct kube-proxy replacement flag`
  - `chore(ci): update trivy action version`
- Breaking changes: append `!` after scope — `feat(tofu)!: rename node pool variable`

## Integration Matrix Task For AI Agents

When installing or modifying any application in this cluster context, AI agents **MUST** respect and implement the following cross-system integration guidelines:

### 1. Storage Considerations
- **No node-local Storage for state**: Do not use `hostPath`. The `local-storage` class (local-path provisioner) is only for non-persistent, small scratch data.
- **Storage tiers**: Pick the StorageClass that matches the workload (TrueNAS has two pools — `fast`=NVMe, `tank`=HDD — served over a 10GbE link):
  - `tns-fast-nfs` — fast pool over NFS. **Default** tier; use for config, caches, app state, model weights, and general-purpose PVCs.
  - `tns-fast-nvmeof` — fast pool over NVMe-oF (block). Use for databases / high-performance, latency-sensitive workloads (CloudNativePG Postgres, Valkey).
  - `tns-tank-nfs` — tank (HDD) pool over NFS. Use only for huge files / media libraries (e.g. Emby).
  - `local-storage` — node-local, disposable. Small scratch only.
- **Stateless/Stateful Separation**: Whenever possible, avoid deploying databases (like Postgres, Redis, Valkey) as Helm sub-charts. Instead, deploy external cloud-native operator clusters (e.g., using `CloudNativePG` or `Valkey-Operator`) alongside the application namespaces, and inject host references. Put their volumes on `tns-fast-nvmeof`. Each app that needs a database gets its **own** instance + credentials (never shared).
- **Object storage (S3)**: the in-cluster S3 is **SeaweedFS** (operator-managed, volume data on `tns-fast-nvmeof`), reachable at `seaweedfs-s3.seaweedfs.svc.cluster.local:8333`. Provision per-app buckets/users/credentials by colocating namespaced CRDs in the app (`S3Identity` + `S3Credentials` → a generated `AWS_*` Secret + `Bucket` with `owner`), referencing the `seaweedfs` cluster (permitted by the `ResourceReferenceGrant` in the `seaweedfs` namespace). Do **not** reintroduce MinIO or Crossplane.

### 2. Security & Policy (Cilium)
- **Cilium Ingress Engine Linkages**: Ensure that custom application routing maps to Cilium's eBPF components. When possible, deploy Cilium-specific annotations to integrate and capture traffic drops.

### 3. Identity Provider (Authentik) Checks
- **Environment API Integration**: All application outposts or bouncers must reference unified environment variables mapping back to encrypted `secrets.sops.yaml`.

### 4. Code compliance & Validation
- **Dry-run Validations**: All configurations must build cleanly using Kustomize overlays: `kustomize build kubernetes/apps` and `kustomize build kubernetes/infrastructure/controllers` (and `.../configs`).
- **Linter Compliance**: Newly created templates must pass `yamllint -c .yamllint` checks cleanly before committing.
- **Secrets Encryption**: When declaring secrets, make sure you write them to `.sops.yaml` files, configure encryption path rules under `.sops.yaml`, and then encrypt them instantly in place using active workstation tooling (`sops --encrypt --in-place ...`).

## Build & Validate

```bash
# Validate OpenTofu config (no credentials needed)
cd tofu && tofu init -backend=false && tofu validate

# Validate Kubernetes manifests
kustomize build kubernetes/clusters/production
kustomize build kubernetes/infrastructure/controllers
kustomize build kubernetes/infrastructure/configs
kustomize build kubernetes/apps

# Lint YAML
yamllint -c .yamllint kubernetes/
```

## CI/CD

- `.github/workflows/ci.yaml` — runs on every push/PR to `main`; **lints only** (YAML, workflows, SOPS encryption status, OpenTofu validate, kustomize + kubeconform). It never deploys or mutates infrastructure.
- All cluster and VM operations (`tofu plan/apply/destroy`, `flux reconcile`, secret editing) are run manually from the workstation via the unified `Taskfile.yaml` runner, with the host connected to the private mesh. No cloud deployment runner is used.
