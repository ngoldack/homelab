# vpn-egress — five ProtonVPN WireGuard egresses behind a Kubernetes load balancer, wired to synthetic-proxy

## Goal

Give the cluster a reusable egress pool: one WireGuard tunnel per ProtonVPN config
(PL / BE-Brussels / NL-Amsterdam / DE-Frankfurt / DE-Berlin), a load balancer that
routes through a random *healthy* tunnel, and `synthetic-proxy` re-pointed at it as the
first consumer — with a measurement that settles whether Synthetic's 429s have any
source-IP dimension at all.

Deliverable: `vpn-egress` namespace (5 Deployments + 1 Service + policies), Flux
Kustomization, the `synthetic-proxy` wiring, and the before/after 429 numbers.

---

## What I verified (facts this plan rests on)

| Fact | Evidence |
|---|---|
| `synthetic-proxy` dials the upstream with `Proxy: http.ProxyFromEnvironment` | `image-builds/synthetic-proxy/go/http.go:79` |
| …and with `ForceAttemptHTTP2: true`, `MaxIdleConnsPerHost: 16`, `IdleConnTimeout: 90s` | same file, lines 78-83 |
| → HTTPS_PROXY is honoured with **zero code change** | `http.Transport.Proxy` semantics |
| The eviction policy is `response.code >= 500`, `consecutiveFailures: 1`, `duration: 5m` | `agentgateway/policy-agent-chains-health.yaml`, `policy-hindsight-health.yaml` |
| WireGuard is built into the Talos kernel on every home node | `talosctl -n 10.30.0.{10,22,23} ls /sys/module` → `wireguard` present |
| Pod CIDR `172.20.0.0/16`, Service CIDR `172.21.0.0/16` | `hermes-egress/policy-configmap.yaml:32`, `media/vlan-routes-configmap.yaml:33` |
| `gluetun` v3.41.3 index digest | `sha256:fa19cc76b2af13d57a8d3dc3066f2ada061b1c761b8aecf989b3877c0486e027` (computed from the OCI index) |
| `HTTPPROXY` defaults to **empty/off** in gluetun v3.41.3 | image `ENV` block — must be set explicitly |
| `HTTPPROXY_LISTENING_ADDRESS` default `:8888`; `PUBLICIP_FILE=/tmp/gluetun/ip` | same `ENV` block |
| gluetun v3.41.3 ships `wget` + `iptables`, but **not** iproute2 (`ip`) | `Dockerfile` line 234-238 → no `ip rule` postStart hook |
| The 13 platform-namespace exclude lists + the header block it mirrors | `kyverno-policies/policies.yaml` lines 44-49 and 13 × 21-entry lists |
| The policy register is derived and must be regenerated | `docs/policy-exceptions.md:26-48` ("Derivation") |
| Key material lives on this workstation, one `PrivateKey` line per file | `~/Downloads/homelab-k8s-{pl--PL-129,be-brussels-BE-151,nl-amsterdam-NL-354,de-frankfurt-DE-477,de-berlin-DE-526}.conf` |

### The measurement that changed the design

I reproduced `synthetic-proxy`'s exact transport (same five fields) against a
CONNECT-counting proxy and an h2-capable TLS upstream
(`/tmp/vpnlb-measure/main.go`, output below). This decides whether a Kubernetes
Service can rotate at all:

```
ForceAttemptHTTP2=true    (shipped config, http.go today)
  serial   10 requests   -> 1 tunnel(s)   (resp: "ok HTTP/2.0")
  burst    20 concurrent -> 0 tunnel(s)
  burst#2  20 concurrent -> 0 tunnel(s)
  TOTAL distinct tunnels: 1

ForceAttemptHTTP2=false
  serial   10 requests   -> 1 tunnel(s)   (resp: "ok HTTP/1.1")
  burst    20 concurrent -> 19 tunnel(s)
  burst#2  20 concurrent -> 3 tunnel(s)
  TOTAL distinct tunnels: 23
```

**Consequence:** with the shipped config the proxy opens exactly **one** connection to
the upstream and multiplexes every request over it, so a ClusterIP Service picks one
random VPN for the **entire lifetime of the proxy process** — including under
concurrency. "Wire synthetic-proxy to the LB and change nothing" therefore produces
*no rotation at all*, which is not what was asked for.

Rotation necessarily requires a transport change, because the h2 connection is
end-to-end TLS inside the CONNECT tunnel: no hop we insert (Service, Envoy, chaining
proxy) can re-pick a tunnel for a connection the client keeps open. The three real
options, all code:

- **A. per-request rotation** — `DisableKeepAlives: true`. Every request dials the
  Service afresh → a new random ready endpoint → a new VPN exit. Literal
  "randomly route through those VPNs". Cost: one TCP+TLS handshake per request
  through the tunnel. **← recommended**, see D4.
- B. per-connection under load — `ForceAttemptHTTP2: false` only. Spreads only when
  concurrency > 1; steady-state stays pinned to one exit. (Measured above.)
- C. per-request by construction — rotate the proxy URL per request across 5 Service
  ports. More moving parts, needs its own cross-site failover story.

This supersedes the earlier "per-connection, no code change" answer, which was given
before this measurement existed.

---

## Architecture

```
hindsight / chat clients
        │
   agentgateway data plane
        │
   synthetic-proxy  (ns agentgateway)
        │   HTTPS_PROXY=http://vpn-egress.vpn-egress.svc.cluster.local:8888
        │   ← one CONNECT per request (keep-alives off), so the LB re-picks every time
        ▼
   Service vpn-egress   ClusterIP :8888, 5 endpoints, Ready-gated   ← THE LOAD BALANCER
        │   (Cilium picks a random *ready* endpoint per connection)
        ▼
 ┌──────────┬──────────┬──────────┬───────────┬───────────┐
 │ vpn-egress-pl │ -be │ -nl │ -de-fra │ -de-ber │   5 × gluetun v3.41.3
 └──────────┴──────────┴──────────┴───────────┴───────────┘
        │  WireGuard (kernel, UDP/51820)
        ▼
   ProtonVPN exits (5 different source IPs) ──► api.synthetic.new:443
```

Why this shape:

- **gluetun** is the maintained WireGuard client container: kernel or userspace
  WireGuard, kill-switch, and a **built-in HTTP CONNECT proxy** — the piece that makes
  the tunnel usable by an HTTP client behind a `Service`.
- **The Service is the load balancer.** Cilium's ClusterIP load balancing picks per
  connection, and — the property that matters — it only ever picks **Ready**
  endpoints, so a tunnel whose healthcheck fails silently drops out of the pool. No
  HAProxy/Envoy/Go router to own, no extra hop, no failover code.
- **The consumer needs no new logic**, only `HTTPS_PROXY` + the one-line transport
  change justified above.

---

## Decisions

- **D1 — One Deployment per VPN, replicas: 1.** Five Deployments named
  `vpn-egress-<site>` (repo `<app>-<component>` convention). A single pod with five
  `wg` interfaces and an `ip rule`/nftables rotator was rejected: it is a single point
  of failure for *all* Synthetic traffic, it needs its own in-repo image, and — because
  rotation is per flow — it is no better than the Service LB for a client that reuses
  connections.
- **D2 — `WIREGUARD_IMPLEMENTATION=kernelspace`.** `wireguard` is built into the Talos
  kernel on all three home nodes (verified), so no `/dev/net/tun`, no
  generic-device-plugin, no `hostPath`. Userspace would silently reintroduce a device
  dependency; set it explicitly so a node without the module fails loudly.
- **D3 — The load balancer is the ClusterIP Service.** Verified default is
  per-connection selection with readiness gating. (Confirm the LB algorithm is not
  maglev: `kubectl -n kube-system get cm cilium-config -o
  jsonpath='{.data.bpf-lb-algorithm}'`. Maglev hashes the 5-tuple, which is still
  per-connection here because keep-alives are off, so the design holds either way.)
- **D4 — Rotation granularity: per request** (`DisableKeepAlives: true` in `http.go`).
  This is the only wiring that makes "randomly route through those VPNs" true, and it
  is what makes the 429 hypothesis falsifiable: requests genuinely arrive from five
  different source IPs, so *if* Synthetic's limit has any IP dimension the rate must
  move. Option B is the smaller change but leaves steady-state traffic pinned to one
  exit, which confounds the very measurement we are paying for.
- **D5 — Key material: one sops Secret, five keys.** `vpn-egress-wireguard` with
  `stringData: {pl, be, nl, de-fra, de-ber}`. **The plan and the commit never contain a
  private key**: the values come from `~/Downloads/homelab-k8s-*.conf` on this
  workstation, through the repo's `.local.yaml` → `task sops:encrypt` flow, and are
  never echoed into a terminal, a log, or this document.
- **D6 — No authentication on the HTTP proxy.** The control is the Cilium allowlist
  (namespace-scoped, port-scoped). gluetun basic-auth would put a second shared
  credential in five places and add a failure mode that takes the entire Synthetic leg
  down on any mismatch, for a benefit the CNI already provides.
- **D7 — Namespace PSA `enforce: privileged`** (audit/warn stay `baseline`), exactly
  like `media`: `NET_ADMIN` is not permitted at baseline.
- **D8 — No read-only rootfs, no seccomp override on the gluetun container.** gluetun
  rewrites `/etc/resolv.conf` and installs its kill-switch rules at start; it runs as
  root with `capabilities: {add: [NET_ADMIN], drop: [ALL]}` and
  `allowPrivilegeEscalation: false`. If `RuntimeDefault` seccomp ever blocks a netlink
  call, the fallback is `seccompProfile: Unconfined` + a comment — not a guess made now.
- **D9 — No `ip rule` postStart hook.** It is the one gluetun-on-Kubernetes gotcha that
  does not apply here (the image has no iproute2; verified), and a failing `postStart`
  restarts the container.
- **D10 — `BLOCK_MALICIOUS=off`.** Upstream default is `on`, which downloads public
  DNSBL blocklists through the tunnel at every start. This pod resolves exactly one
  hostname; the fetch is a startup dependency with no upside here. Recorded as a
  deliberate posture deviation, reversible with one env var.
- **D11 — No `dependsOn` from the `agentgateway` Kustomization to `vpn-egress`.**
  A hard edge would let a broken VPN lane block the whole LLM lane's reconciliation
  (and, because the runner gates on every Kustomization being Ready, the repo's
  post-merge workflow). The coupling is handled by ordering instead: Phase 1 merges and
  is verified live *before* Phase 2 flips the consumer (see D12).
- **D12 — Two phases, two PRs.** Phase 1 is the enable-but-unused lane. Phase 2 wires
  the consumer. Splitting is not ceremony: the eviction policy is
  `consecutiveFailures: 1` / `duration: 5m`, so *any* 503 during a single-commit
  roll-out (the window before the VPN pods are Ready) buys a guaranteed 5-minute
  OpenRouter failover. Landing the lane first removes the window entirely.

---

## Files

**New — `kubernetes/infrastructure/home/vpn-egress/`**

| File | Content |
|---|---|
| `namespace.yaml` | `Namespace vpn-egress`, PSA labels `enforce: privileged`, `audit/warn: baseline`. No `gateway.ngoldack.de/*-ingress` label (nothing external) |
| `cilium-default-deny.yaml` | `CiliumNetworkPolicy` `default-deny` with empty `endpointSelector`, no ingress/egress (copy `media/cilium-default-deny.yaml`) |
| `cilium-allowlist.yaml` | See "Policies" below |
| `serviceaccount.yaml` | `ServiceAccount vpn-egress`, `automountServiceAccountToken: false` |
| `secret.sops.yaml` | `Secret vpn-egress-wireguard`, five keys, SOPS-encrypted; `secret.local.yaml` working copy is git-ignored |
| `deployments.yaml` | The 5 Deployments (same file: one shared shape, tabulated per site) |
| `service.yaml` | ClusterIP `vpn-egress`, port 8888 → `http-proxy` |
| `kustomization.yaml` | `resources:` list, alphabetical, all eight entries |

**New — Flux**: `kubernetes/clusters/home/vpn-egress.yaml`; one line added to
`kubernetes/clusters/home/kustomization.yaml`.

**Changed — Phase 1**: `kubernetes/infrastructure/home/kyverno-policies/policies.yaml`
(header comment + 13 exclude lists), `docs/policy-exceptions.md` (Legend A + two reason
rows).

**Changed — Phase 2**: `kubernetes/infrastructure/home/image-builds/synthetic-proxy/{synthetic-proxy.yaml,go/http.go}`,
`kubernetes/infrastructure/home/agentgateway/{synthetic-proxy.yaml,cilium-allowlist.yaml}`.

---

## Phase 1 — the egress lane (no consumer)

### 1.1 Secret (SOPS)

Create `vpn-egress/secret.local.yaml` (git-ignored) shaped like
`agentgateway/synthetic-key.sops.yaml`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vpn-egress-wireguard
  namespace: vpn-egress
type: Opaque
stringData:
  pl: <PrivateKey from ~/Downloads/homelab-k8s-pl--PL-129.conf>
  be: <PrivateKey from ~/Downloads/homelab-k8s-be-brussels-BE-151.conf>
  nl: <PrivateKey from ~/Downloads/homelab-k8s-nl-amsterdam-NL-354.conf>
  de-fra: <PrivateKey from ~/Downloads/homelab-k8s-de-frankfurt-DE-477.conf>
  de-ber: <PrivateKey from ~/Downloads/homelab-k8s-de-berlin-DE-526.conf>
```

Extract the values with a command that never prints them, e.g.
`awk '/^PrivateKey/{print $3}' <file>` piped straight into the editor/`sed`, then
encrypt with the repo flow (`task sops:encrypt SOURCE=<dir>/secret.local.yaml
FILE=<dir>/secret.sops.yaml`), delete the local copy, and confirm
`grep -c 'PrivateKey = ' ` produced nothing in any tracked file.

### 1.2 The five Deployments

Identical shape; only the four per-site values differ (endpoint IP and peer public key
are not secret — they are in the configs):

| Deployment | Secret key | `WIREGUARD_ENDPOINT_IP` | `WIREGUARD_PUBLIC_KEY` |
|---|---|---|---|
| `vpn-egress-pl` | `pl` | `79.127.186.251` | `/Ce+DOB9gzY8TFSqRwu0vwKW2+blWftPnrLesBW92T8=` |
| `vpn-egress-be` | `be` | `79.127.164.95` | `WVoDDXmBy6pDq+r9HB+f+Ki7TP8jv/aeo5HTOdKlt3c=` |
| `vpn-egress-nl` | `nl` | `62.112.9.164` | `127jo9F8kpNz/SQfhY2o5I8HB7X0VLMJVSaMGGuJowQ=` |
| `vpn-egress-de-fra` | `de-fra` | `194.126.177.13` | `XcWEb0DMaFBex2HD2DVUStifh6wBZe9ELo2N/KLlMHc=` |
| `vpn-egress-de-ber` | `de-ber` | `89.36.76.130` | `9xUSjs4KYUv0ySbhrYjwN/49TpHfmIcI/2KdGkOEGz0=` |

Pod spec, once:

- `serviceAccountName: vpn-egress`, `automountServiceAccountToken: false`
- `nodeSelector: {topology.homelab/site: home}`
- `strategy: {type: RollingUpdate, rollingUpdate: {maxSurge: 0, maxUnavailable: 1}}` (repo convention; also avoids two tunnels racing for the same proton session)
- soft spread: `topologySpreadConstraints: [{maxSkew: 1, topologyKey: kubernetes.io/hostname, whenUnsatisfiable: ScheduleAnyway, labelSelector: app.kubernetes.io/name=vpn-egress}]`
- container `gluetun`, `image: ghcr.io/qdm12/gluetun:v3.41.3@sha256:fa19cc76b2af13d57a8d3dc3066f2ada061b1c761b8aecf989b3877c0486e027`
- resources `requests: {cpu: 50m, memory: 64Mi}`, `limits: {memory: 256Mi}`
- `readinessProbe`/`livenessProbe`: `exec: ["/gluetun-entrypoint", "healthcheck"]`
  (readiness: `initialDelaySeconds: 15, periodSeconds: 10, failureThreshold: 6`;
  liveness: `initialDelaySeconds: 60, periodSeconds: 30, failureThreshold: 6`).
  Generous on purpose — a wobbly liveness would restart-loop the tunnel, and the
  readiness probe is the *only* thing that removes a dead exit from the LB.

Env (per site):

```yaml
# custom provider: the WireGuard values come from the Proton config verbatim
- {name: VPN_SERVICE_PROVIDER, value: custom}
- {name: VPN_TYPE, value: wireguard}
- name: WIREGUARD_PRIVATE_KEY
  valueFrom: {secretKeyRef: {name: vpn-egress-wireguard, key: <site>}}
- {name: WIREGUARD_ADDRESSES, value: "10.2.0.2/32"}      # IPv6 deliberately dropped
- {name: WIREGUARD_ALLOWED_IPS, value: "0.0.0.0/0"}       # ::/0 deliberately dropped
- {name: WIREGUARD_PUBLIC_KEY, value: "<per-site peer key>"}
- {name: WIREGUARD_ENDPOINT_IP, value: "<per-site IP>"}
- {name: WIREGUARD_ENDPOINT_PORT, value: "51820"}
- {name: WIREGUARD_PERSISTENT_KEEPALIVE_INTERVAL, value: 25s}
- {name: WIREGUARD_IMPLEMENTATION, value: kernelspace}    # D2
# the piece that makes the tunnel usable by an HTTP client
- {name: HTTPPROXY, value: "on"}                          # upstream default is OFF
- {name: HTTPPROXY_LISTENING_ADDRESS, value: ":8888"}
- {name: HTTPPROXY_LOG, value: "on"}                      # one line per CONNECT: the evidence trail
# kill-switch exceptions: cluster traffic must never enter the tunnel
- {name: FIREWALL_INPUT_PORTS, value: "8888"}
- {name: FIREWALL_OUTBOUND_SUBNETS, value: "172.20.0.0/16,172.21.0.0/16"}
- {name: BLOCK_MALICIOUS, value: "off"}                   # D10
- {name: LOG_LEVEL, value: info}
```

Notes that matter: `WIREGUARD_MTU` stays at the upstream default `1320`; `PUBLICIP_ENABLED`
stays `on` (it writes the observed exit IP to `/tmp/gluetun/ip`, which is step 1.6's
cheapest assertion); gluetun's own resolver (DoT through the tunnel) handles all name
resolution, so nothing depends on the cluster DNS.

### 1.3 Service (the load balancer)

```yaml
apiVersion: v1
kind: Service
metadata: {name: vpn-egress, namespace: vpn-egress, labels: {app.kubernetes.io/name: vpn-egress}}
spec:
  type: ClusterIP
  selector: {app.kubernetes.io/name: vpn-egress}
  ports: [{name: http-proxy, port: 8888, targetPort: 8888, protocol: TCP}]
```

### 1.4 Policies

`cilium-allowlist.yaml` — two halves, both needed:

- **ingress** (on 8888): `fromEndpoints: [{k8s:io.kubernetes.pod.namespace: agentgateway}]`.
  Comment that future workloads add their namespace to this list — that list *is* the
  access control for a general-purpose egress pool.
- **ingress**: `fromEntities: [health]` (harmless with exec probes; correct the moment
  anyone switches to an HTTP probe).
- **egress**: `toCIDRSet` the five endpoint `/32`s on UDP 51820, plus cluster DNS
  (`kube-dns`, UDP+TCP 53). Everything else the pod sends is inside the tunnel and
  therefore invisible to Cilium; nothing else is needed. Comment that the five CIDRs
  must be updated together with the configs if a Proton endpoint moves.

### 1.5 Kyverno + policy register

- `kyverno-policies/policies.yaml`: add `- vpn-egress` after `- media` in **all 13**
  exclude lists, and add it to the header comment block at lines 44-49 (which is
  documented as mirroring the lists).
- `docs/policy-exceptions.md` (derived — the doc requires regeneration in the same
  change): add `vpn-egress` to Legend A's name list and to the code block, bump "The
  21 namespaces" to 22, and extend the two reason rows that justify `NET_ADMIN`
  (`disallow-privileged-containers`, `require-drop-all-capabilities`) to read
  `media/vpn-egress`.

Nothing else in the repo enumerates platform namespaces (checked:
`security-baseline` does not).

### 1.6 Flux

`kubernetes/clusters/home/vpn-egress.yaml` — same shape as `media.yaml`:
`interval: 10m`, `path: ./kubernetes/infrastructure/home/vpn-egress`, `prune: true`,
**`wait: false`**, `timeout: 15m`, `retryInterval: 2m`, `sourceRef` flux-system,
`decryption` sops/sops-age, `postBuild.substituteFrom` cluster-vars, **no `dependsOn`**.

`wait: false` is deliberate (D11): with `wait: true` + healthChecks, one unreachable
Proton endpoint would hold this Kustomization NotReady forever, and
`hack/flux-wait-kustomizations.sh` gates the repo's post-merge workflow on *every*
Kustomization's Ready condition — a single flaky VPN would fail every future merge.
Health signal comes from monitoring instead.

Add `- vpn-egress.yaml` to `kubernetes/clusters/home/kustomization.yaml` with a
two-line comment, next to the other app lanes.

### 1.7 Verification (live, Phase 1)

```bash
export KUBECONFIG=$PWD/kubeconfig-home.yaml
kubectl -n vpn-egress get deploy,po,svc -o wide
```

1. **All five Ready** (this is the readiness gate the LB depends on).
2. **Five distinct exit IPs, none of them the homelab WAN IP** —
   `kubectl -n vpn-egress exec deploy/vpn-egress-<site> -- cat /tmp/gluetun/ip`, once
   per site; sanity-check that each geolocates to the expected country.
3. **The LB actually spreads**: from a scratch pod in `agentgateway`
   (`curlimages/curl:8.11.1@sha256:c1fe1679c34d9784c1b0d1e5f62ac0a79fca01fb6377cdd33e90473c6f9f9a69`,
   the pin already used by `hack/hermes-egress-e2e.sh`), loop 20 requests through
   `-x http://vpn-egress.vpn-egress.svc.cluster.local:8888 https://api.ipify.org` and
   confirm the returned set of IPs is a subset of (2) with **more than one** distinct
   value. Cross-check the per-site `HTTPPROXY_LOG` counts.
4. **End-to-end to Synthetic without touching the key or the quota**:
   `curl -s -o /dev/null -w '%{http_code}' -x http://vpn-egress...:8888 https://api.synthetic.new/v1/models`
   → expect **401** (proves tunnel → DNS → TLS → HTTP reach to the real upstream; an
   unauthenticated probe consumes no quota and cannot extend the proxy's hold-off
   window). Never probe with the API key from here — `synthetic-proxy` is deliberately
   the only component that pokes Synthetic.
5. `kubectl -n vpn-egress get ciliumnetworkpolicy` + `kubectl get policyreport -n
   vpn-egress` → no violations outside the expected privileged-namespace ones.

---

## Phase 2 — wire synthetic-proxy (second PR)

### 2.1 The transport change (D4)

`image-builds/synthetic-proxy/go/http.go`, in the transport literal:

```go
// Rotation requires breaking connection reuse: the upstream connection is
// end-to-end TLS *inside* the CONNECT tunnel, so as long as one connection
// stays open (and with ForceAttemptHTTP2 it is multiplexed into exactly one)
// the load balancer's per-connection pick can never re-choose a VPN.
// Measured 2026-09-21 with this exact transport: h2 -> 1 tunnel for a
// 20-request burst (0 new), keep-alives off -> a fresh tunnel per request,
// i.e. one random egress per request. See docs/vpn-egress.md.
DisableKeepAlives: true,
```

Then the standard in-repo build dance (`hack/` + `image-builds/synthetic-proxy.yaml`):
bump the Job's revision/name suffix → apply → read `status.imageDigest` → pin the new
digest into `agentgateway/synthetic-proxy.yaml` (replacing
`registry.ngoldack.de/synthetic-proxy@sha256:70adc920b7d1db35a337aed96d62e2901418bad9f21cc8d5f0e36b03769243e6`,
which is the rollback target) → merge. No lint/formatter runs beyond the repo's own
`task check` at the end.

### 2.2 The env

`agentgateway/synthetic-proxy.yaml`, `env:` block:

```yaml
# Route the Synthetic leg through the vpn-egress pool: the Service is the load
# balancer, and with keep-alives off each request picks a random ready tunnel.
- name: HTTPS_PROXY
  value: http://vpn-egress.vpn-egress.svc.cluster.local:8888
# Keep in-cluster upstreams (and the pod's own health surface) off the tunnel.
- name: NO_PROXY
  value: localhost,127.0.0.1,.svc,.svc.cluster.local
```

One gotcha to honour: `http.ProxyFromEnvironment` reads and caches the environment at
**first use**, so the value must be present in the container env (it is) — do not
expect a live `kubectl set env` to be picked up by the running process.

### 2.3 The permission half

`agentgateway/cilium-allowlist.yaml`, egress: add a rule allowing this namespace to
reach `vpn-egress` on 8888. (The `world:443` rule stays — the gateway data plane and
other pods in the namespace still need it.) The matching ingress half is already in
1.4, so both sides of the pair exist; without both, Cilium silently drops and the
failure looks like a broken VPN.

### 2.4 Verification (live, Phase 2)

1. `kubectl -n agentgateway get pod -l app.kubernetes.io/name=synthetic-proxy -o
   jsonpath='{.items[0].spec.containers[0].env[?(@.name=="HTTPS_PROXY")].value}'` and
   `kubectl exec` a running request path to prove traffic flows.
2. **Prove the traffic really traverses a VPN**: drive a request through the gateway
   into a Synthetic-backed chain and confirm (a) the caller gets a normal response, and
   (b) the matching `CONNECT api.synthetic.new:443` line appears in a
   `vpn-egress-*` pod's log — that log line is the end-to-end proof that the request
   left through Proton and not through the homelab WAN.
3. **Measure the spread the consumer actually gets**: count established sockets on
   `:8888` per pod (`kubectl -n vpn-egress exec <pod> -- cat /proc/net/tcp`) while a
   burst is in flight through the gateway. With D4, expect one connection per in-flight
   request, distributed over the ready exits — not all on one pod.
4. **Regression check**: the Synthetic leg still serves 200s (not 503 → eviction), and
   `synthetic_proxy_upstream_429_total` is still being recorded.

---

## Phase 3 — answer the actual question

The hypothesis is that the 429 window is partly source-IP-driven. The honest prior is
against it: Synthetic's three flavours, as the proxy's own code and comment explain,
are **per key** (parallel-limit concurrency, generic rate-limit, quota) — and this
egress rotation changes the IP, not the key. So the plan's deliverable is the
*measurement*, not a claim:

- Record `synthetic_proxy_upstream_429_total` (and the hold-off verdicts) for a
  comparable window before and after Phase 2, same workload (hindsight ingestion plus
  the usual chat traffic).
- If the 429 rate moves materially → the limit has an IP dimension, and the pool is the
  fix (then consider widening to more configs).
- If it does not move → the limit is key-scoped as documented, the rotation is still the
  right thing to have (it removes IP-level shaping and is reusable egress for future
  workloads), but the 429 problem must be solved elsewhere (quota/plan, or pulling the
  Synthetic share of traffic down).
- Also record the latency delta from D4 (one more TCP+TLS handshake per request, through
  a VPN exit), because that is the cost being paid for the rotation.

---

## Risks

| Risk | Handling |
|---|---|
| **Multi-IP on one account may trip Synthetic's abuse detection.** Five simultaneous source IPs sharing one key is exactly the pattern abuse heuristics look for | Called out explicitly; the rotation is reversible with one env var, and Phase 3's numbers set the retention decision |
| Added per-request latency (D4) | Measured in Phase 3; the fallback is option B (`ForceAttemptHTTP2: false`, no keep-alive change) which trades rotation for latency |
| Proton may cap concurrent sessions per account | Five configs = five sessions; check the plan's device limit (Proton allows 10), and note that a sixth workload config already exists for media (`homelab-k8s-media-vpn-IS-19.conf`) |
| One VPN dies (key revoked, endpoint moved) | Readiness removes it from the Service; the other four keep serving. This is *why* the Service is the LB rather than a bare pod |
| `kernelspace` unavailable on a future node | Explicit `WIREGUARD_IMPLEMENTATION=kernelspace` fails loudly instead of silently reverting to userspace and needing `/dev/net/tun` |
| Cilium drops the WireGuard handshake | Symptom is gluetun's startup check failing with no obvious cause; the CNP egress rule lists the exact UDP/51820 CIDRs, and `cilium monitor`/hubble is the first stop |
| NAT/endpoint: the endpoint traffic must not be routed into the tunnel | gluetun handles this (endpoint route exception); if it ever regresses the tunnel simply never comes up — no leak |

**Rollback.** Phase 2 only: remove `HTTPS_PROXY`/`NO_PROXY` and the agentgateway
cilium egress rule, and repin the previous image digest
(`registry.ngoldack.de/synthetic-proxy@sha256:70adc920b7d1db35a337aed96d62e2901418bad9f21cc8d5f0e36b03769243e6`).
Phase 1 is inert until Phase 2 exists; deleting the Flux entry + `git revert` leaves no
trace.

---

## Non-goals

- No IPv6 in the tunnels (both IPv6 fields from the Proton configs are dropped on
  purpose so there is no v6 leak path to reason about).
- No port forwarding / no inbound over the VPN (`VPN_PORT_FORWARDING` stays off).
- No second consumer yet — the allowlist is written so adding one is a one-line change,
  but `synthetic-proxy` is the only client in this change.
- No docs page beyond what is required: `docs/policy-exceptions.md` regeneration (D5's
  repo rule) — a runbook page only if the Phase 3 numbers justify keeping the pool.
