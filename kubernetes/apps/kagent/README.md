# kagent — AI Agent System

Multi-tier agent fleet for deep research, platform/SRE automation, and home
automation, plus two guarded personal agents. Runs on kagent (Agent /
SandboxAgent / RemoteMCPServer CRDs) against the in-cluster vLLM
(`qwen3-coder-80b`) via the `local-qwen-main` ModelConfig.

> Status: **greenfield, pre-deploy.** Every manifest is built and linted but the
> cluster is not yet up. Items tagged `VERIFY-BEFORE-DEPLOY` in the manifests must
> be confirmed against the installed kagent CRDs / running services.

## Agent tree

```
main-orchestrator (Tier 0)            ← all inbound events (n8n, khook)
├── research-chief (Tier 1)           ← Priority #1: deep-research pipeline
│   ├── web-search-agent
│   ├── document-ingest-agent
│   ├── synthesis-agent
│   ├── citation-agent
│   └── document-author-agent
│       ├── pdf-converter-specialist  (Tier 3)
│       └── spreadsheet-specialist    (Tier 3)
├── platform-chief (Tier 1)
│   ├── k8s-sre-agent
│   ├── gitops-flux-agent
│   ├── observability-agent
│   ├── database-specialist           (Tier 3)
│   ├── identity-network-agent
│   └── cluster-update-specialist
└── homelab-chief (Tier 1)
    ├── homeassistant-expert-agent
    └── zigbee-mqtt-agent

security-orchestrator (Tier 0, cross-cutting)   ← advisory + audit
finance-agent (GUARDED, SandboxAgent)           ← NOT wired into main
mail-agent    (GUARDED, SandboxAgent)           ← NOT wired into main
```

`main-orchestrator` delegates only to the three chiefs (+ memory/notify). Chiefs
delegate to their workers via kagent "agents-as-tools" (`type: Agent`).

## Security model — what is actually enforced

kagent has **no runtime interceptor** that can force every tool call through a
policy agent. So the "always-in-the-loop security-orchestrator" is implemented as
**advisory + audit**, and the binding controls are the kagent-native primitives:

| Control | Mechanism | Where |
|---|---|---|
| Human approval on writes | `requireApproval: [toolNames]` | k8s-sre apply/patch; mail write |
| No destructive ops | tool simply **omitted** | no `k8s_delete_resource` anywhere |
| Network isolation | `SandboxAgent` deny-by-default + `allowedDomains` | finance, mail |
| Unreachability | guarded agents **not** listed as any agent's tool | finance, mail |
| Scoped creds | separate read/write MCP servers; `headersFrom` Secrets | mail read vs write |
| Advisory review + audit | `security-orchestrator` agent | guarded-path requests |

`security-orchestrator` records decisions and alerts the operator; it does **not**
silently gate calls. Treat "consult security first" as a convention, not a
runtime guarantee.

## Hardening (defence in depth)

Layered enforcement *outside* the LLM, rolled out in phases:

| Layer | Control | Status |
|---|---|---|
| Container | restricted `securityContext` + `/tmp` emptyDir on every Agent (`patches/agent-deployment-hardening.yaml`) | **done** |
| Admission | scoped Kyverno **Enforce** on the `homelab.io/kagent-agent` label (`configs/kyverno-policies/agent-hardening.yaml`) | **done** |
| Prompt | Spotlighting (untrusted-content delimiters) on mail/web/ingest/research/HA agents | **done** |
| Tool-server RBAC | scoped ClusterRole (read-minus-secrets, patch/scale, delete pods only) replacing chart cluster-admin (`controllers/kagent/tool-server-rbac.yaml`) | **done** |
| MCP proxy | agentgateway JWT (Authentik) + CEL tool authz, MCP routed through it (`apps/agentgateway`) | **done** |
| Network | Cilium default-deny egress for agents (`apps/kagent/network`) | **done** |
| Runtime | Tetragon TracingPolicies (shell/SA-token/egress) → stdout→OTel→Loki (`configs/tetragon-policies`) | **done** |
| Workload identity | SPIRE `k8s_psat` per-agent SVIDs (`controllers/spire`) | **done** (SVIDs issued; mTLS not yet consumable) |
| Sandbox | Agent Substrate (gVisor) for the guarded SandboxAgents (`controllers/agent-substrate`) | **done** (experimental) |
| Pod Security | `restricted` PSA enforced on the `kagent` namespace (`controllers/kagent/namespace.yaml`) | **done** |
| Container runtime | `gvisor` RuntimeClass; docling sandboxed under runsc (`configs/runtime-classes`) | **done** |

**Known limits (verified):**
- Tool-server RBAC is allow-only: "no secrets / no delete" = simply not granted. Pod delete (restart) is allowed; namespaces/CRDs/PVCs/secrets are not.
- agentgateway enforces tool allowlists via **CEL** (`AgentgatewayPolicy`), not Kyverno; stateful "forbidden tool-call chains" need a BYO ext-authz service. The MCP bearer is a **static** token (no native refresh) — mint via Authentik client-credentials and rotate manually.
- **agentgateway has no SPIFFE support**, so agent→MCP mTLS via SVID is not consumable yet; SPIRE establishes per-agent identity for future use.
- `SandboxAgent` requires **Agent Substrate** (upstream-experimental; adds privileged DaemonSets) — confirm chart value paths + smoke-test gVisor on the Talos kernel before relying on it. Cilium egress + `requireApproval` are the always-on controls underneath.
- Chose **Tetragon over Falco** (same Cilium vendor as the CNI; one eBPF stack).

### Spec checklist — accepted gaps (audited against the 10-layer hardening spec)
Everything in the spec's Complete Hardening Checklist is implemented **except** these,
each a deliberate, documented decision rather than an oversight:
- **`mcp-security-audit` / `mcp-scan` in CI** — N/A for this repo: MCP servers are external `RemoteMCPServer` URL refs, not in-repo server code, and the CI is lint-only (no running cluster). Re-evaluate when an in-repo MCP bridge (docling-mcp, …) is built.
- **Forbidden tool-call-chain policies** — agentgateway CEL is per-call/stateless; sequence rules ("secret-read → external-post") need a BYO ext-authz service behind the gateway. Not built.
- **JIT projected SA tokens (`expirationSeconds: 3600`) + `automountServiceAccountToken: false` per agent** — the kagent `Agent` CRD exposes neither field, and the controller manages agent SAs. Mitigated instead: agent SAs have **no** RoleBindings (deny-all), and the real cluster privilege (the tool-server SA) is scoped off cluster-admin.
- **kagent bundled Postgres → CNPG** — the bundled PG is restricted-PSA-compliant (UID 999) so it doesn't block PSA, but it is dev-grade (single replica, `sslmode=disable`). Migration deferred: the chart only takes a single DSN via `urlFile`, and the mount mechanism needs confirming before cutover. Recommended next step.
- **Prempti (Falco-at-tool-call interception)** — experimental; Tetragon has no direct equivalent. Not adopted.
- **gotenberg under gVisor** — headless Chromium is commonly runsc-incompatible; left on the default runtime pending a smoke test. (pandoc runs under gVisor; carbone was removed — CCL/not OSS, replaced by fully-OSS Pandoc.)

## MCP server status

Agents reference **only deployed** MCP servers. Everything else is a flagged
`RemoteMCPServer` scaffold in `mcp-servers/scaffolds.yaml` (and the document
tools) that is **not** referenced by any agent until its backend exists.

| MCP | Status | Backed by |
|---|---|---|
| `mem0-memory-server` | **deployed** | Mem0 (`apps/mem0`) |
| `n8n-tools` | **deployed** | n8n (`apps/n8n`) |
| `kagent-tool-server` | **built-in** | kagent Helm chart (k8s/helm/cilium/... tools) |
| `searxng-mcp` (web search + fetch) | **deployed** | mcp-searxng (`apps/research`) → SearXNG; wired into the research pipeline |
| `docling` / `gotenberg` / `pandoc` | scaffold | services deployed, need REST→MCP bridge |
| `flux-git` | scaffold | build a Flux+Git MCP |
| `homeassistant-api` / `zigbee2mqtt-api` / `mqtt-admin` | scaffold | build API→MCP bridges |
| `mail-mcp-read` / `mail-mcp-write` | scaffold | deploy better-email-mcp (+ SOPS creds) |
| `scalable-portfolio-api` | scaffold | build scraper/wealthAPI MCP (+ SOPS creds) |
| `policy-evaluator` / `audit-log` | scaffold | small custom MCPs (advisory) |

### Built-in tool names

The platform agents use the chart-installed `kagent-tool-server` with explicit
`toolNames` (`k8s_get_resources`, `k8s_describe_resource`, `k8s_get_pod_logs`,
`k8s_get_events`, `k8s_get_resource_yaml`, and for SRE `k8s_apply_manifest` /
`k8s_patch_resource` under `requireApproval`). Confirm the exact catalog in the
kagent UI → Tool Servers; more toolsets (`helm_*`, `cilium_*`, `prometheus_*`,
`argo_*`) can be added.

## Extending

1. New worker: add `agents/NN-name.yaml` (`kagent.dev/v1alpha2`, `type:
   Declarative`), reference it from its chief as `type: Agent`, list it in
   `kustomization.yaml`.
2. New tool backend: deploy the service, build/point an MCP at it, add a
   `RemoteMCPServer` (or kmcp `MCPServer`), then add it to the relevant agents'
   `tools`. Gate writes with `requireApproval`.
3. New guarded agent: use `SandboxAgent` with `allowedDomains`, keep it out of
   every caller's `tools`, route via the guarded path.

## Validation checklist

Build + lint (host tools; mirrors CI):

```bash
kustomize build kubernetes/apps >/dev/null            # includes apps/kagent
kustomize build kubernetes/infrastructure/controllers >/dev/null
kustomize build kubernetes/infrastructure/configs >/dev/null
kustomize build kubernetes/clusters/production >/dev/null
yamllint -c .yamllint kubernetes/
task sops:check
```

Pre-deploy (against a live kagent install):

- [ ] `kubectl get crd | grep kagent.dev` shows Agent, SandboxAgent,
      RemoteMCPServer, MCPServer, ModelConfig, Khook.
- [ ] `ModelConfig local-qwen-main` Ready (vLLM reachable, `kagent-vllm` secret).
- [ ] `kagent-tool-server` RemoteMCPServer present; tool names match the agents.
- [ ] mem0 + n8n RemoteMCPServers report tools discovered.
- [ ] Each Agent reconciles `Accepted` + `Ready` (no broken tool refs).
- [ ] Scaffold RemoteMCPServers are unhealthy **but unreferenced** (expected).
- [ ] HITL: k8s-sre `apply/patch` shows Approve/Reject; no delete tool exists.
- [ ] finance/mail are SandboxAgents, network-isolated, absent from every
      agent's tool list.
- [ ] khook triggers fire main-orchestrator on CrashLoopBackOff/OOMKill/Flux fail.
```
