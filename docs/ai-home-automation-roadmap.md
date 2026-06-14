# AI + Home Automation Roadmap (TODO)

Tracks the remaining work from the "Homelab AI + Home Automation Spec". The full
**22-agent tree is now scaffolded** under `kubernetes/apps/kagent/` using the
verified kagent schema (`Agent` / `SandboxAgent` / `RemoteMCPServer`, built-in
tools via `kagent-tool-server`). Agents wire **only the MCPs that already exist**
(Mem0 `memory`, `n8n-tools`, and the built-in tool server); every other MCP is a
flagged, **unwired** scaffold. The remaining work below is the MCP backends/bridges
plus their wiring. See `kubernetes/apps/kagent/README.md` for the architecture,
security model, and validation checklist. Keep each item GitOps-managed,
SOPS-encrypted, and operator-first where the operator is mature.

## Done in this PR
- [x] Expose the Phoenix trace UI (`phoenix.netbird.<DOMAIN>`, Authentik forward-auth).
- [x] kagent agents split one-file-per-agent under `kubernetes/apps/kagent/`.
- [x] `k8s-sre-agent` (replaces k8sgpt) wired to KubernetesTools + Mem0 + n8n-tools.
- [x] Home-automation platform deployed: EMQX (operator), Home Assistant (Helm), Zigbee2MQTT (Helm).
- [x] EMQX MQTT auth (built_in_database, bootstrapped zigbee2mqtt + homeassistant users).
- [x] Document-tooling backing services deployed: gotenberg, docling, pandoc (+ MCP wrapper CRDs, unwired). (Carbone removed — CCL/not OSS — replaced by fully-OSS Pandoc.)
- [x] Full agent tree scaffolded (22 agents): main + security orchestrators; research chief + 7 research/doc agents; platform chief + 6 platform agents; homelab chief + 2 home agents; finance + mail as guarded SandboxAgents.
- [x] kagent schema corrected to verified upstream: `RemoteMCPServer` (was invalid `McpServer`), `kagent-tool-server` built-in tools (was invalid `KubernetesTools`), list-form `requireApproval`.
- [x] Security model: HITL `requireApproval` on k8s write (no delete tool); finance/mail network-isolated SandboxAgents, unreachable from main; security-orchestrator as advisory/audit.

## Home automation platform (new namespaces: `homeassistant`, `zigbee2mqtt`, `mqtt`)
- [x] **MQTT / EMQX Operator** — `emqx-operator` (controllers) + a 3-node `apps.emqx.io/v2beta1 EMQX` cluster (`apps/mqtt`), MQTT `:1883` via the `emqx-listeners` Service, dashboard internal-only (SOPS password), volume on `tns-fast-nvmeof`.
  - [x] MQTT `built_in_database` password auth + bootstrapped `zigbee2mqtt` + `homeassistant` users (`apps/mqtt/emqx-auth.sops.yaml`). The HA MQTT password is entered in the HA UI.
- [x] **Home Assistant** — deployed via the battle-tested **pajikos** Helm chart (`apps/homeassistant`), StatefulSet + managed config, PVC on `tns-fast-nfs`, exposed over NetBird behind Authentik forward-auth (`homeassistant.netbird.<DOMAIN>`). (The `przemekhys/homeassistant-operator` was evaluated but ships only a raw `install.yaml` with an unverified CRD — not battle-tested enough.)
- [x] **Zigbee2MQTT** — official Helm chart (`apps/zigbee2mqtt`), persistent storage, `homeassistant: true`, `permit_join: false`, MQTT → EMQX, MQTT password via Flux `valuesFrom`. **Network coordinator** `tcp://slzb06.home.arpa:6638`; if USB, label a node `zigbee=true` (the nodeSelector is set).
- [ ] Verify the end-to-end path on first deploy: Z2M → EMQX → Home Assistant MQTT discovery → HA automations (and confirm chart value paths + the coordinator port for your hardware).

## MCP servers to build (scaffolded, unwired — backends do not exist yet)
Each is a flagged `RemoteMCPServer` in `apps/kagent/mcp-servers/scaffolds.yaml`
(or `document-tools.yaml`), referenced by **no** agent until its backend exists.
Build the backend/bridge, confirm tool names, then wire into the listed agents:
- [x] `browser-research` — DONE via `searxng-mcp` (`apps/research`, MIT, HTTP-native): `searxng_web_search` + `web_url_read` against the in-cluster SearXNG. Wired into research-chief, web-search, document-ingest, cluster-update, k8s-sre. This makes the deep-research pipeline functional. Also deployed `gpt-researcher` (Apache-2.0) as a turnkey Perplexity-style engine on SearXNG+vLLM (REST :8000; agent wiring needs the gptr-mcp wrapper — TODO).
- [ ] `flux-git` — read Flux/Kustomize/Helm state + open Git PRs. Used by gitops-flux + cluster-update.
- [ ] `homeassistant-api` — read states, write automations. Used by homeassistant-expert (+ read for zigbee-mqtt).
- [ ] `zigbee2mqtt-api` — bridge health, devices, network map, pairing. Used by zigbee-mqtt-agent.
- [ ] `mqtt-admin` — EMQX topics/clients/ACL. Used by zigbee-mqtt-agent.
- [~] `docling` / `gotenberg` / `pandoc` — backing services deployed; need REST→MCP bridge (docling-mcp; OpenAPI→MCP for gotenberg/pandoc). Pandoc (GPL) replaced Carbone; XLSX templating dropped (no fully-OSS no-enterprise option — add an openpyxl service if needed).
- [ ] `mail-mcp-read` / `mail-mcp-write` — better-email-mcp (IMAP read; separate SMTP write creds). Used by mail-agent only; write under requireApproval.
- [ ] `scalable-portfolio-api` — Scalable Capital read-only (scraper/wealthAPI). finance-agent only; SOPS creds.
- [ ] `policy-evaluator` / `audit-log` — small custom MCPs for security-orchestrator (advisory).
- [ ] `phoenix-otel` — trace context for observability-agent (Arize Phoenix, not Langfuse).
- [ ] `filesystem/artifacts` — `/output/artifacts` read/write for document + research agents.

## Remaining agent work (tree is scaffolded — see apps/kagent/README.md)
- [ ] Wire each agent's TODO tools once the MCP backend above exists (search the
      manifests for `TODO wire`).
- [ ] Set real `allowedDomains` on finance-agent + mail-agent SandboxAgents.
- [ ] Confirm built-in `kagent-tool-server` tool names + add helm/cilium/prometheus toolsets where useful.

## n8n + ops
- [ ] Example n8n workflow hooks: notifications + agent invocation (Signal, HA webhook, email, cron).
- [ ] READMEs/runbooks: Zigbee pairing, MQTT credential rotation, HA recovery.

## Security / validation checklist (first deploy)
- [ ] All credentials SOPS-encrypted; least-privilege for mutating agent tools.
- [ ] EMQX dashboard + Z2M frontend internal-only; HA behind Authentik.
- [ ] Every mutating agent action traceable in Phoenix.
- [ ] Confirm kagent/khook CRD apiVersions + the Mem0 server image/MCP endpoint
      (see the `VERIFY-BEFORE-DEPLOY` notes in the manifests).
