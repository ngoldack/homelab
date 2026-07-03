# Homelab — Agent Instructions

## Architecture

This repo manages **Talos OS** Kubernetes clusters using **OpenTofu** for VM provisioning and **Flux CD** for GitOps delivery. It is built in phases, planned and tracked in OpenSpec (`openspec/changes/`) — read the relevant change before making architectural edits:

- **v0 (current): `home`** — a single Proxmox-hosted cluster (4 VMs on `pmx-main`): one control-plane, a general worker, a GPU worker (P100) and a CPU-inference worker. Cilium (kube-proxy replacement) is installed mesh-**ready** but not yet meshed. This is the only cluster that exists today.
- **v1: `cloud`** — a Hetzner edge cluster joined to `home` via Cilium Cluster Mesh; adds ingress, in-cluster S3, Authentik, CloudNativePG, Valkey, agentgateway.
- **v3: `offsite`** — a TrueNAS-hosted cluster for offsite/DR.

Anything beyond v0 (cloud, cluster mesh, S3, databases, Authentik, ingress) is **not deployed yet** — treat it as the design in OpenSpec, not as live config.

Two main directories:

- `tofu/` — OpenTofu infrastructure code (Proxmox VMs, Talos machine configs, secrets). Uses `bpg/proxmox` and `siderolabs/talos` providers. Node pools are a single `map(object)` in `variables.tf`.
- `kubernetes/` — manifests reconciled by Flux, following the canonical Flux layout:
  - `kubernetes/clusters/<name>/` — per-cluster Flux `Kustomization` entrypoints (`infra-controllers`, `infra-configs`, `apps`). v0 has `clusters/home/`.
  - `kubernetes/infrastructure/controllers/` — operators, CNI/CSI, and the central Helm `sources.yaml` (v0: `cilium`, the VictoriaMetrics/Grafana stack, the NVIDIA device plugin, `local-path`, the TrueNAS CSI driver).
  - `kubernetes/infrastructure/configs/` — cluster-wide config that depends on controllers.
  - `kubernetes/apps/<app>/` — one directory per application (its own `kustomization.yaml`); flat layout, no base/overlay split. `kubernetes/apps/kustomization.yaml` is the toggle list: add/remove an `<app>` entry to enable/disable it (v0 ships just `llama-cpp`). Shared building blocks live in `kubernetes/apps/_components/`.

## Conventions

### OpenTofu (`tofu/`)
- Tofu is **separated by location**: `tofu/locations/<home|cloud|offsite>/` is a self-contained root module per location (its own state + its own `secret.sops.yaml`). v0 ships `tofu/locations/home/` (Proxmox); `cloud` (Hetzner, v1) and `offsite` (TrueNAS, v3) are placeholders. Run against a location, e.g. `tofu -chdir=tofu/locations/home ...` or the `task tofu:*` runners.
- Sensitive values (API tokens, passphrases) are **never** declared as `variable` blocks. They are read exclusively from the location's `secret.sops.yaml` via the `carlpett/sops` provider (`data "sops_file" "secrets"`).
- **Per-host naming**: the home location runs on Proxmox hosts `pmx-main` (v0), and later `pmx-ai`, `pmx-core` (Dell R210ii), `pmx-test`, plus `pbs` (Proxmox Backup Server). Per-host secrets/references are keyed by host — the API token is `proxmox_<host>_api_token` (e.g. `proxmox_pmx-main_api_token`). Proxmox uses **API-token** auth (`api_token = "<user>@<realm>!<tokenid>=<uuid>"`), not username/password.
- All Talos machine configs must disable the default CNI (`cni.name = none`) and kube-proxy (`proxy.disabled = true`) — Cilium replaces both.
- Each location's state lives remotely in a **Hetzner Object Storage** bucket (`backend "s3"` in `providers.tf`, one bucket keyed by `locations/<name>/terraform.tfstate` per location) — never committed to Git. It is still encrypted at rest via OpenTofu native AES-GCM state encryption (backend-agnostic), layered on top of the bucket. Bucket + S3 credentials are a manual, one-time Hetzner Console step (no Terraform-manageable resource exists); credentials live in the location's `secret.sops.yaml` and are exported as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` by `task tofu:*`.
- Node pools are defined as a single `map(object)` variable in `variables.tf`. Do not add individual variables per node type.

### Kubernetes (`kubernetes/`)
- All HelmRelease resources reference a `HelmRepository` defined in `kubernetes/infrastructure/controllers/sources.yaml`. Do not inline `chart.spec.url`.
- Encrypted secrets must use the `.sops.yaml` filename suffix and be encrypted with the age key declared in `.sops.yaml`.
- New applications belong in `kubernetes/apps/<app>/` with their own `kustomization.yaml`. Add an `<app>` entry to `kubernetes/apps/kustomization.yaml` to enable it.
- All three Flux `Kustomization` objects (`infra-controllers`, `infra-configs`, `apps`) include a `decryption.provider: sops` block. Preserve this on edits.

### Secrets & Encryption
- Encryption uses **age** via **SOPS**. Recipients are defined once via the `&age_recipients` YAML anchor in `.sops.yaml` (the workstation key + a CI key) and shared across both `path_regex` rules (`tofu/locations/*/secret.sops.yaml` and `kubernetes/**/*.sops.yaml`). Keep recipients in sync across both rules; the workstation private key lives in `age.key` (gitignored, exported as `SOPS_AGE_KEY` for sops/tofu).
- The state file passphrase is stored as `state_encryption_passphrase` inside each location's `secret.sops.yaml` (e.g. `tofu/locations/home/secret.sops.yaml`).
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

When installing or modifying any application, AI agents **MUST** respect these cross-system integration guidelines. Items marked **(v1+)** describe subsystems that are designed in OpenSpec but **not deployed in v0** — do not wire an app to them until that phase lands.

### 1. Storage Considerations
- **No node-local Storage for state**: Do not use `hostPath`. The `local-storage` class (local-path provisioner) is only for non-persistent, small scratch data.
- **Storage tiers**: Pick the StorageClass that matches the workload (TrueNAS has two pools — `fast`=NVMe, `tank`=HDD — served over a 10GbE link):
  - `tns-fast-nfs` — fast pool over NFS. **Default** tier; use for config, caches, app state, model weights, and general-purpose PVCs.
  - `tns-fast-nvmeof` — fast pool over NVMe-oF (block). Use for databases / high-performance, latency-sensitive workloads (CloudNativePG Postgres, Valkey).
  - `tns-tank-nfs` — tank (HDD) pool over NFS. Use only for huge files / media libraries (e.g. Emby).
  - `local-storage` — node-local, disposable. Small scratch only.
- **Never delete TrueNAS volumes**: every `tns-*` StorageClass uses `reclaimPolicy: Retain`. Deleting a PVC/PV MUST NOT delete the underlying TrueNAS (`nas-main`) dataset — volumes are reclaimed **manually** on the NAS. When tearing down an app, keep its volume. Only `local-storage` is safe to discard.
- **Backups (v1+)**: split by target. **External** S3 (Hetzner) is used **only** for etcd / Talos snapshots, with **one folder per cluster**. **Internal** S3 (in-cluster SeaweedFS) backs up everything else that needs a backup (app data, databases). Do not send app/DB backups to the external bucket.
- **Stateless/Stateful Separation (v1+)**: Whenever possible, avoid deploying databases (like Postgres, Redis, Valkey) as Helm sub-charts. Instead, deploy external cloud-native operator clusters (e.g., using `CloudNativePG` or `Valkey-Operator`) alongside the application namespaces, and inject host references. Put their volumes on `tns-fast-nvmeof`. Each app that needs a database gets its **own** instance + credentials (never shared).
- **Object storage (S3) (v1+)**: the in-cluster S3 is **SeaweedFS** (operator-managed, volume data on `tns-fast-nvmeof`), reachable at `seaweedfs-s3.seaweedfs.svc.cluster.local:8333`. Provision per-app buckets/users/credentials by colocating namespaced CRDs in the app (`S3Identity` + `S3Credentials` → a generated `AWS_*` Secret + `Bucket` with `owner`), referencing the `seaweedfs` cluster (permitted by the `ResourceReferenceGrant` in the `seaweedfs` namespace). Do **not** reintroduce MinIO or Crossplane.

### 2. Security & Policy (Cilium)
- **Cilium Ingress Engine Linkages (v1+)**: Ensure that custom application routing maps to Cilium's eBPF components. When possible, deploy Cilium-specific annotations to integrate and capture traffic drops.

### 3. Identity Provider (Authentik) Checks
- **Environment API Integration (v1+)**: All application outposts or bouncers must reference unified environment variables mapping back to encrypted `.sops.yaml` secrets.

### 4. Code compliance & Validation
- **Dry-run Validations**: All configurations must build cleanly using Kustomize overlays: `kustomize build kubernetes/apps` and `kustomize build kubernetes/infrastructure/controllers` (and `.../configs`).
- **Linter Compliance**: Newly created templates must pass `yamllint -c .yamllint` checks cleanly before committing.
- **Secrets Encryption**: When declaring secrets, make sure you write them to `.sops.yaml` files, configure encryption path rules under `.sops.yaml`, and then encrypt them instantly in place using active workstation tooling (`sops --encrypt --in-place ...`).

## Build & Validate

```bash
# Validate OpenTofu config (no credentials needed)
tofu -chdir=tofu/locations/home init -backend=false && tofu -chdir=tofu/locations/home validate

# Build every Flux Kustomize entrypoint (v0)
kustomize build kubernetes/clusters/home
kustomize build kubernetes/infrastructure/controllers
kustomize build kubernetes/infrastructure/configs
kustomize build kubernetes/apps

# Schema-validate the built manifests
kustomize build kubernetes/apps | kubeconform -strict -ignore-missing-schemas

# Lint YAML + assert every *.sops.yaml is still encrypted
yamllint -c .yamllint kubernetes/
task sops:check
```

## CI/CD

- `.github/workflows/ci.yaml` — runs on every push/PR to `main`; **lints only** (YAML, workflows, SOPS encryption status, OpenTofu validate, kustomize + kubeconform). It never deploys or mutates infrastructure.
- All cluster and VM operations (`tofu plan/apply/destroy`, `flux reconcile`, secret editing) are run manually from the workstation via the unified `Taskfile.yaml` runner, with the host connected to the private mesh. No cloud deployment runner is used.
