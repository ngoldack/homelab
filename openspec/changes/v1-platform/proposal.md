## Why

With the v0 foundation serving a model, v1 turns it into a usable platform: it becomes durable (offsite backups), reachable from outside the LAN (the Hetzner ingress node), authenticated (Authentik), and gains the stateful building blocks (Postgres, Valkey) plus the central model layer (LiteLLM) that v2's agents depend on.

## What Changes

- **Clusters `home` + `cloud`.** v1 stands up the **cloud** cluster: one Hetzner VPS = a single-node Talos/Cilium cluster (cp+workloads), and joins it to home via **Cilium Cluster Mesh** (home id 1, cloud id 2). home was built mesh-ready in v0.
- **ADD the cloud cluster + Cluster Mesh** — its own Flux + Cilium (id 2); `cilium clustermesh connect` peers it with home across the WAN. The cloud cluster is the **public ingress edge**: a Gateway + TLS routes external traffic to home Services marked global.
- **ADD offsite backup** of cluster + database state to a Hetzner S3 bucket.
- **ADD Authentik** for SSO/OIDC + forward-auth, fronting exposed UIs.
- **ADD CloudNativePG operator** (per-app Postgres + scheduled S3 backups) and the **Valkey operator**.
- **ADD in-cluster S3** — S3-compatible object storage via the **SeaweedFS operator** (https://github.com/seaweedfs/seaweedfs-operator); each app gets its own bucket + credentials (no MinIO, no Crossplane). Distinct from the Hetzner *offsite-backup* bucket above.
- **ADD LiteLLM** as the central model layer in front of llama.cpp — consumers reference stable aliases (`default`, `fast`, `embeddings`), not the model.
- **ADD cert-manager** for TLS at the edge.
- Synergy: LiteLLM points at the v0 llama.cpp Services; no model is re-deployed.

## Capabilities

### New Capabilities
- `cluster-mesh`: the cloud cluster (Hetzner, id 2) + Cilium Cluster Mesh joining it to home; cross-cluster global services. (home was prepared in v0; offsite (id 3) joins in v3.)
- `offsite-backup`: scheduled backup of cluster + database state to a remote Hetzner S3 bucket.
- `identity`: Authentik-based SSO (OIDC + forward-auth).
- `data-operators`: CloudNativePG and Valkey operators providing per-app database/cache instances with backups.
- `object-storage`: in-cluster S3 (SeaweedFS operator) providing per-app buckets + credentials.
- `model-gateway`: LiteLLM central alias layer in front of llama.cpp.

### Modified Capabilities
- `cilium-networking`: enable Cluster Mesh + the cross-cluster (home↔cloud) encrypted transport.
- `llm-serving`: model is now consumed through LiteLLM aliases rather than directly.

## Impact

- **tofu/**: add the Hetzner `cloud` node (hcloud provider) + S3 bucket; site tags `cloud`.
- **kubernetes/**: `infrastructure/controllers` gains cert-manager, CNPG + Valkey operators, the SeaweedFS operator (in-cluster S3, volume data on `tns-fast-nvmeof`), backup (Velero/CNPG-Barman); `infrastructure/configs` gains cluster-issuers + edge Gateway; `apps/` gains authentik + litellm.
- **Secrets**: Hetzner S3 creds, Authentik secret, DB credentials — all `*.sops.yaml`.
- First **public exposure**: review attack surface; only Authentik-protected routes are external.
