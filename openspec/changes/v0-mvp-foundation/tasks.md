## 1. Repo reduction & layout

- [ ] 1.1 Branch `v0-mvp-foundation`; rename `kubernetes/clusters/production` → `kubernetes/clusters/homelab` and fix Flux Kustomization `path:` references
- [ ] 1.2 Delete out-of-scope apps from `kubernetes/apps/` (vllm, ollama, whisper, knowledge, research, phoenix, seaweedfs, mqtt, zigbee2mqtt, homeassistant, gotenberg, docling, pandoc, mem0, n8n, kagent, agentgateway, authentik, authentik-outpost, technitium, litellm) and remove their entries from `apps/kustomization.yaml`
- [ ] 1.3 Trim `infrastructure/controllers` to: cilium, gateway-api, nvidia-device-plugin, storage CSI, observability, cert-manager(only if needed by v0 — otherwise defer to v1); delete the rest (spire, tetragon, security/trivy, crowdsec, external-dns, traefik, netbird, agent-substrate, khook, kagent, seaweedfs-operator, emqx-operator, cnpg/valkey operators, keda, headlamp)
- [ ] 1.4 Trim `infrastructure/configs` to runtime-classes + cilium config; delete kyverno-policies, tetragon-policies, cluster-issuers, edge-ingress, object-storage
- [ ] 1.5 `kustomize build` all overlays + `yamllint` pass on the reduced tree

## 2. tofu — single homelab node + GPU

- [ ] 2.1 Reduce `node_pools` to one `pmx-main` node (control-plane, schedulable); remove edge.tf / multi-node pools; add `site=homelab` nodeLabel
- [ ] 2.2 Add NVIDIA Talos extensions + kernel modules pinned to a Pascal-supporting `580.159.04` build; gate on `gpu=true`
- [ ] 2.3 `tofu fmt` + `tofu init -backend=false && tofu validate` pass

## 3. Cilium networking

- [ ] 3.1 Cilium HelmRelease: kube-proxy replacement, Hubble, Gateway API, LB-IPAM pool, encryption-ready; Talos `cni:none` + `proxy.disabled`
- [ ] 3.2 `kustomize build infrastructure/controllers` + kubeconform pass

## 4. Storage

- [ ] 4.1 Default StorageClass via CSI (TrueNAS CSI or local-path for single-node MVP — per design open question)
- [ ] 4.2 Verify a test PVC manifest builds/validates

## 5. Observability (metrics + logs + dashboards)

- [ ] 5.1 VictoriaMetrics (single) + Loki (single-binary, filesystem PVC) HelmReleases
- [ ] 5.2 OTel collector DaemonSet (filelog + hostmetrics) with `tolerations: [operator: Exists]` so the control-plane/GPU node is covered → Loki + VictoriaMetrics
- [ ] 5.3 Grafana with VictoriaMetrics + Loki datasources (pinned uids) + "Container Logs (All)" and cluster-overview dashboards
- [ ] 5.4 `kustomize build` + kubeconform + yamllint pass

## 6. llama.cpp (qwen3.5:9b on P100)

- [ ] 6.1 `apps/llama-cpp`: Deployment with the upstream CUDA `llama-server` image, `runtimeClassName: nvidia`, `nvidia.com/gpu: 1`, node pinned to `site=homelab`
- [ ] 6.2 Pascal tuning args: `-ngl 99`, `--flash-attn`, `--cont-batching`, `--parallel`, `--ctx-size 16384`, `--mlock`, `--metrics`; weights on a PVC; OpenAI-compatible `:8080`; prometheus scrape annotations
- [ ] 6.3 Add `llama-cpp` to `apps/kustomization.yaml`; build + kubeconform pass

## 7. Validate & ship

- [ ] 7.1 Full matrix: `kustomize build` ×(clusters/homelab, controllers, configs, apps) + `kubeconform -strict -ignore-missing-schemas` + `yamllint -c .yamllint` + `tofu validate`
- [ ] 7.2 SOPS: any new secret is a `*.sops.yaml`, encrypted in place; `task sops:check` passes
- [ ] 7.3 Per-group Conventional Commits; open PR; on merge, manual `tofu apply` then `flux reconcile`; verify node Ready, `nvidia.com/gpu`, a `/v1/chat/completions` call, logs in Grafana
