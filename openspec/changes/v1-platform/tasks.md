## 1. Backup to Hetzner S3

- [ ] 1.1 tofu: Hetzner S3 bucket (hcloud/object storage) at the `cloud` site; output endpoint/bucket; creds in `*.sops.yaml`
- [ ] 1.2 Backup controller (CNPG Barman plugin for DBs + cluster-state backup) targeting the bucket
- [ ] 1.3 A scheduled backup runs and is restorable (documented restore check)

## 2. Cloud cluster (Hetzner) + Cluster Mesh

- [ ] 2.1 `kubernetes/clusters/cloud/` Flux entrypoint + `infrastructure/cloud/` (own Cilium id 2/name cloud + mesh, shared Gateway CRDs)
- [ ] 2.2 tofu: re-add the Hetzner VPS as its OWN single-node Talos cluster (separate machine secrets, hcloud provider), `cluster.id=2`, cp schedulable
- [ ] 2.3 Enable Cluster Mesh on home (id 1) too; `cilium clustermesh connect` home↔cloud. KEY: the clustermesh-apiserver must be reachable across WAN/NAT — expose via the cloud edge or a WireGuard/VPN transport
- [ ] 2.4 Verify `cilium clustermesh status` healthy + a global Service (`service.cilium.io/global: "true"`) resolves cross-cluster
- [ ] 2.5 Cilium Gateway on the cloud cluster; external traffic routes to home global Services

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

## 6. agentgateway (model gateway)

- [ ] 6.1 Install agentgateway (Gateway API CRDs already in v0; `agentgateway-crds` + `agentgateway` OCI Helm charts from `cr.agentgateway.dev`)
- [ ] 6.2 `apps/agentgateway`: routes mapping stable model names → the v0 llama.cpp InferenceService backends (OpenAI base url); no model redeploy
- [ ] 6.3 Wire agentgateway OTel traces → the OTel collector + Prometheus metrics scraped into VictoriaMetrics; verify an LLM call is observable end-to-end

## 7. In-cluster S3 (SeaweedFS)

- [ ] 7.1 SeaweedFS operator (https://github.com/seaweedfs/seaweedfs-operator) in `infrastructure/controllers`; volume data on `tns-fast-nvmeof`
- [ ] 7.2 Per-app bucket + credentials pattern (own bucket/creds, no sharing; MinIO/Crossplane excluded), documented
- [ ] 7.3 An app reads/writes its own bucket via the in-cluster S3 endpoint

## 8. Validate & ship

- [ ] 7.1 Full matrix (kustomize build ×N + kubeconform + yamllint + tofu validate) + `task sops:check`
- [ ] 7.2 Per-capability Conventional Commits; PR; on merge `tofu apply` (cloud node + bucket) then `flux reconcile`
- [ ] 7.3 Verify: backup completes; external request reaches an Authentik-protected service over TLS; `curl` agentgateway alias returns a completion from llama.cpp
