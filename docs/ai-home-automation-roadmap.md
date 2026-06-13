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

## Home automation platform (new namespaces: `homeassistant`, `zigbee2mqtt`, `mqtt`)
- [ ] **MQTT / EMQX Operator** — `HelmRepository` `https://repos.emqx.io/charts`; `emqx-operator` HelmRelease; `apps.emqx.io/v2beta1 EMQX` 3-node core cluster, MQTT `:1883` internal, dashboard internal-only, auth via SOPS secret. (Storage on `tns-fast-nvmeof`.)
- [ ] **Home Assistant** — start with the dedicated operator (`przemekhys/homeassistant-operator`); fall back to a `StatefulSet` + PVC if the operator is too immature. PVC-backed `/config` (`tns-fast-nfs`), HTTPS behind Authentik/NetBird, MQTT integration, config backup/export.
- [ ] **Zigbee2MQTT** — official Helm chart (`https://charts.zigbee2mqtt.io`); persistent storage; `homeassistant: true`, `permit_join: false`; MQTT → EMQX; **prefer a network-attached coordinator** (TCP, e.g. `tcp://slzb06...:6638`) over node-local USB. If USB: label a node (`zigbee=true`) + nodeSelector + device access.
- [ ] Verify the end-to-end path: Z2M → EMQX → Home Assistant MQTT discovery → HA automations.

## MCP servers to build (referenced by the agent fleet)
Deploy each as a kagent `McpServer` (or MCP-like tool endpoint), then wire into agents:
- [ ] `browser-research` — web/docs/changelogs/CVEs/GitHub releases.
- [ ] `flux-git` — read Flux/Kustomize/Helm state, diffs, release files.
- [ ] `homeassistant-api` — read entities/states/automations/services; create/update automations.
- [ ] `zigbee2mqtt-api` — bridge health, devices, pairing, network map.
- [ ] `mqtt-admin` — topic browse, publish/test, ACL/auth validation.
- [ ] `docling` — parse PDF/DOCX/XLSX/PPTX.
- [ ] `carbone` — generate DOCX/XLSX/PPTX from templates.
- [ ] `gotenberg` — convert HTML/MD/Office → PDF.
- [ ] `langfuse-otel` → **phoenix-otel** — trace context / debugging metadata (we use Arize Phoenix, not Langfuse).
- [ ] `filesystem/artifacts` — artifact read/write for document + research agents.

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
