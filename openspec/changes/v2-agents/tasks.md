## 1. n8n (workflow automation)

- [ ] 1.1 `apps/n8n`: deployment with its OWN CNPG Postgres + Valkey (per the v1 data-operator pattern)
- [ ] 1.2 Point n8n LLM/agent steps at the agentgateway endpoint (`default` alias)
- [ ] 1.3 Expose n8n behind Authentik (forward-auth) via the edge Gateway

## 2. kagent (agent fleet)

- [ ] 2.1 kagent operator/CRDs in `infrastructure/controllers`
- [ ] 2.2 `apps/kagent`: a minimal agent set + a ModelConfig pointing at the agentgateway `default` alias
- [ ] 2.3 Built-in tool server / MCP scaffolds wired; verify an agent completes a tool call

## 3. agentgateway (MCP authorization)

- [ ] 3.1 agentgateway operator in `infrastructure/controllers`
- [ ] 3.2 Per-agent JWT validation (Authentik client-credentials) + CEL tool-authz policy in front of kagent MCP servers
- [ ] 3.3 Verify an unauthorized MCP call is rejected, an authorized one passes

## 4. mem0 (agent memory)

- [ ] 4.1 `apps/mem0`: its OWN pgvector CNPG instance + credentials
- [ ] 4.2 agentgateway gains the `embeddings` alias; mem0 uses it for embeddings and `default` for extraction
- [ ] 4.3 Register mem0 as a kagent MCP memory server; verify a store + recall

## 5. Validate & ship

- [ ] 5.1 Full matrix (kustomize build ×N + kubeconform + yamllint) + `task sops:check`
- [ ] 5.2 Per-app Conventional Commits; PR; on merge `flux reconcile`
- [ ] 5.3 End-to-end: an n8n workflow and a kagent agent both produce a completion through agentgateway → llama.cpp; mem0 recall works; agentgateway enforces auth
