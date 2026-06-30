## Why

The repository has grown to 30+ applications across overlapping concerns, most of which are not needed to prove the core value: a self-hosted LLM platform on homelab hardware. We are resetting to a lean, phased MVP. v0 establishes the foundation everything else builds on — a working Talos cluster on the main host, Cilium-only networking, storage, a monitoring suite, and the first LLM served by llama.cpp on the single Tesla P100.

## What Changes

- **Site `homelab` only.** All v0 work targets `pmx-main` (Minisforum AR900i, 96GB, 1x P100). The `cloud` (Hetzner) and `offsite` sites are reserved for later phases.
- **ADD** a bootstrapped Talos control-plane/worker on `pmx-main` (single-node cluster), GPU enabled via Talos NVIDIA extensions (host driver 580.159.04 already present).
- **ADD** Cilium as the sole CNI — kube-proxy replacement, Hubble, LB-IPAM, Gateway API enabled. All networking uses Cilium features (no second network layer).
- **ADD** a storage CSI driver (TrueNAS) for persistent volumes.
- **ADD** a minimal monitoring suite: VictoriaMetrics (metrics), Loki (logs), Grafana (dashboards), an OTel collector for node/pod log + host-metric collection.
- **ADD** `llama.cpp` (`llama-server`) serving `qwen3.5:9b` on the single P100, tuned with all Pascal/P100-appropriate optimizations, exposing an OpenAI-compatible API in-cluster.
- **BREAKING — reduce to MVP.** Every app not in the v0/v1/v2 plan is removed: vLLM, Ollama, Whisper, SeaweedFS, EMQX/MQTT, Zigbee2MQTT, Home Assistant, the research stack (SearXNG, gpt-researcher, mcp-searxng), Phoenix, knowledge/Open WebUI, document services (gotenberg/docling/pandoc), SPIRE, Tetragon, Trivy, Kyverno, CrowdSec, Technitium, NetBird, Agent Substrate, khook, Headlamp. (LiteLLM, Authentik, CNPG, Valkey, backups, ingress return in v1; n8n/kagent/agentgateway/mem0 in v2.)
- **Simplify the repo:** rename `clusters/production` → `clusters/homelab`; keep the Flux 3-tier layout (`infrastructure/{controllers,configs}`, flat `apps/`); tag tofu resources by site.

## Capabilities

### New Capabilities
- `talos-cluster`: Talos OS provisioning + Kubernetes bootstrap on the homelab main host, including GPU (NVIDIA P100) enablement.
- `cilium-networking`: Cilium as the exclusive CNI and the contract that all networking uses Cilium features.
- `cluster-storage`: a CSI-provisioned default StorageClass for persistent workloads.
- `observability`: metrics + logs + dashboards (VictoriaMetrics, Loki, Grafana) with full cluster-wide collection.
- `llm-serving`: OpenAI-compatible LLM inference via llama.cpp on the Tesla P100, tuned for Pascal.

### Modified Capabilities
- (none — greenfield reset; no existing OpenSpec specs yet.)

## Impact

- **tofu/**: single-node `pmx-main` Talos config, NVIDIA extensions + kernel modules, GPU node label; site tags.
- **kubernetes/**: `clusters/homelab/` Flux entrypoints; `infrastructure/controllers` reduced to Cilium, cert/CSI, observability; `apps/` reduced to `llama-cpp` only; deletion of all out-of-scope apps.
- **Hardware**: GPU passthrough / Talos NVIDIA driver (580.159.04) + P100 inference tuning.
- **No public exposure** in v0 (LAN only); ingress + TLS + auth arrive in v1.
