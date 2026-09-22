# LLM gateway evaluation: agentgateway (kgateway) vs Agent Router (ex-Envoy AI Gateway)

Research date: **2026-09-16**. Web research against primary sources (official docs,
release notes, GitHub issues/APIs); every claim below was URL-verified by the
research agents unless marked otherwise. Question asked: *would kgateway be a
better fit than the current gateway for this cluster's stack (hermes, langfuse,
local llama.cpp, OpenRouter, authentik, Cilium)?*

## 0. The landscape moved under both names

Two renames materially change the comparison:

| | What we run today | What "kgateway's AI features" means now |
|---|---|---|
| Project | **Envoy AI Gateway → "Agent Router"**, moved from CNCF Envoy to the **Agentic AI Foundation** on 2026-09-10 ([rename blog](https://theagentrouter.ai/blog/envoy-ai-gateway-is-now-agent-router), [AAIF](https://aaif.io/blog/agent-router-joins-aaif)) | **agentgateway** — split out of kgateway in **v2.2.0 (Feb 2026)**; kgateway's Envoy data plane no longer routes LLM traffic ([v2.2 notes](https://kgateway.dev/docs/envoy/2.2.x/reference/release-notes/)) |
| Unchanged | CRDs, `aigateway.envoyproxy.io` group, Helm charts, namespace — deployed manifests keep working | new API group `agentgateway.dev`, own chart/controller/namespace |
| Version | v1.1.0 (2026-08-21), v1.0.0 GA 2026-06-23, v1beta1 under SemVer | agentgateway v1.5.0 (2026-08-27); kgateway v2.4.4 (2026-08-31) |

So the real question is **Agent Router (ex-EAG) vs agentgateway** — not
"Envoy AI Gateway vs kgateway".

## 1. Verdict for this stack

**Stay on Agent Router.** The rename is cosmetic here: our `AIGatewayRoute` /
`AIServiceBackend` / `BackendSecurityPolicy` / `SecurityPolicy` objects apply
unchanged, and v1.1.0 already shipped features we use (per-request credential
override, GenAI-semconv tracing). Migration buys three specific things and costs
a full policy re-expression plus kgateway-project churn.

**agentgateway wins on three specifics** (all URL-verified):

1. **Native authentik integration for MCP** — a first-class `provider: Authentik`
   MCP auth type that auto-derives `/jwks/`, bridges OIDC discovery, and injects
   a DCR endpoint ([docs](https://agentgateway.dev/docs/kubernetes/latest/documentation/mcp/auth/authentik/)).
   Matters only if hermes adopts MCP OAuth flows.
2. **First-class provider table** — OpenRouter, DeepSeek, and arbitrary
   OpenAI-compatible upstreams are documented rows, not generic
   OpenAI-schema entries ([providers](https://agentgateway.dev/docs/kubernetes/latest/integrations/llm/providers/openai-compatible/)).
3. **Turnkey Langfuse + budget controls** — documented agentgateway → OTel
   collector → Langfuse path, plus virtual keys with per-key token budgets and
   cost tracking ([langfuse blog](https://agentgateway.dev/blog/2026-02-17-agentgateway-langfuse-integration/), [budget limits](https://agentgateway.dev/docs/kubernetes/main/documentation/llm/cost-controls/budget-limits/)).

**Neither wins on Cilium coexistence** — both are ordinary second GatewayClasses
(next to `cilium`), and neither publishes Cilium-specific guidance. The only
shared coupling is the `gateway.networking.k8s.io` CRDs and a second LB IP.

## 2. Capability comparison (what we actually use)

| Capability | Agent Router (current) | agentgateway |
|---|---|---|
| Model routing by header/body | ✅ `x-ai-eg-model`, ext-proc extracts model from body | ✅ equivalent |
| Per-model fallback chain | ✅ prioritized backendRefs + `modelNameOverride` + EG retry (`numAttemptsPerPriority`) — **this is what our `chat` chain uses** | ✅ priority groups + CEL `unhealthyCondition` eviction |
| Local OpenAI-compatible upstream (llama.cpp) | ✅ generic OpenAI schema (how we run the P100) | ✅ vLLM/custom provider docs, `backendRef` is namespace-local only |
| External OpenAI-compatible (OpenRouter/Synthetic) | ⚠️ API surface was there (`BackendSecurityPolicy` APIKey) but the Synthetic credential was a placeholder the upstream rejected — never proven live before the migration | ✅ `AgentgatewayBackend.policies.auth.secretRef`, both vhosts live; Synthetic additionally exposed as **per-tier paths** — see §5 |
| JWT auth via authentik JWKS | ✅ EG `SecurityPolicy` + `remoteJWKS.uri` (our live config) | ✅ `AgentgatewayPolicy.jwtAuthentication` (+ `jwks.remote` with `backendRef`) |
| Prompt guards | ❌ not in OSS core (verified by release-note sweep) | ✅ regex/moderation/webhook guardrails (response masking not on streams) |
| Token rate limiting | ✅ via Redis + `llmRequestCosts` (soft quota, charges at stream end) | ✅ local/global, token units, `x-ratelimit-*` headers |
| Cost accounting | ⚠️ token counts only, no $ model | ✅ model cost catalog → realized USD; virtual-key budgets |
| MCP gateway | ✅ `MCPRoute` (hostnames, tool routing/filtering, CEL authz) | ✅ native MCP incl. Virtual MCP federation |
| LLM spans/traces | ✅ OTLP; v1.1 GenAI semconv (OpenInference default) | ✅ OTel GenAI semconv; documented Langfuse path |
| CRD footprint | 6 AI CRDs + EG (already running) | 3 `agentgateway-crds` (+ kgateway's ~12–15 if not standalone) |
| API stability | v1beta1, SemVer since 1.0 | 1.x with fast minor churn; AI API broke kgateway 2.1→2.2 |

## 3. Known risks on the current gateway (issue-tracker verified)

These are real and worth knowing before adding more traffic to it:

- **Fail-open failure mode (#2560)** — if the ext-proc sidecar is healthy but not
  attached to the filter chain, requests reach providers unauthenticated while
  every CR still reports `Accepted`. Detector: `run.sock` absent from a
  config_dump. Scariest single issue for an auth-gated gateway.
- **QuotaPolicy accounting bugs (#2551, #2550, #2460)** — per-tenant distinct
  budgets pool into a shared bucket; changes can silently stop applying.
- **Self-hosted OpenAI-schema streaming edge cases (#2463, #2490)** — #2490 is
  literally "Qwen streaming breaks Claude Code subagents with file tools",
  directly relevant to our local Qwen + hermes agents.
- **No end-user attribution in spans (#2589)**, no built-in cost model — langfuse
  remains required for LLM observability regardless of gateway choice.

## 4. Migration cost, if it were ever taken

Re-express every policy in a new API group: `SecurityPolicy.jwt` →
`AgentgatewayPolicy.jwtAuthentication`; `BackendSecurityPolicy` →
`AgentgatewayBackend.policies.auth`; rate limits → descriptors/budgets; chains →
priority groups. Prefer **standalone agentgateway** (3 CRDs, Rust proxy) over the
full kgateway stack — we already have a Cilium edge and do not need a second
general-purpose Envoy edge.

## 5. Route inventory of the deployed agentgateway lane

What the migrated lane actually exposes (read off the live cluster 2026-09-17:
namespace `agentgateway`, HTTPRoute `local-llm` on the in-cluster Gateway
`agentgateway`; the public host `llm.aigateway.svc.ngoldack.de` fronts the same
paths via the `llm-edge` route, and gateway-scoped `AgentgatewayPolicy`
`jwt-auth` gates every one of them with authentik JWTs). Rules are evaluated
**first match wins**, so this order is also the precedence order:

| Path (`PathPrefix`) | Backend | Upstream |
|---|---|---|
| `/v1/chat/completions` | `local-p100` | llmkube llama.cpp on the P100 via `llama-kv-broker`, `/v1/chat/completions` |
| `/v1/models` | `local-p100` | same, passthrough via `llama-kv-broker` (the "Models" route type 501s for custom providers) |
| `/v1/synthetic-small` | `synthetic-small` | `api.synthetic.new`, model `syn:small:text` |
| `/v1/synthetic-large` | `synthetic-large` | `api.synthetic.new`, model `syn:large:text` |
| `/v1/synthetic-deepseek` | `synthetic-hf-deepseek` | `api.synthetic.new`, model `hf:deepseek-ai/DeepSeek-V4.1-Flash` |
| `/v1/chat` | `chat-chain` | priority groups: `synthetic-small` → `synthetic-large` → `synthetic-hf-deepseek` (the `openrouter-flash` leg was removed 2026-09-22) |

`/v1/chat` keeps the original tiered chain because existing clients depend on
it; the three `/v1/synthetic-*` paths are the *same* providers with the
priority chain removed, so a client that needs a deterministic model (cost, or
a model that has to answer at all) can pin one tier instead of hoping the chain
walks as expected. The single-tier backends in `backends.yaml` are verbatim
copies of the corresponding `chat-chain.yaml` stanzas, so each tier's model
string, `secretRef` and SNI appear in two files — edit both together.

Ordering constraint: `/v1/chat` must never be widened to a bare `/v1` prefix
and must stay below the more specific rules. With first match wins, a broad
`/v1` rule placed above them would swallow `/v1/models` and every
`/v1/synthetic-*` path. (`PathPrefix` matches on path segments, so `/v1/chat`
does not capture `/v1/synthetic-*` today — the ordering is what keeps it that
way after later edits.)

Two behaviours worth knowing before debugging a tier:

- **Credential handling.** `AgentgatewayBackend.policies.auth.secretRef` reads
  the Secret's `Authorization` key, strips a leading `Bearer ` if present, and
  writes `Authorization: Bearer <value>` upstream. The Secret may therefore
  hold either the bare key or a full `Bearer …` header value.
- **History of `synthetic-key`:** on the retired Agent Router lane the Synthetic
  credential was a placeholder that never worked live. On this lane the Secret
  held a placeholder too until it was replaced with a live key (commit
  `17726f8`, verified 2026-09-17: a JWT-authenticated `/v1/chat` call returned
  a real completion in ~1.8 s). Treat a Synthetic tier that answers with 401 as
  a bad key in that Secret, not as a chain problem.
- **[2026-09-22] The OpenRouter fallback this section probes was removed from
  every chain.** The group-advance semantics below still describe the
  mechanism, which would apply again if a second group were added back.
- **A 401 from Synthetic is non-retriable, so `chat-chain` does NOT fall
  through to a later group on it.** Priority groups advance on *unhealthy*
  responses, and agentgateway's default `unhealthyCondition` is any 5xx or a
  connection failure — a 4xx auth rejection is neither, so the client gets the
  401 and the OpenRouter tier is never tried. A broken Synthetic key therefore
  takes down `/v1/chat` even though a healthy fallback is configured behind it;
  `/v1/synthetic-*` gives a way to see that failure directly. Probed live
  2026-09-17 with a throwaway two-group backend (group 1 = Synthetic with a
  deliberately bogus Secret, group 2 = OpenRouter with the real key): the
  response was `401 {"error":"Invalid API Key."}` — the OpenRouter group was
  never tried. The same backend pointing group 1 at the live `synthetic-key`
  returned `200` with `model: zai-org/GLM-4.7-Flash` for `syn:small:text`.

## 6. Unresolved / not verified

- No official Cilium + (agentgateway | Agent Router) coexistence doc exists for
  either project; the coexistence claims are Gateway API mechanism reasoning.
- agentgateway GatewayClass conformance report not separately verified.
- All production-adoption evidence found for both projects is vendor-published.
- Agent Router has no dated public roadmap past the v1.1 "What's Next" section.

## Sources

Primary docs/releases: [theagentrouter.ai release notes](https://theagentrouter.ai/release-notes/),
[agentgateway docs](https://agentgateway.dev/docs/kubernetes/latest/),
[kgateway v2.2 notes](https://kgateway.dev/docs/envoy/2.2.x/reference/release-notes/),
[Envoy Gateway JWT task](https://gateway.envoyproxy.io/v1.9/tasks/security/jwt-authentication/),
[agentgateway Langfuse integration](https://agentgateway.dev/blog/2026-02-17-agentgateway-langfuse-integration/),
[authentik MCP auth](https://agentgateway.dev/docs/kubernetes/latest/documentation/mcp/auth/authentik/).
Issue tracker: [agent-router#2560](https://github.com/theagentrouter/agent-router/issues/2560),
[#2551](https://github.com/theagentrouter/agent-router/issues/2551),
[#2490](https://github.com/theagentrouter/agent-router/issues/2490),
[#2589](https://github.com/theagentrouter/agent-router/issues/2589).
