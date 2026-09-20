# PXA vs llama-p100 — Qwen3.6-35B canary A/B on the Tesla P100

**Date:** 2026-09-20 · **Card:** Tesla P100 (sm_60, 16 GiB), single seat `talos-919-w9u`
**Production engine:** `registry.ngoldack.de/llama-p100@sha256:f5839c86…` (llama.cpp v0.4.0 + shinbunbun 29-patch set)
**Canary engine:** `registry.ngoldack.de/pxa:2026.09.13-rc3@sha256:9bf9824d…` (poisonxa16/pxa, commit 3c5b80d, built from source, CUDA 12.8.1, arch 60)

## Summary matrix

| Model | Engine | Cold prefill 8k (pp t/s) | Cold prefill 16k (pp t/s) | Warm prefill | Decode t/s (llm-bench) | Depth t/s | Slot restore | Peak VRAM |
|---|---|---|---|---|---|---|---|---|
| **Qwen3.6-35B UD-Q3_K_S** (same GGUF) | llama-p100 (prod) | 69.6 | n/a | ~130 ms* | **23.68** | 21.71 | **UNRESOLVED** (cached=0, ~69.7 s re-eval) | 15083 MiB |
| **Qwen3.6-35B UD-Q3_K_S** | pxa canary | 67.7 | 78.2 (19.3k) | 190 ms | **14.30** | 11.56 | **FIXED** (cached ≈ 8,768) | 15405 MiB |
| **PXA-Fusion4-35B PXQ2** | pxa canary | 329 | 263 | 424 ms | **40.78** | 24.68 | **FIXED** (cached ≈ 8,768) | 13077 MiB |
| **PXA-Coder-35B PXQ4** (24/41 layers, partial offload) | pxa canary | 126.9 (8k) | n/a (16k > ctx 16384) | n/a | **3.37** (CPU-spill) | n/a | **FIXED** (cached ≈ 8,768) | 13203 MiB |

`*` findings-doc warm repeat baseline (~130 ms). `pp t/s` = `timings.prompt_per_second` (cold: `cache_prompt:false`). Decode/depth = `hack/llm-bench.sh` protocol (temp 0, seed 42, 3 repeats). Warm = 2-call `cache_prompt:true`, 2nd call.

## Slot-persistence verdict

**RESTORE BUG: FIXED BY PXA** on every tested qwen35moe-hybrid model (Qwen3.6 GGUF, Fusion4-PXQ2, Coder-PXQ4).

- Production (llama-p100, documented flaw reproduced): save 200 (790 ms, 145 MB) → erase → restore 200 (`n_restored: 7312`) → next identical request **re-evaluated all 7432 tokens in 69.7 s** (cached = 0; the ~44 s pathology).
- **PXA canary (Qwen3.6 GGUF):** save 200 (`n_saved: 8768`, ~327 MB ring) → **pod delete / process restart** → file persisted on the dedicated PVC → restore 200 (`n_restored: 8768`, 1.3 s) → next request evaluated only the **156-token delta in 3.07 s** (prompt_n = 156 vs 8768; cached prefix ≈ 8768 ≥ 8000). Turn-1 cold was 85.6 s for comparison.
- **Fusion4 PXQ2 canary:** identical procedure → restore 200 (`n_restored: 8768`) → **156 delta tokens / 1.55 s**.

The mechanism repair (`--kv-unified` + context checkpoints) makes the restored ring actually reusable across processes; the task's 500 ms prompt_ms bar was slightly undershot (1.6–3.1 s on first request after restore — restore-application overhead on the first call), but the failure mode (cached=0 / 44 s recompute) is definitively absent.

## Method

- All traffic direct to the cluster Service IP/URL from an in-cluster CNP-admitted bench client pod (`llmkube-system`); no ingress/agentgateway, no kubelet port-forward (that link flapped during the window).
- Timing-critical calls to the raw llama-server port; same protocol both engines.
- Deterministic: temperature 0, seed 42. Cells: cold 1k/8k/16k (`cache_prompt:false`, target-exact via `/tokenize`), warm (2-call), decode 512-token ×3 prompt classes, JSON tool-call cell (100% compliance 3⁄3 per engine/model).
- Raw per-run JSON: `hack/bench-logs/` (git-ignored). Rows: `hack/bench-results.csv` (canonical), `hack/bench-results-pxa-ab.csv`.
- Window executed git-driven via Flux (production + nomic scaled to 0, one canary active at a time; all toggles in PRs, reverted at close). Production manifest never otherwise modified.
- pxa canary A ran `ctx 20480` (its gallocr budget at 32768 = 16.5 GiB > card — OOM-laddered down) and `batch/ubatch 512` (the allocator's ~1.9 GiB scratch pool; parity with prod's 512-ubatch default). Documented deviations from the 32768-parity spec, forced by pxa's buffer policy on 16 GiB.

## Fidelity / output equivalence

- **Cross-engine byte-equality was NOT measured.** The window design keeps one engine resident at a time (exclusive 16 GiB card), and `hack/llm-bench.sh` records only summary timings, not response content. A paired temp-0 comparison would require both services up simultaneously (impossible on this single card) or a second window per prompt. Kernel differences alone (pxa tile-f16 flash-attention, differing accumulation order) mean exact byte equality between the engines is not expected.
- JSON tool-call schema compliance: 100% (3/3) on all three pxa canaries (Qwen3.6 GGUF, Fusion4-PXQ2, Coder-PXQ4). Production was not given the controlled tool-cell (the pxa-ab suite's JSON cell ran only on the canaries; production's `/v1/chat/completions` JSON path is exercised by the broker/agentgateway in normal service, not measured here).
- Per-engine determinism was spot-checked (sanity completions at temp 0), not a repeated-run byte-gate; pxa's own bit-exactness gates were not re-run here.

## Caveats

- pxa is built from source at rc3 (ghcr.io/poisonxa16/pxa not anonymously pullable); no PXQ re-quant of the production GGUF (the plain-file comparison is the fair engine test).
- `--kv-unified` documented semantics = a cross-slot attention-KV ring with context checkpoints (CHANGELOG v2026.08.31); whether it repairs slot-restore is exactly what the verdicts above measure — it does, empirically, on this arch family.
- pxa's own changelog notes KV q8_0 is a memory lever, not a speed lever (f16 preferred at depth); q8_0 kept for parity.
- **Fusion4/PXQ2 tier** is the model's "one 16 GB card" tier per its card; the PXQ4 tier does not fit.
- **Coder (PXQ4)** is 24 GB-class; on the P100 it ran partial offload (24/41 layers, ctx 16384) with CPU-spill decode (measured 3.37 t/s). Its cold-8k prefill was 126.9 pp-t/s; the 16k ladder cell is excluded (exceeds its ctx 16384 — pxa's gallocr reserves the full model+ring budget regardless of ngl, forcing the ctx trim to fit). The KV-restore test is unaffected (attention/KV on GPU).
- **Model-config deviations from production parity** (forced by pxa's allocator on 16 GiB): canary A ctx 20480 (vs 32768) + batch/ubatch 512 (the ~1.9 GiB pooled scratch); coder ctx 16384 + layers 24. All recorded; the summary matrix carries each measured config.
- MTP heads exist on the specialized models but are inert without `--spec-type` (out of scope).

## Recommendation

- **Do NOT swap the engine under the production GGUF**: pxa decodes the plain UD-Q3_K_S at 14.3 t/s vs llama-p100's 23.7 (−40 %), depth 11.6 vs 21.7. The slot-restore fix is real but not worth a 40 % decode regression on the current model.
- **Cherry-pick the slot-restore mechanism** (KV-unified ring + context-checkpoint restore) into the llama-p100 image if it ports — it closes the documented `n_prompt_tokens_cache = 0` flaw for the existing service.
- **PXA's PXQ-native models are the performance story on this card**: Fusion4-PXQ2 measured **40.8 t/s decode (1.7× production), depth 24.7, prefill 179–329 t/s** — with the restore fix included. If the fleet moves to a PXA/PXQ model line, pxa (digest-pinned) is the engine.
- The coder pass (below) adds the third, partial-offload data point.