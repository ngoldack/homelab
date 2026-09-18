# Image rescan (image-scan)

One CronJob, daily at 04:45 UTC, that enumerates every digest **actually
running** in the cluster and scans each unique digest with trivy, then pushes
the per-image CVE counts to VictoriaMetrics. Report-only by construction:
nothing is blocked, deleted or restarted on a finding — the scanner writes
metrics, the VMRule (`../monitoring/rules-image-scan.yaml`) raises alerts, a
human decides.

This is the "continuously rescan deployed digests" half of the supply-chain
phase. The build-time scan gate (trivy in the image-builds Jobs, coordinated
trivy pin) covers images when they are BUILT; this job covers the digests
that are RUNNING, including images that never passed through the build
pipeline at all (third-party DaemonSets, operators, helm-chart images).

## What it covers

- Every pod in every namespace, all three container status arrays
  (regular/init/ephemeral), any phase — so a digest running only inside a
  Job is scanned during the window it actually runs.
- The digest from `status.containerStatuses[].imageID`, not `spec.image`:
  the spec can name a tag that was re-pushed after the pod started; the
  imageID of a running pod cannot change under it. The scan therefore
  targets exactly the bytes in memory.
- Digests from any registry: first-party ones through
  `registry.ngoldack.de` (zot, anonymous pull) and third-party ones directly
  from their upstream registry (docker.io, ghcr.io, quay.io, nvcr.io,
  reg.kyverno.io, ...). Live count at implementation time: 14 unique
  running digests, 12 of them third-party.

## What it does NOT cover

- **Chart-internal images not currently running.** The scan enumerates what
  runs, not what is referenced in Git but scaled to zero, and image
  references inside vendored charts stay chart-managed per repo convention.
- **Multi-arch children.** Trivy scans the manifest referenced by the
  digest (the platform manifest the node pulled). Other-architecture
  variants of the same index are not scanned — no workload here runs them.
- **Base-image CVEs already fixed upstream.** Trivy reports vulnerabilities
  against the OS package inventory in the image; a CVE fixed in a newer
  base image still shows until the image is rebuilt. That is deliberate:
  the alert means "the running bytes contain it", regardless of whether a
  fix exists somewhere else.
- **Non-running attack surface**: images pulled but not currently running
  are invisible until a pod schedules them (the next daily run then sees
  them). This is the trade the design makes for scanning the truth of what
  executes rather than a Git-derived guess.
- **The registry itself as a trust root**: the scan trusts registry content
  the same way kubelet does (TLS only, no signature verification —
  admission-time cosign verification is the separate
  `verify-first-party-image-signatures` policy layer).

## Metrics

Pushed (not scraped — a daily Job leaves no target to scrape) to the
VictoriaMetrics import endpoint the restore drills also write to:

```
http://vmsingle-monitoring-victoria-metrics-k8s-stack.monitoring.svc.cluster.local.:8428/api/v1/import/prometheus
```

| metric | labels | meaning |
| --- | --- | --- |
| `image_scan_cve` | `image`, `digest`, `severity` | CVE count for one running digest; `severity` ∈ critical/high/medium/other; all four series always emitted (zero is a real 0) |
| `image_scan_run_timestamp_seconds` | — | completion time of the last run that reached the push; value is a Unix timestamp |
| `image_scan_images_enumerated` | — | unique digests found running this run |
| `image_scan_images_scanned` | — | digests scanned without a trivy/jq error |
| `image_scan_scan_errors_total` | `scan` | one series per digest whose scan failed (safe name of the ref) |

Reading an alert:

- `ImageScanCriticalCve` (critical): the latest scan of digest
  `{{ $labels.digest }}` found critical CVEs while the digest is running.
  Rebuild/bump the image so a new digest rolls out, or record the accepted
  risk in the policy exception register. The digest label is the rollback
  contract — anything else (tag, name) can change under you.
- `ImageScanStale` (warning): no run has pushed in > 26h (daily + 2h
  slack). A failed run pushes NOTHING — staleness is the failure signal, by
  design; there is no "success" metric to mask it.
- `ImageScanMetricsAbsent` (warning): no series at all — never ran, CronJob
  removed, or the metric names drifted from the table above. Also the
  schema canary for the first real run.

## Registry retention (zot)

The registry already runs blob-level garbage collection: the zot config
mounted by the helmrelease sets `storage.gc: true`, `gcDelay: "1h"`,
`gcInterval: "24h"`. Zot's documented default (v2.1.x, the deployed version
is v2.1.21): with no `retention` block, GC **deletes all untagged manifests
not referenced by indexes or artifacts** after the delay. Verified live on
2026-09-18: 21 GC log lines that day, including
`gc successfully completed for /var/lib/registry/llama-p100` — the registry
reaps untagged manifests on its own schedule, no config change was needed
and none was made.

**Accepted risk — why no `retention` policy was added.** A digest becomes
untagged as soon as a later build re-pushes the same tag (this happens in
normal operation: the guard image's tag was re-pushed twice on the day this
was written). A retention/GC policy that reaps untagged manifests can
therefore delete a digest a Deployment still pins by
`image@sha256:...`, surfacing as `ImagePullBackOff` on the next pod
restart. The mitigation is structural: digests are the contract (every
image reference in this repo pins a digest; a rebuild re-pins), and the
rescan acts as the safety net — see below.

**Manual GC trigger.** Not normally needed (`gcInterval: "24h"` runs by
itself); after a large cleanup or before debugging disk growth:

```sh
# watch GC work (log module "gc", per-repository)
kubectl -n registry logs sts/zot | grep '"module":"gc"'
```

Zot documents no single-shot GC API call; the supported procedure is the
config-driven scheduler above (`gcInterval: "0"` in the mounted config
would make GC run once at startup — a change to
`kubernetes/infrastructure/home/registry/helmrelease.yaml` + Flux
reconcile, i.e. a normal GitOps change, not a runtime poke).

**Safety net for a pinned-but-reaped digest.** The daily scan fetches every
running digest from its registry; if zot has GC'd a digest a Deployment
still pins, the scan's fetch of that digest fails and
`image_scan_scan_errors_total{scan="registry_ngoldack_de_..._sha256_..." }`
appears — before any pod restart has to hit `ImagePullBackOff`. Grep the
scan job log for `trivy failed on` to confirm which digest vanished.

## Runtime bounds

- `concurrencyPolicy: Forbid` + `startingDeadlineSeconds: 7200` — no
  overlapping runs, 2h grace after a missed fire.
- `activeDeadlineSeconds: 3600` — hard wall-clock stop; 14 digests at
  concurrency 4 and ~10 min per scan is ~35 min, so the deadline is ~4x
  measured-path headroom (first run re-measures).
- Per-image `--timeout 600s`; a hung registry costs one error line, not
  the run.
- Trivy cache is a fresh emptyDir each run: one DB download per day
  (~40-60 MiB) is the whole cost of keeping the job stateless.

## Least privilege

- RBAC (`rbac.yaml`): one ServiceAccount; ClusterRole grants only
  `get/list/watch` on `pods` (and nothing else). Cluster-scoped because
  the workloads whose images matter live in every namespace and cannot be
  enumerated from the repo without going stale. Kube-state-metrics was the
  zero-RBAC alternative and does not work here: this deployment exports no
  `image_id` label (verified live 2026-09-18 — zero series for
  `kube_pod_container_info{image_id!=""}` against 171 containers).
- No registry credentials mounted (the registry is anonymous-pull); the
  ServiceAccount token is the only credential, and it is read-only.
- Network (`cilium-default-deny.yaml` + `cilium-allowlist.yaml`):
  egress only to kube-apiserver, DNS, the vmsingle import endpoint, the
  registry via the Gateway path, and port-443 world (third-party digest
  fetches). No inbound except kubelet probes.

## Files

| file | purpose |
| --- | --- |
| `namespace.yaml` | PSA audit/warn restricted control namespace |
| `rbac.yaml` | SA + cluster-scoped read-only pods ClusterRole, justification inline |
| `cilium-default-deny.yaml` | deny-all baseline |
| `cilium-allowlist.yaml` | the four egress needs, each justified |
| `scan-config.yaml` | the script (one copy, `substitute: disabled`) |
| `cronjob.yaml` | the daily schedule and runtime bounds |

## First scheduled run: what must be confirmed

Everything below was verified as far as it could be without running the
scan; the first run proves or fails loudly on the rest:

1. **Metric families land with exactly the names in the table above** (the
   PromQL shapes were validated live against VictoriaMetrics; the names
   themselves are only provable with a run — `ImageScanMetricsAbsent`
   fires if they drift).
2. **zot serves the first-party digests to the scanner over the Gateway
   path** (`toEntities: ingress`, port 443, hostname
   `registry.ngoldack.de`) — verified reachable from a pod via that path,
   not yet from this namespace's pods (the CNP is new).
3. **trivy runs correctly as uid 65534 with `HOME=/tmp` and the cache
   emptyDir** (the staged-binary init container pattern is proven by the
   build pipeline's use of the same trivy version; the non-root
   invocation in THIS pod shape is not yet exercised).
4. **Scan wall-clock at the current fleet size** — the 4x-headroom estimate
   (deadline 3600s) needs the first run's `image_scan_images_scanned` +
   Job duration to become a measured number.
5. **The vmsingle import rule** (`monitoring-vmsingle-image-scan-import`,
   declared in this directory's cilium-allowlist.yaml but targeting the
   monitoring namespace — the drill precedent: the consumer's
   Kustomization owns the destination rule) actually admits the push —
   same cross-namespace-ingress caveat the drill metrics documented
   before their first run.
