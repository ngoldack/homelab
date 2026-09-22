# Workload placement verdicts (home cluster)

Derived from the fit analysis (6 scouts) plus live verification on 2026-09-22. This is
the record behind the placement default: *everything lands on the efficiency worker,
the performance worker is opt-in.* Volatile live numbers (pod counts, memory) are
recorded where they are evidence; the **labels and selectors are the durable part**.

## The two home workers

Roles are **inverted** from what the historical `instance-type` label suggests, which
is the single most important fact in this document:

| node | role | instance-type (stale) | sandbox | iGPU | live proof |
| --- | --- | --- | --- | --- | --- |
| `talos-3bi-1fl` | **efficiency** | `worker` | — | `Intel-UHD-Graphics-770` | 99 pods, **81% memory** (the loaded node) |
| `talos-919-w9u` | **performance** | `gpu-worker` | `true` | — | 76 pods, **33% memory** (the idle node) |

The `instance-type=gpu-worker` value on `talos-919-w9u` is historical (the P100 is
gone) and is kept only because BuildKit selects on it. Reading it as "the GPU node,
therefore the fast one" is what made the old assumption wrong.

**The efficiency worker is the busy one** (81% memory) and the performance worker is
idle (33%). That asymmetry is why the default is a *preference* and not a pin: a
required affinity for efficiency would simply fail to fit, and anything that cannot
overflow onto performance is stranded.

## Why "move the performance-critical things to performance" is mostly already done

The namespaces are **already split across both workers** by the scheduler, and there is
no concentration to fix. Observed pod distribution in the app namespaces:

| namespace | efficiency (`3bi`) | performance (`919`) |
| --- | --- | --- |
| monitoring | 11 | 4 |
| langfuse | 10 | 8 |
| buildkit | 5 | 10 |
| authentik | 5 | 11 |
| hermes / hermes-sandbox | — | 5 / 1 |
| hindsight | 4 | 3 |
| media | 8 | 4 |

This spread *is* the problem the default solves: no manifest expresses a preference,
so scoring has been deciding. The class label + policy replace that with an explicit,
reviewable default.

## Per-workload verdicts

| workload | lands on | mechanism | why |
| --- | --- | --- | --- |
| kata sandboxes (`hermes-sandbox`) | performance | hard nodeSelector `workload.hermes.io/sandbox=true` (carried by the RuntimeClass itself) | needs nested KVM; cannot go elsewhere |
| BuildKit builder | performance | hard nodeSelector `instance-type=gpu-worker` | rootless builder needs the `user.max_user_namespaces` sysctl patch keyed on that node |
| `media-jellyfin` | performance | `workload/media=true` (moves with the iGPU) | the only pod in the cluster that mounts `/dev/dri` |
| `immich-machine-learning` | performance | derived `hardware/igpu=Intel-UHD-Graphics-770` | follows the card automatically; no manifest edit needed when the iGPU moves |
| CNPG instances on `nvmeof` classes | wherever the volume bound them | `volumeBindingMode: WaitForFirstConsumer` | the volume decides and the pod follows; not overridable by affinity without Multi-Attach |
| `authentik-server` (2 replicas) | one per home worker | **required** `podAntiAffinity` on hostname | deliberate one-server-per-node so a node loss leaves a serving replica; must never be clobbered |
| hindsight workers | performance (user-named opt-in) | its own `podAntiAffinity` spread + the class opt-in | the user's named "performance critical stuff"; do **not** default it to efficiency (workers are ~5.5 GiB RSS each, and efficiency is at 81%) |
| everything else | **efficiency** | the policy's non-binding weight-100 preference | the default; overflows to performance when it does not fit |

## Deliberately excluded

- **DaemonSets** — CNI, CSI, security (tetragon/crowdsec), metrics and the gateway data
  plane must run on **both** workers. A class preference on them would degrade
  performance-node coverage rather than move a workload. Out of the policy's scope.
- **Platform namespaces** — the same exclusion list the sibling validate policies use.
- **Node pinning via `cpu_affinity`** — that is a Proxmox-level core pin, unrelated to
  Kubernetes scheduling; it does not interact with the class label.

## Verification note

Every claim about the Kyverno policy's behaviour in this document was measured with the
cluster's Kyverno CLI (v1.19.1) against fixtures, not inferred — including the two
silently-broken formulations that were rejected (`patchesJsonPatch` is not a CRD field;
autogen excludes JSON-patch mutates matching on Pod). See
`docs/overview.md` § "Workload placement policy" for the mechanism and
`kubernetes/infrastructure/home/kyverno-policies/placement-default.yaml` for the
annotated policy.
