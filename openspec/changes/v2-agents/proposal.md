## Why

With the platform (agentgateway, Postgres, Valkey, auth, ingress) in place, v2 adds the actual automation and agent layer: workflow automation (n8n) and an AI agent fleet (kagent) governed by an MCP gateway (agentgateway), with long-term memory (mem0). Everything reaches the model only through agentgateway → llama.cpp, so adding agents requires no change to model serving.

## What Changes

- **Site `home`.** All v2 components run on the home cluster (the `cloud` node stays ingress-only).
- **ADD n8n** for workflow automation, using its own Postgres (CNPG) + Valkey, with its LLM/agent steps pointed at agentgateway.
- **ADD kagent** — the AI agent fleet (declarative agents + MCP) using the agentgateway `default` alias as its ModelConfig backend.
- **ADD agentgateway** — MCP gateway enforcing per-agent auth (JWT via Authentik) + tool authorization in front of kagent's MCP servers.
- **ADD mem0** — semantic long-term memory on a dedicated pgvector (CNPG) instance, embeddings via the agentgateway `embeddings` alias; exposed to kagent as an MCP memory server.
- Synergy chain: **n8n / kagent → agentgateway → llama.cpp**; mem0 and embeddings reuse the same model layer. No new model server.

## Capabilities

### New Capabilities
- `workflow-automation`: n8n workflows that can call the model layer and other services.
- `agent-platform`: kagent agent fleet + agentgateway MCP authorization.
- `agent-memory`: mem0 semantic memory on pgvector, embeddings via agentgateway.

### Modified Capabilities
- `model-gateway`: agentgateway gains the `embeddings` alias (embedding model served by/through the model layer) and becomes the single entry point for all agent/LLM traffic.
- `identity`: Authentik issues the JWTs agentgateway validates (service-account / client-credentials).

## Impact

- **kubernetes/apps**: add `n8n`, `kagent`, `agentgateway`, `mem0` (each with its own CNPG/Valkey instance + credentials as needed).
- **infrastructure/controllers**: kagent + agentgateway operators/CRDs.
- **Secrets**: n8n/mem0 DB creds, agentgateway JWT/client secret (from Authentik) — all `*.sops.yaml`.
- **Model layer unchanged**: proves the v0/v1 design — agents plug into agentgateway with zero model redeploy.
