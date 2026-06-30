## Why

The repository has grown to 30+ applications across overlapping concerns, most of which are not needed to prove the core value: a self-hosted LLM platform on home hardware. We are resetting to a lean, phased MVP. v0 establishes the foundation everything else builds on — a working Talos cluster on the main host, Cilium-only networking, storage, a monitoring suite, and the first LLM served by llama.cpp on the single Tesla P100.

## What Changes

- **Site `home` only.** All v0 work targets `pmx-main` (Minisforum AR900i, 96GB, 1x P100). The `cloud` (Hetzner) and `offsite` sites are reserved for later phases.
- **ADD** a bootstrapped Talos control-plane/worker on `pmx-main` (single-node cluster), GPU enabled via Talos NVIDIA extensions (host driver 580.159.04 already present).
- **ADD** Cilium as the sole CNI — kube-proxy replacement, Hubble, LB-IPAM, Gateway API enabled. All networking uses Cilium features (no second network layer).
- **ADD** a storage CSI driver (TrueNAS) for persistent volumes.
- **ADD** a minimal monitoring suite: VictoriaMetrics (metrics), Loki (logs), Grafana (dashboards), an OTel collector for node/pod log + host-metric collection.
- **ADD** `llama.cpp` (`llama-server`) serving `qwen3.5:9b` on the single P100, tuned with all Pascal/P100-appropriate optimizations, exposing an OpenAI-compatible API in-cluster.
- **BREAKING — reduce to MVP.** Every app not in the v0/v1/v2 plan is removed: vLLM, Ollama, Whisper, SeaweedFS, EMQX/MQTT, Zigbee2MQTT, Home Assistant, the research stack (SearXNG, gpt-researcher, mcp-searxng), Phoenix, knowledge/Open WebUI, document services (gotenberg/docling/pandoc), SPIRE, Tetragon, Trivy, Kyverno, CrowdSec, Technitium, NetBird, Agent Substrate, khook, Headlamp. (LiteLLM, Authentik, CNPG, Valkey, backups, ingress return in v1; n8n/kagent/agentgateway/mem0 in v2.)
- **Simplify the repo:** rename `clusters/production` → `clusters/home`; keep the Flux 3-tier layout (`infrastructure/{controllers,configs}`, flat `apps/`); tag tofu resources by site.

## Capabilities

### New Capabilities
- `talos-cluster`: Talos OS provisioning + Kubernetes bootstrap of the home cluster (4 VMs on pmx-main: cp/wk/wk-gpu/wk-cpu, incl. GPU enablement on wk-gpu).
- `cilium-networking`: Cilium as the exclusive CNI on home (built mesh-ready; the cloud cluster + Cluster Mesh land in v1).
- `cluster-storage`: the four TrueNAS CSI tiers (default `standard`).
- `observability`: metrics + logs + traces + dashboards (VictoriaMetrics + VictoriaLogs + VictoriaTraces + Grafana).
- `llm-serving`: OpenAI-compatible llama.cpp — qwen3.5:9b on the GPU worker, qwen3-coder-next on the dedicated CPU worker.

### Modified Capabilities
- (none — greenfield reset; no existing OpenSpec specs yet.)

## Impact

- **tofu/**: single-node `pmx-main` Talos config, NVIDIA extensions + kernel modules, GPU node label; site tags.
- **kubernetes/**: `clusters/home/` Flux entrypoints; `infrastructure/controllers` reduced to Cilium, cert/CSI, observability; `apps/` reduced to `llama-cpp` only; deletion of all out-of-scope apps.
- **Hardware**: GPU passthrough / Talos NVIDIA driver (580.159.04) + P100 inference tuning.
- **No public exposure** in v0 (LAN only); ingress + TLS + auth arrive in v1.
