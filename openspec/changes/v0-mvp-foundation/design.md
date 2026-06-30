## Context

The current cluster carries 30+ apps and several competing subsystems (vLLM, Ollama, SeaweedFS, SPIRE, Tetragon, Kyverno, NetBird, a research stack, home automation). For an MVP that proves "self-hosted LLM on homelab hardware," almost all of it is noise. v0 strips back to a single-node Talos cluster on `pmx-main` with the minimum needed to run and observe one model.

Hardware: Minisforum AR900i, 96GB DDR5, 1x Tesla P100 (Pascal, CC 6.0, 16GB HBM2). The homelab host is always Proxmox VE; Talos runs as a VM with the P100 passed through (never bare-metal). NVIDIA driver `580.159.04` is installed on the host and confirmed working with the P100.

## Goals / Non-Goals

**Goals:**
- One reproducible Talos node (control-plane + workloads) on the homelab site.
- Cilium as the only networking layer, with its features turned on (kube-proxy replacement, Hubble, LB-IPAM, Gateway API) so later phases need no new network stack.
- Persistent storage via the existing TrueNAS CSI tiers.
- Metrics + logs + traces + dashboards, collecting from every node/pod, all on the VictoriaMetrics stack.
- `qwen3.5:9b` served by llama.cpp on the P100, OpenAI-compatible, fully GPU-offloaded, tuned for Pascal.
- A repo small enough to read in one sitting.

**Non-Goals:**
- No public ingress, TLS, or auth (v1). LAN-only.
- No LiteLLM, databases, or backups yet (v1). v0 consumers hit llama.cpp directly.
- No agents (v2). No multi-node / `ai-host` / `cloud` / `offsite` (later).
- No Loki or Tempo — logs and traces live on VictoriaLogs / VictoriaTraces.

## Decisions

- **llama.cpp over vLLM/Ollama.** vLLM dropped Pascal; Ollama works but llama.cpp gives the finest control for old hardware and a clean path to LMCache and the future dual-P100 `ai-host`. Run the upstream CUDA `llama-server` image, OpenAI-compatible endpoint on `:8080`.
- **P100/Pascal tuning** (all enabled): full offload `-ngl 99` (a 9B at Q5_K_M ≈ 6–7GB fits 16GB with room for KV), `--flash-attn` (llama.cpp's FA kernel runs on Pascal and cuts KV memory), FP16 compute (P100 has fast FP16 + HBM2), `--cont-batching` + `--parallel N`, a generous `--ctx-size` (e.g. 16384), `--mlock`, `--metrics` for Prometheus. Weights on a PVC (no re-download on restart). Driver/Talos NVIDIA extension pinned to a **Pascal-supporting** build matching `580.159.04`.
- **Single Talos node** (`allowSchedulingOnControlPlanes`) — one Proxmox VM, no HA in MVP. Cilium installed with kube-proxy replacement so adding nodes later is trivial.
- **Cilium-only networking** — Talos CNI `none` + `proxy.disabled`, Cilium provides everything. Documented as a hard rule so no Service mesh / second CNI creeps in.
- **Observability = the VictoriaMetrics stack end to end:** VictoriaMetrics (metrics) + VictoriaLogs (logs, LogsQL) + VictoriaTraces (OTLP traces) + Grafana, fed by a single OTel collector DaemonSet that exports metrics/logs/traces to the three VM backends. No Loki, no Tempo — one ecosystem, fewer moving parts. The collector tolerates all taints so the single (control-plane/GPU) node is captured.
- **Storage = the four existing TrueNAS CSI tiers** (`fast`=tns-fast-nvmeof, `standard`=tns-fast-nfs [default], `storage`=tns-tank-nfs, `local`=local-path). v0 uses `standard` for weights/monitoring; `fast` is reserved for databases (v1+).
- **Repo reduction is a deletion, not a rewrite.** Keep the proven Flux 3-tier layout; rename `clusters/production` → `clusters/homelab`; delete every out-of-scope app/controller in one sweep; `apps/` ends with just `llama-cpp`.

## Risks / Trade-offs

- **Driver 580 vs Pascal** → The 580 branch dropped consumer Pascal support; the P100 is a *datacenter* card on the datacenter driver. Confirmed working on the host. Mitigation: pin the exact Talos NVIDIA extension tag that carries a Pascal-capable 580.x build; verify `nvidia-smi` + `nvidia.com/gpu` before relying on it.
- **Single node = no HA** → Acceptable for MVP; etcd snapshot to disk; HA deferred.
- **Big-bang app deletion** → Mitigation: do it on a branch, validate all overlays build + kubeconform pass before merge; nothing is deployed automatically (manual Flux reconcile).
- **9B model size vs usefulness** → v0 proves the pipeline; the capable `qwen3.6:35b` lands on the future dual-P100 `ai-host`.

## Migration Plan

1. Branch. Rename `clusters/production` → `clusters/homelab`; update Flux Kustomization paths.
2. Delete out-of-scope apps + controllers; trim `apps/kustomization.yaml` to `llama-cpp`; trim `infrastructure` to Cilium + GPU + storage + observability.
3. Reduce tofu to the single `pmx-main` node (+ NVIDIA extensions, site tags); drop edge/multi-node.
4. Add `apps/llama-cpp`. Validate (kustomize build ×N, kubeconform, yamllint, tofu validate).
5. Merge; manually `tofu apply` then `flux reconcile`.

## Open Questions

- Exact `qwen3.5:9b` GGUF source/quant (Q5_K_M assumed — confirm).
- VictoriaTraces is a young project; confirm the current image/ingestion path (OTLP) and the Grafana datasource plugin before relying on traces in v0.

## Resolved

- **Topology:** homelab is always Proxmox VE; Talos runs as a VM with the P100 via PCIe passthrough (never bare-metal).
- **Storage:** use the four existing TrueNAS CSI tiers; `standard` (`tns-fast-nfs`) is the default; `fast` (`tns-fast-nvmeof`) for databases only.
- **Observability backend:** the VictoriaMetrics stack (VictoriaMetrics + VictoriaLogs + VictoriaTraces) instead of Loki/Tempo.
