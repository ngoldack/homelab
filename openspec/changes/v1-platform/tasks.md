## 1. Backup to Hetzner S3

- [ ] 1.1 tofu: Hetzner S3 bucket (hcloud/object storage) at the `cloud` site; output endpoint/bucket; creds in `*.sops.yaml`
- [ ] 1.2 Backup controller (CNPG Barman plugin for DBs + cluster-state backup) targeting the bucket
- [ ] 1.3 A scheduled backup runs and is restorable (documented restore check)

## 2. Hetzner ingress node (cloud site)

- [ ] 2.1 tofu: smallest Hetzner VPS as a Talos node, public IP, `site=cloud`, joined to the cluster
- [ ] 2.2 Cilium spanning homelab↔cloud with transparent encryption; verify L3 reachability
- [ ] 2.3 Cilium Gateway API entrypoint on the cloud node; external traffic routes to homelab services

## 3. cert-manager + TLS

- [ ] 3.1 cert-manager + a production ClusterIssuer (ACME)
- [ ] 3.2 Wildcard/edge certificate issued for the public domain

## 4. Authentik (identity)

- [ ] 4.1 `apps/authentik` (its own Postgres via CNPG + Valkey) behind the edge Gateway
- [ ] 4.2 OIDC provider + forward-auth outpost; protect one UI end-to-end as proof

## 5. Data operators

- [ ] 5.1 CloudNativePG operator + Barman plugin (controllers)
- [ ] 5.2 Valkey operator (controllers)
- [ ] 5.3 Each app that needs a DB/cache gets its OWN instance + credentials (pattern documented)

## 6. LiteLLM (model gateway)

- [ ] 6.1 `apps/litellm`: config-only proxy mapping aliases (`default`/`fast`/`embeddings`) → the v0 llama.cpp Service (OpenAI base url)
- [ ] 6.2 Expose `litellm.<ns>.svc:4000/v1`; no model redeploy (reuse llama.cpp)

## 7. Validate & ship

- [ ] 7.1 Full matrix (kustomize build ×N + kubeconform + yamllint + tofu validate) + `task sops:check`
- [ ] 7.2 Per-capability Conventional Commits; PR; on merge `tofu apply` (cloud node + bucket) then `flux reconcile`
- [ ] 7.3 Verify: backup completes; external request reaches an Authentik-protected service over TLS; `curl` LiteLLM alias returns a completion from llama.cpp
