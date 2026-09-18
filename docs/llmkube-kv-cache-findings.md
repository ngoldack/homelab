# LLMKube KV-cache persistence findings (P100, llama.cpp v0.4.0)

Empirical results from the live `qwen36-35b` InferenceService on the Tesla P100
(`registry.ngoldack.de/llama-p100:0.4.0-p100`, llama.cpp v0.4.0, `--parallel 1`,
`cacheTypeK/V: q8_0`, context 32768), measured during the kv-broker experiment
(2026-09). The broker implementation lives in
`kubernetes/infrastructure/home/image-builds/llama-kv-broker/`; its deployment in
`kubernetes/infrastructure/home/llmkube-models/kv-broker.yaml`.

## TL;DR

Slot **save** works. Slot **restore** on this build is a destructive no-op: it
returns HTTP 200 and reports `n_restored: 1`, but the server discards the cache
and re-prefills from scratch — and after a server restart, an attempted restore
makes the *second* identical request dramatically slower. The broker therefore
ships restore **disabled by default** (`KV_RESTORE_MODE=off`) while keeping the
save/plumbing for a future engine fix.

## Measurements

| Scenario | Result |
| --- | --- |
| Warm repeat (same prompt, cache hit via `cache_prompt: true`) | ~130 ms TTFT-equivalent prefill |
| Restore from slot file, warm pod | HTTP 200, `n_restored: 1`, but subsequent request reports `n_prompt_tokens_cache = 0` — full re-prefill, no benefit |
| After server restart: request 1 (restore attempted) | appears fine |
| After server restart: request 2 (identical prompt) | prefill balloons to ~44 s (vs 130 ms warm without restore) |
| Save after each response | ~0.4–0.5 s mutex held post-EOF; 86–124 MB slot file per long session |

The 44 s post-restart pathology is why restore is not merely "useless" but
*harmful* on this build: the restored layout poisons the cache state until the
next full recompute.

## Root-cause hypothesis (unverified)

v0.4.0's `--slot-save-path` restore path likely mismatches the KV layout for
hybrid-attention models (Qwen3.5-style: only every 4th layer is full attention,
SWA layers keep a fixed window). The save/restore byte format was written for
the classic uniform-KV layout, so restoring produces an internally inconsistent
cache that must be recomputed. The llama.cpp discussion proposing
`--kv-unified` / context checkpoints suggests the mechanism may be repaired in a
later build; the broker keeps the code path behind a flag so re-testing is an
env flip, not a code change.

## Shipped state (build :4, `kv_broker_build_info{version="4"}`)

- `KV_RESTORE_MODE=off` (default): no `/slots` probe, no restore, no erase.
  Saves still run each request (cheap, keeps files for future experiments).
- `KV_RESTORE_MODE=auto`: restore only after a detected server restart
  (`id_task` decrease); the previously tested behavior, now the non-default.
- `KV_RESTORE_MODE=always`: stat-restore-if-exists on every request.
- Metrics on `/metrics` (`/metrics` added in build 3): requests, inflight,
  restore/save totals+latency histograms, slot file counts/bytes, sweeper
  deletions, upstream errors, server restarts.
- Restore decision + failure path (`restore_total{result=ok|error|skipped_no_file|skipped_warm}`)
  are unit-tested; `off` pins zero probe/restore traffic
  (`TestRestoreModeOffSkipsProbeAndRestore`).

## Validation checklist (before re-enabling restore)

1. Run the save/restore round-trip directly against the server pod, then issue
   the same completion twice and confirm the second reports
   `n_prompt_tokens_cache > 0`.
2. If it still reports 0, test a llama.cpp build with `--kv-unified` or the
   context-checkpoint mechanism before spending more time on slot files.
3. Only flip `KV_RESTORE_MODE` to `auto`/`always` in `kv-broker.yaml` after a
   passing round-trip on the *pinned image digest*.

Treat slot files strictly as a disposable cache: conversation history and
Hindsight memory remain authoritative.
