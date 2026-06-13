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

## MCP server status

Agents reference **only deployed** MCP servers. Everything else is a flagged
`RemoteMCPServer` scaffold in `mcp-servers/scaffolds.yaml` (and the document
tools) that is **not** referenced by any agent until its backend exists.

| MCP | Status | Backed by |
|---|---|---|
| `mem0-memory-server` | **deployed** | Mem0 (`apps/mem0`) |
| `n8n-tools` | **deployed** | n8n (`apps/n8n`) |
| `kagent-tool-server` | **built-in** | kagent Helm chart (k8s/helm/cilium/... tools) |
| `docling` / `gotenberg` / `carbone` | scaffold | services deployed, need REST→MCP bridge |
| `browser-research` | scaffold | build a SearXNG/Brave MCP bridge |
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
