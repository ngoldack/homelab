# homelab

GitOps-driven homelab running a [Talos OS](https://www.talos.dev/) Kubernetes cluster on [Proxmox](https://www.proxmox.com/), provisioned with [OpenTofu](https://opentofu.org/) and managed by [Flux CD](https://fluxcd.io/).

> **Full architecture with mermaid diagrams: [docs/architecture.md](docs/architecture.md)** — every
> moving part (GitOps flow, operators, storage tiers, observability, the defense-in-depth security
> stack, the AI agent fleet, the MCP topology, and the deep-research pipeline).

## Stack

| Layer | Tool |
|---|---|
| Hypervisor | Proxmox VE |
| OS | Talos Linux (gVisor system extension on workers) |
| Provisioning | OpenTofu (`bpg/proxmox` + `siderolabs/talos`) |
| CNI | Cilium (eBPF, kube-proxy replacement, Gateway API, Hubble) |
| Storage | TrueNAS CSI (`tns-csi`, NFS + NVMe-oF) + local-path |
| Object Storage | SeaweedFS (in-cluster S3, embedded IAM, CRD-driven buckets/users) |
| Databases | CloudNativePG (per-app Postgres, Barman DR) + Valkey operator |
| VPN Overlay | NetBird (mesh) |
| Identity / SSO | Authentik (forward-auth + OIDC issuer) |
| TLS | cert-manager + Let's Encrypt |
| Observability | VictoriaMetrics + Loki + Tempo + OTel Collector + Grafana |
| LLM Serving | Ollama (GPU, 3x Tesla P100) behind LiteLLM aliases; Qwen3-ASR for STT |
| AI Agents | kagent + khook (22-agent fleet), Mem0 memory, Arize Phoenix traces |
| MCP Enforcement | agentgateway (per-agent JWT + CEL tool authz) |
| Agent Sandbox | gVisor RuntimeClass + Agent Substrate (guarded agents) |
| Deep Research | SearXNG + mcp-searxng + gpt-researcher |
| Home Automation | EMQX (MQTT) + Home Assistant + Zigbee2MQTT |
| Document Tooling | Gotenberg (PDF) + Docling (parse) + Pandoc (generate) |
| Workload Identity | SPIRE / SPIFFE (per-agent SVIDs) |
| Runtime Security | Tetragon (eBPF) |
| Policy / Supply chain | Kyverno + Trivy |
| Autoscaling | KEDA |
| GitOps | Flux CD |
| Secrets | SOPS + age |
| State Encryption | OpenTofu native AES-GCM |

## Cluster Layout

Node pools are a `map(object)` in `tofu/variables.tf` (counts configurable):

| Pool | Role | vCPU | RAM | Notes |
|---|---|---|---|---|
| `master` | controlplane | 4 | 4 GB | |
| `worker-default` | worker | 8 | 24 GB | gVisor |
| `worker-ai` | worker | 12 | 84 GB | gVisor, `ai=true:NoSchedule` taint |

## Repository Structure

```
.
├── tofu/                   # OpenTofu — VM provisioning & Talos bootstrap
└── kubernetes/
    ├── clusters/
    │   └── production/     # Flux entrypoints (infra-controllers → infra-configs → apps)
    ├── infrastructure/
    │   ├── controllers/    # operators (Cilium, CNPG, kagent, agentgateway, SPIRE, …)
    │   │                   # + central sources.yaml (HelmRepositories)
    │   └── configs/        # cluster-wide config (Kyverno + Tetragon policies,
    │   │                   # cluster-issuers, object-storage, runtime-classes)
    └── apps/               # one dir per app; kustomization.yaml is the toggle list
        └── _components/    # shared kustomize components (cnpg-barman-backup)
```

See **[docs/architecture.md](docs/architecture.md)** for the full diagrammed breakdown.

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
