# Homelab Architecture

End-to-end documentation of every moving part: from Proxmox VMs up through the
GitOps delivery chain, the platform operators, the security stack, and the AI
agent / deep-research system.

> Conventions: this repo is GitOps-first (Flux), secrets are SOPS/age encrypted,
> CI lints only, and all live operations run from the workstation via `Taskfile`.
> See [AGENTS.md](../AGENTS.md) for the engineering rules and
> [apps/kagent/README.md](../kubernetes/apps/kagent/README.md) for the agent
> subsystem deep-dive.

---

## 1. Physical → logical stack

```mermaid
flowchart TD
  subgraph HW["Hardware"]
    PVE["Proxmox VE host"]
    TNS["TrueNAS<br/>NVMe pool + HDD pool<br/>10GbE"]
    HZ["Hetzner Object Storage<br/>(offsite DR)"]
  end

  subgraph VM["Talos VMs — provisioned by OpenTofu"]
    CP["control plane<br/>4 vCPU / 4 GB"]
    WD["worker-default<br/>8 vCPU / 24 GB<br/>gVisor"]
    WAI["worker-ai<br/>12 vCPU / 84 GB<br/>gVisor + ai taint"]
  end

  PVE --> VM
  VM --> K8S["Kubernetes (Talos OS)"]
  K8S --> FLUX["Flux CD — GitOps reconciler"]
  FLUX --> WL["Controllers + Configs + Apps"]

  TNS -. "CSI: NFS + NVMe-oF" .-> K8S
  HZ -. "Talos etcd / Velero / CNPG backups" .-> K8S
```

OpenTofu (`tofu/`) provisions the VMs via `bpg/proxmox`, renders Talos machine
configs via `siderolabs/talos`, and stores its state **encrypted in Git**
(native AES-GCM, passphrase in `tofu/secret.sops.yaml`). Node pools are a single
`map(object)` in `tofu/variables.tf` (counts are configurable; the gVisor system
extension + `user.max_user_namespaces` sysctl are baked into the worker pools).

---

## 2. GitOps delivery (Flux dependency order)

```mermaid
flowchart LR
  GIT["Git repo<br/>kubernetes/clusters/production"] --> FS["flux-system"]
  FS --> IC["infra-controllers<br/>operators, CNI, CSI, CRDs"]
  IC --> ICFG["infra-configs<br/>policies, issuers, S3, runtimeclasses"]
  ICFG --> APPS["apps<br/>(toggle list)"]

  SOPS["SOPS age key<br/>(sops-age secret)"]
  SOPS -. decrypt .-> IC
  SOPS -. decrypt .-> ICFG
  SOPS -. decrypt .-> APPS
  CS["cluster-secrets<br/>DOMAIN, DR_S3_*"]
  CS -. "postBuild substituteFrom" .-> ICFG
  CS -. "postBuild substituteFrom" .-> APPS
```

Three Flux `Kustomization`s chain by `dependsOn`: **infra-controllers →
infra-configs → apps**. All three decrypt SOPS; `infra-configs` and `apps` also
substitute `${DOMAIN}` / `${DR_S3_*}` from the `cluster-secrets` secret.

---

## 3. Repository layout

```text
homelab/
├── tofu/                      # OpenTofu: Proxmox VMs, Talos config, encrypted state
│   ├── variables.tf           # node_pools map(object); Hetzner DR; extensions
│   ├── talos.tf               # machine config patches (CNI=none, gVisor sysctl…)
│   └── secret.sops.yaml       # proxmox pw + state passphrase (SOPS)
├── kubernetes/
│   ├── clusters/production/   # Flux entrypoints (infra-controllers/configs/apps)
│   ├── infrastructure/
│   │   ├── controllers/       # operators + central sources.yaml (HelmRepositories)
│   │   └── configs/           # cluster-wide config that depends on controllers
│   └── apps/                  # one dir per app; kustomization.yaml is the toggle
│       └── _components/       # shared kustomize components (cnpg-barman-backup)
├── docs/                      # this file, roadmap, disaster-recovery
└── .github/workflows/ci.yaml  # lint-only CI
```

---

## 4. Infrastructure controllers (operators)

```mermaid
flowchart TB
  subgraph net["Networking"]
    gapi["gateway-api (CRDs)"]
    cil["cilium<br/>eBPF CNI, kube-proxy replacement,<br/>Gateway API, Hubble"]
    nb["netbird<br/>(mesh overlay)"]
  end
  subgraph sto["Storage"]
    tns["tns-csi<br/>TrueNAS NFS + NVMe-oF"]
    lp["local-path"]
    sw["seaweedfs-operator<br/>(in-cluster S3)"]
  end
  subgraph dat["Data operators"]
    cnpg["cnpg-operator<br/>+ barman-plugin (DR)"]
    vk["valkey-operator"]
  end
  subgraph secg["Security + policy"]
    cm["cert-manager"]
    kyv["security: kyverno + trivy-operator"]
    tet["tetragon (runtime eBPF)"]
    spi["spire (SPIFFE identity)"]
  end
  subgraph obs["Observability"]
    o["observability:<br/>VictoriaMetrics, Loki, Tempo,<br/>OTel Collector, Grafana"]
  end
  subgraph aip["AI platform"]
    ka["kagent + khook"]
    agw["agentgateway (MCP proxy)"]
    sub["agent-substrate<br/>(gVisor sandbox runtime)"]
  end
  subgraph etc["Other"]
    keda["keda (autoscaling)"]
    emqx["emqx-operator (MQTT)"]
    bk["backup (Velero + Talos etcd)"]
    hl["headlamp (dashboard)"]
  end
```

HelmRepositories are centralised in
`infrastructure/controllers/sources.yaml`; every `HelmRelease` references one by
name (no inline chart URLs).

---

## 5. Storage tiers

```mermaid
flowchart LR
  subgraph TrueNAS["TrueNAS (10GbE)"]
    nvme["NVMe pool"]
    hdd["HDD pool"]
  end
  nvme --> tfnvme["tns-fast-nvmeof<br/>(block, NVMe-oF)"]
  nvme --> tfnfs["tns-fast-nfs<br/>(NFS, DEFAULT)"]
  hdd  --> ttnfs["tns-tank-nfs<br/>(NFS, bulk)"]

  tfnvme --> dbs["Databases (CNPG), Valkey,<br/>SeaweedFS volumes, EMQX"]
  tfnfs  --> gen["Config, caches, model weights,<br/>HA config, general PVCs"]
  ttnfs  --> media["Media / huge files"]

  local["local-storage<br/>(node-local, disposable)"] --> scratch["small scratch only"]
  seaweed["SeaweedFS S3<br/>seaweedfs-s3.svc:8333"] --> s3["per-app buckets via CRDs"]
  hetzner["Hetzner DR bucket"] --> dr["offsite: etcd, Velero, CNPG WAL"]
```

Per-app Postgres (CloudNativePG) and Valkey instances are **never shared** — each
app gets its own + credentials. S3 buckets/users are provisioned per-app via
SeaweedFS CRDs (`S3Identity` + `S3Credentials` + `Bucket`).

---

## 6. Observability & audit pipeline

```mermaid
flowchart LR
  pods["Workloads (stdout + OTLP)"] --> otel["OTel Collector (DaemonSet)"]
  tet["Tetragon (eBPF events, JSON stdout)"] --> otel
  otel --> loki["Loki (logs)"]
  otel --> vm["VictoriaMetrics (metrics)"]
  otel --> tempo["Tempo (traces)"]
  kagent["kagent agents (OTLP)"] --> phoenix["Arize Phoenix<br/>(LLM traces)"]
  loki --> graf["Grafana"]
  vm --> graf
  tempo --> graf
  graf -. "alerts (webhook)" .-> n8n["n8n"]
```

Tetragon runtime-security events flow stdout → OTel filelog → Loki (alertable in
Grafana → n8n). LLM agent traces go to Phoenix via OTLP gRPC `:4317`.

---

## 7. Security — defense in depth

```mermaid
flowchart TB
  L1["L1 Admission — Kyverno<br/>3 baseline (Audit) + agent-restricted (Enforce, label-scoped)"]
  L2["L2 Pod Security Admission<br/>Talos baseline cluster-wide; restricted on kagent ns"]
  L3["L3 RBAC<br/>per-agent SAs (deny-all); tool-server scoped off cluster-admin"]
  L4["L4 Network — Cilium<br/>default-deny ingress+egress for agent pods, allowlists"]
  L5["L5 Workload identity — SPIRE<br/>per-agent SPIFFE SVIDs (k8s_psat)"]
  L6["L6 Runtime — Tetragon<br/>TracingPolicies: shell exec, SA-token read, public egress"]
  L7["L7 Sandbox — gVisor<br/>RuntimeClass (docling, pandoc) + Agent Substrate (guarded agents)"]
  L8["L8 MCP enforcement — agentgateway<br/>per-agent JWT (Authentik) + CEL tool authz"]
  L9["L9 LLM guardrails<br/>requireApproval (HITL), Spotlighting, guarded SandboxAgents"]
  L10["L10 Supply chain — Trivy + image scanning"]

  L1 --> L2 --> L3 --> L4 --> L5 --> L6 --> L7 --> L8 --> L9 --> L10
```

Secrets are SOPS/age throughout. The full hardening table + accepted gaps live in
[apps/kagent/README.md](../kubernetes/apps/kagent/README.md).

---

## 8. Identity & external exposure

```mermaid
flowchart LR
  user["Operator"] --> nbm["NetBird mesh<br/>(netbird.DOMAIN)"]
  nbm --> ak["Authentik<br/>forward-auth (embedded outpost)"]
  ak --> apps["n8n, SearXNG, Phoenix,<br/>Home Assistant, Grafana…"]

  cil["Cilium Gateway API"] --> ak
  ak -. "OIDC provider" .-> gw["agentgateway (JWT issuer)"]
  cm["cert-manager + Let's Encrypt"] -. TLS .-> cil
```

Apps are reached over the **NetBird mesh** (not the public internet) behind
**Authentik** forward-auth. Authentik is also the **OIDC issuer** for
agentgateway's MCP JWTs.

---

## 9. AI agent system (kagent)

```mermaid
flowchart TD
  MO["main-orchestrator<br/>(Tier 0 — all inbound events)"]
  SEC["security-orchestrator<br/>(Tier 0 — advisory + audit)"]

  MO --> RC["research-chief (T1)"]
  MO --> PC["platform-chief (T1)"]
  MO --> HC["homelab-chief (T1)"]

  RC --> WS["web-search-agent"]
  RC --> DI["document-ingest-agent"]
  RC --> SY["synthesis-agent"]
  RC --> CI["citation-agent"]
  RC --> DA["document-author-agent"]
  DA --> PDF["pdf-converter-specialist"]
  DA --> XL["spreadsheet-specialist"]

  PC --> SRE["k8s-sre-agent"]
  PC --> GF["gitops-flux-agent"]
  PC --> OBS["observability-agent"]
  PC --> DB["database-specialist"]
  PC --> IDN["identity-network-agent"]
  PC --> CU["cluster-update-specialist"]

  HC --> HA["homeassistant-expert-agent"]
  HC --> ZB["zigbee-mqtt-agent"]

  FIN["finance-agent<br/>(GUARDED SandboxAgent)"]
  MAIL["mail-agent<br/>(GUARDED SandboxAgent)"]

  SEC -. "cross-cutting policy" .-> MO
  MO -. "guarded path only" .-> FIN
  MO -. "guarded path only" .-> MAIL
```

22 agents (20 `Agent` + 2 `SandboxAgent`). `main-orchestrator` delegates only to
the three chiefs; guarded agents are kept out of every toolset (unreachable).
Triggers (`khook`) fire `main-orchestrator` on cluster events
(CrashLoopBackOff/OOMKill/Flux failures).

---

## 10. MCP tool topology

```mermaid
flowchart LR
  agents["kagent agents<br/>(restricted ns)"]

  agents -->|"Bearer JWT"| agw["agentgateway<br/>JWT (Strict) + CEL authz"]
  agw --> mem0["mem0 MCP (memory)"]
  agw --> n8n["n8n MCP (notify/workflows)"]

  agents -->|"direct (pre-token)"| sx["searxng-mcp<br/>search + fetch (research ns)"]
  agents --> tools["kagent-tool-server<br/>(built-in k8s/helm, scoped RBAC)"]
  sx --> searxng["SearXNG"]

  subgraph scaffold["Scaffolded (unwired)"]
    direction LR
    s1["flux-git, ha-api, z2m-api,<br/>mqtt-admin, mail-*, scalable-portfolio,<br/>policy-evaluator, audit-log"]
  end
  agents -. TODO .-> scaffold
```

Only **mem0**, **n8n**, **searxng-mcp**, and the built-in **kagent-tool-server**
are wired. mem0/n8n route through agentgateway (JWT — inert until the Authentik
token is minted); `searxng-mcp` is direct so the research pipeline works now. The
rest are flagged `RemoteMCPServer` scaffolds, referenced by no agent.

---

## 11. Deep-research pipeline (Perplexity-style)

```mermaid
sequenceDiagram
  autonumber
  participant U as User / n8n / khook
  participant MO as main-orchestrator
  participant RC as research-chief
  participant WS as web-search-agent
  participant SX as searxng-mcp
  participant DI as document-ingest
  participant SY as synthesis-agent
  participant CT as citation-agent
  participant DA as document-author
  U->>MO: research question
  MO->>RC: delegate
  RC->>RC: expand into 3-7 sub-questions
  par parallel sub-searches
    RC->>WS: sub-question
    WS->>SX: searxng_web_search + web_url_read
    SX-->>WS: results + page content
    WS-->>RC: structured sources
  end
  RC->>DI: ingest URLs/docs (web_url_read; docling later)
  RC->>SY: synthesize (context/evidence/analysis/conclusion)
  RC->>CT: verify claims + add citations
  RC->>DA: author deliverable (Pandoc/Gotenberg)
  DA-->>U: cited report (DOCX/PPTX/PDF)
```

A turnkey alternative — **gpt-researcher** (`apps/research`, Apache-2.0) — runs
the same plan→search→synthesize→report loop autonomously over our SearXNG + Ollama,
exposed as a REST engine (wiring it into `research-chief` needs the `gptr-mcp`
wrapper — TODO).

---

## 12. agentgateway MCP enforcement (status)

```mermaid
sequenceDiagram
  participant A as agent pod
  participant G as agentgateway
  participant AK as Authentik (JWKS)
  participant M as mem0 / n8n MCP
  A->>G: POST /mcp/mem0 + Authorization Bearer <JWT>
  G->>AK: fetch JWKS, validate signature/issuer/aud
  Note over G: mode Strict — reject if invalid
  G->>G: CEL authz — has(jwt.sub)
  G->>M: proxied MCP call
  M-->>A: tool result
```

**Integration status:** structurally complete and Flux-ordered correctly
(CRDs in `infra-controllers` before the CRs in `apps`), but **not yet functional**
— JWT `mode: Strict` + a placeholder bearer means MCP-through-gateway is rejected
until a real Authentik client-credentials token is minted into
`mcp-gateway-token` (or set `mode: Permissive` for staged rollout). Field shapes
(`AgentgatewayPolicy`, tool-name passthrough, the cross-ns `ReferenceGrant`) carry
`VERIFY-BEFORE-DEPLOY` notes.

---

## 13. Applications (the `apps/` toggle)

| App | Purpose | Notable backing services |
|---|---|---|
| `authentik` | Identity / SSO / OIDC | CNPG, Valkey, SeaweedFS (media) |
| `ollama` | LLM serving (GPU, OpenAI-compatible) | 3x P100, LiteLLM, PVC (models) |
| `litellm` | Central model-alias layer | ollama |
| `asr` | Speech-to-text (Qwen3-ASR) | 1x P100 (transformers backend) |
| `searxng` | Meta-search (JSON enabled) | — |
| `n8n` | Workflow automation / Signal notify | CNPG, Valkey |
| `mem0` | Agent long-term memory (pgvector) | CNPG |
| `phoenix` | LLM trace observability | CNPG |
| `kagent` | AI agent fleet (22 agents + MCP) | LiteLLM, mem0, n8n, searxng-mcp |
| `agentgateway` | MCP JWT + CEL enforcement | Authentik (OIDC) |
| `research` | Deep-research backend | mcp-searxng + gpt-researcher |
| `mqtt` | EMQX broker (3-node) | — |
| `homeassistant` | Home automation | EMQX |
| `zigbee2mqtt` | Zigbee bridge | EMQX, coordinator |
| `gotenberg` / `docling` / `pandoc` | Document convert / parse / generate | gVisor (docling, pandoc) |

Enable/disable an app by adding/removing its line in
`kubernetes/apps/kustomization.yaml`.

---

## Validation

```bash
kustomize build kubernetes/infrastructure/controllers
kustomize build kubernetes/infrastructure/configs
kustomize build kubernetes/apps
kustomize build kubernetes/clusters/production
yamllint -c .yamllint kubernetes/
task sops:check
cd tofu && tofu init -backend=false && tofu validate
```
