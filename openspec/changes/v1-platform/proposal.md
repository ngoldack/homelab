## Why

With the v0 foundation serving a model, v1 turns it into a usable platform: it becomes durable (offsite backups), reachable from outside the LAN (the Hetzner ingress node), authenticated (Authentik), and gains the stateful building blocks (Postgres, Valkey) plus the central model layer (LiteLLM) that v2's agents depend on.

## What Changes

- **Sites `homelab` + `cloud`.** The `cloud` (Hetzner) site joins: smallest VPS as a public-IP ingress node, plus Hetzner S3 for backups. `offsite` stays a marker.
- **ADD offsite backup** of cluster state + databases to a Hetzner S3 bucket.
- **ADD the Hetzner ingress node** — a Talos node at the `cloud` site with a public IP; all non-LAN traffic enters here and is routed to homelab services. Uses Cilium features end to end (encryption homelab↔cloud, Gateway API).
- **ADD Authentik** for SSO/OIDC + forward-auth, fronting the UIs that get exposed.
- **ADD CloudNativePG operator** (per-app Postgres instances + scheduled backups to Hetzner S3) and the **Valkey operator**.
- **ADD LiteLLM** as the central model layer in front of llama.cpp — consumers reference stable aliases (`default`, `fast`, `embeddings`), not the model.
- **ADD cert-manager** (if not already in v0) for TLS at the edge.
- Synergy: LiteLLM points at the v0 llama.cpp Service; no model is re-deployed.

## Capabilities

### New Capabilities
- `offsite-backup`: scheduled backup of cluster + database state to a remote Hetzner S3 bucket.
- `public-ingress`: the Hetzner `cloud` ingress node and the path for external traffic into homelab, secured with Cilium + TLS.
- `identity`: Authentik-based SSO (OIDC + forward-auth).
- `data-operators`: CloudNativePG and Valkey operators providing per-app database/cache instances with backups.
- `model-gateway`: LiteLLM central alias layer in front of llama.cpp.

### Modified Capabilities
- `cilium-networking`: add transparent encryption and Gateway-API ingress spanning the homelab↔cloud link.
- `llm-serving`: model is now consumed through LiteLLM aliases rather than directly.

## Impact

- **tofu/**: add the Hetzner `cloud` node (hcloud provider) + S3 bucket; site tags `cloud`.
- **kubernetes/**: `infrastructure/controllers` gains cert-manager, CNPG + Valkey operators, backup (Velero/CNPG-Barman); `infrastructure/configs` gains cluster-issuers + edge Gateway; `apps/` gains authentik + litellm.
- **Secrets**: Hetzner S3 creds, Authentik secret, DB credentials — all `*.sops.yaml`.
- First **public exposure**: review attack surface; only Authentik-protected routes are external.
