# AI + Home Automation Roadmap (TODO)

Tracks the remaining work from the "Homelab AI + Home Automation Spec". The
current PR wires **only the MCPs that already exist** (Mem0 `memory`, `n8n-tools`,
and the built-in `KubernetesTools`) and adds the `k8s-sre-agent` (the explicit
k8sgpt replacement). Everything below is **not yet implemented** — it is captured
here as the backlog. Keep each item GitOps-managed, SOPS-encrypted, and
operator-first where the operator is mature.

## Done in this PR
- [x] Expose the Phoenix trace UI (`phoenix.netbird.<DOMAIN>`, Authentik forward-auth).
- [x] kagent agents split one-file-per-agent under `kubernetes/apps/kagent/`.
- [x] `k8s-sre-agent` (replaces k8sgpt) wired to KubernetesTools + Mem0 + n8n-tools.
- [x] Home-automation platform deployed: EMQX (operator), Home Assistant (Helm), Zigbee2MQTT (Helm).
- [x] EMQX MQTT auth (built_in_database, bootstrapped zigbee2mqtt + homeassistant users).
- [x] Document-tooling backing services deployed: gotenberg, docling, carbone (+ MCP wrapper CRDs, unwired).

## Home automation platform (new namespaces: `homeassistant`, `zigbee2mqtt`, `mqtt`)
- [x] **MQTT / EMQX Operator** — `emqx-operator` (controllers) + a 3-node `apps.emqx.io/v2beta1 EMQX` cluster (`apps/mqtt`), MQTT `:1883` via the `emqx-listeners` Service, dashboard internal-only (SOPS password), volume on `tns-fast-nvmeof`.
  - [x] MQTT `built_in_database` password auth + bootstrapped `zigbee2mqtt` + `homeassistant` users (`apps/mqtt/emqx-auth.sops.yaml`). The HA MQTT password is entered in the HA UI.
- [x] **Home Assistant** — deployed via the battle-tested **pajikos** Helm chart (`apps/homeassistant`), StatefulSet + managed config, PVC on `tns-fast-nfs`, exposed over NetBird behind Authentik forward-auth (`homeassistant.netbird.<DOMAIN>`). (The `przemekhys/homeassistant-operator` was evaluated but ships only a raw `install.yaml` with an unverified CRD — not battle-tested enough.)
- [x] **Zigbee2MQTT** — official Helm chart (`apps/zigbee2mqtt`), persistent storage, `homeassistant: true`, `permit_join: false`, MQTT → EMQX, MQTT password via Flux `valuesFrom`. **Network coordinator** `tcp://slzb06.home.arpa:6638`; if USB, label a node `zigbee=true` (the nodeSelector is set).
- [ ] Verify the end-to-end path on first deploy: Z2M → EMQX → Home Assistant MQTT discovery → HA automations (and confirm chart value paths + the coordinator port for your hardware).

## MCP servers to build (referenced by the agent fleet)
Deploy each as a kagent `McpServer` (or MCP-like tool endpoint), then wire into agents:
- [ ] `browser-research` — web/docs/changelogs/CVEs/GitHub releases.
- [ ] `flux-git` — read Flux/Kustomize/Helm state, diffs, release files.
- [ ] `homeassistant-api` — read entities/states/automations/services; create/update automations.
- [ ] `zigbee2mqtt-api` — bridge health, devices, pairing, network map.
- [ ] `mqtt-admin` — topic browse, publish/test, ACL/auth validation.
- [~] `docling` — backing service deployed (`apps/docling`); **needs an MCP endpoint** (run docling-mcp against docling-serve) before wiring into agents.
- [~] `carbone` — backing service scaffolded (`apps/carbone`, **needs a license**); needs an MCP/OpenAPI bridge.
- [~] `gotenberg` — backing service deployed (`apps/gotenberg`); needs an MCP/OpenAPI bridge.
- [ ] `langfuse-otel` → **phoenix-otel** — trace context / debugging metadata (we use Arize Phoenix, not Langfuse).
- [ ] `filesystem/artifacts` — artifact read/write for document + research agents.

> The `docling`/`gotenberg`/`carbone` McpServer CRDs exist (`apps/kagent/mcp-servers/document-tools.yaml`) but are not yet wired into any agent — they go healthy once each service has an MCP endpoint in front (see the file header).

## Remaining agents (one file per agent, very detailed prompts)
- [ ] `11-gitops-flux-agent` (flux-git, kubernetes-tools, browser-research, n8n-tools)
- [ ] `12-observability-agent` (phoenix-otel, browser-research, kubernetes-tools, n8n-tools)
- [ ] `20-homeassistant-expert` (homeassistant-api, mqtt-admin, browser-research, n8n-tools, memory)
- [ ] `21-zigbee-mqtt-agent` (zigbee2mqtt-api, mqtt-admin, homeassistant-api, browser-research, n8n-tools) — never assume active-active Z2M.
- [ ] `30-researcher-agent` (browser-research, docling, filesystem/artifacts, memory)
- [ ] `31-document-author-agent` (docling, carbone, gotenberg, filesystem/artifacts)
- [ ] `32-spreadsheet-analyst-agent` (docling, carbone, filesystem/artifacts)
- [ ] `40-cluster-update-specialist` (browser-research, flux-git, kubernetes-tools, docling, filesystem/artifacts, n8n-tools, memory) — emits a 1–10 risk score + rollback plan.
- [ ] `41-database-specialist` (kubernetes-tools, browser-research, filesystem/artifacts, n8n-tools)
- [ ] `42-identity-network-agent` (kubernetes-tools, browser-research, n8n-tools)
- [ ] Wire each specialist into `00-main-orchestrator` once it exists.

## n8n + ops
- [ ] Example n8n workflow hooks: notifications + agent invocation (Signal, HA webhook, email, cron).
- [ ] READMEs/runbooks: Zigbee pairing, MQTT credential rotation, HA recovery.

## Security / validation checklist (first deploy)
- [ ] All credentials SOPS-encrypted; least-privilege for mutating agent tools.
- [ ] EMQX dashboard + Z2M frontend internal-only; HA behind Authentik.
- [ ] Every mutating agent action traceable in Phoenix.
- [ ] Confirm kagent/khook CRD apiVersions + the Mem0 server image/MCP endpoint
      (see the `VERIFY-BEFORE-DEPLOY` notes in the manifests).
