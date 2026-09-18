# Hermes Agent + Kubernetes Agent Sandbox

Production-minded Hermes Agent deployment: every model-generated terminal
command executes inside a Kubernetes Agent Sandbox Pod (kubernetes-sigs/
agent-sandbox v1.0.2, SIG Apps) on the shared Kata-capable worker
`wk-main-performance` (16 vCPU / 48 GiB, nested virt,
`siderolabs/kata-containers` extension, label-pinned via
`workload.hermes.io/sandbox=true`; mixed use — GPU inference lands there
too).

## Architecture

```mermaid
graph LR
    C[Client] -->|API_SERVER_KEY bearer| A[hermes:8642<br/>StatefulSet]
    B[Browser] -->|https://hermes.ngoldack.de<br/>authentik SSO (dashboard OIDC)| G[edge Gateway]
    G -->|:9119| A
    D[Hermes Desktop] -->|kubectl port-forward :9119<br/>basic auth| A
    A -->|gRPC podIP:9090<br/>direct exec| S[Sandboxd in Kata guest]
    A -->|v2 scoped token<br/>GET /v1/files| R[Router ClusterIP:8080]
    A -->|trace export :3000<br/>observability/langfuse| L[langfuse-web<br/>ns langfuse]
    R -->|proxy :8080| S
    A -.SandboxClaim CRUD via SA token.- K[kube-apiserver]
    K --> W[agent-sandbox controller]
    W --> S
```

Layers (Flux dependencies in this order):
`agent-sandbox` (controller + Router + admission + policies) →
`hermes-sandbox` (namespace, RuntimeClass `kata`, `hermes-go`
template/warm pool, Cilium) → `hermes` (gateway). The `hermes-sandbox`
Kustomization carries `healthCheckExprs` on `SandboxWarmPool`
(`status.readyReplicas == spec.replicas`), so a half-provisioned warm pool
blocks the gateway instead of reporting Ready: the SandboxWarmPool CRD has
no `conditions`, and without the CEL expression kstatus cannot see an
unready pool.

### Trust model and the documented design decisions
- **Claim RBAC (user-approved):** the sandbox-router (Go, v1.0.2) proxies to
  already-adopted sandboxes and has no claim API, so the gateway's SDK
  creates/deletes its own SandboxClaims via the Kubernetes API. The `hermes`
  SA gets a Role in `hermes-sandbox` ONLY: `sandboxclaims`
  (create/get/list/watch/delete) + `sandboxes` (get). No pods, no secrets,
  no exec/port-forward, no cluster scope. Owned claims carry the label
  `agent-sandbox.hermes/owner=<AGENT_SANDBOX_GATEWAY_ID>` (`hermes` in the
  StatefulSet; pod hostname when unset); the plugin sweeps orphaned claims
  once at provider startup and retries failed deletions.
- **Direct gRPC exec (user-approved):** v1.0.2 sandboxd exposes execution
  only as gRPC ProcessService on podIP:9090 (REST is `/v1/files`,
  `/v1/health`, `/v1/metadata`); the Go Router is HTTP-only, so Router
  exec does not exist. The plugin executes via plaintext gRPC to the
  adopted pod. Cilium admits pod→sandbox 9090 only from the `hermes`
  namespace, and Router→sandbox 8080 only from the Router's own pod labels;
  claim ownership is the trust boundary.
- **Admission + runtime:** the `secure-hermes-sandbox`
  ValidatingAdmissionPolicy (binding `validationActions: [Deny]`,
  `failurePolicy: Fail`) covers `pods` CREATE/UPDATE **and**
  `pods/ephemeralcontainers` UPDATE, enforcing the Kata-only, credential-less
  sandbox shape on all containers; the RuntimeClass `kata` carries only
  `scheduling.nodeSelector` (no toleration). File transfer is fetch-only
  today: `GET /v1/files` through the Router with a v2 scoped token.

## Bootstrap / deploy order

1. `task sandbox:preflight` (nested-virt on pmx-main) — must pass.
2. Tofu worker + machine config — the performance worker carries the
   `siderolabs/kata-containers` extension and the
   `workload.hermes.io/sandbox=true` label; no taints are applied anywhere.
3. Commit/push; Flux reconciles: `agent-sandbox` → `hermes-sandbox` →
   `hermes` (the last waits for the warm pool gate above).
4. Plugin/gateway and runtime image builds: one-shot buildkit Jobs under
   `kubernetes/infrastructure/home/image-builds/` (Flux-managed; see
   Upgrade below).

## Operation

- **Interaction path (v1):** private OpenAI-compatible API server,
  `hermes:8642`, bearer `API_SERVER_KEY`. The API has no public route and no
  messaging platform; the dashboard on `:9119` IS published at
  `https://hermes.ngoldack.de` behind authentik SSO (see Dashboard exposure
  below) and remains reachable with `kubectl port-forward` for Desktop.
  ```bash
  curl -H "Authorization: Bearer $API_SERVER_KEY" \
    -H 'Content-Type: application/json' \
    -d '{"model":"local","messages":[{"role":"user","content":"…"}]}' \
    http://hermes.hermes.svc:8642/v1/chat/completions
  ```
  `model: local` routes to the P100 (qwen36-35b) through the in-cluster
  agentgateway. The ConfigMap pins that path explicitly, and all three keys
  matter:
  - `provider: custom:agentgateway` — with the default `auto` Hermes resolved
    `local` to the **OpenRouter** provider, whose public API this namespace's
    egress allowlist blocks (every session then failed with "Hermes can't reach
    the model provider" while a direct curl to the Service worked);
  - `providers.agentgateway.key_env: OPENAI_API_KEY` — without it the request
    reached agentgateway with an empty bearer token and was rejected 401
    ("token header is malformed"); `key_env` names the pod env var, so no
    credential is stored in the ConfigMap;
  - `providers.agentgateway.request_timeout_seconds: 600` — the P100 is a
    reasoning model on one GPU: a single call runs ~125 s (≈11k-token system
    prompt) and a turn with tool calls takes minutes, so the default budget
    interrupted calls with "Operation interrupted: waiting for model response".
    This is the canonical v12+ shape; the old `custom_providers[0].timeout`
    was an unknown key (`providers.?: unknown config keys ignored: timeout`)
    and never took effect. On v2026.9.7 the runtime lookup
    (`hermes_cli/timeouts.py`) keys on the *resolved* provider id, and a named
    custom endpoint resolves to the bare id `custom`, so the LIVE knob is the
    `HERMES_API_TIMEOUT=600` env in the StatefulSet (the documented fallback in
    `run_agent._resolved_api_call_timeout` / `_stream_timeouts`); both are kept
    in lockstep.
  Budget accordingly: `hack/hermes-e2e.sh` allows `HERMES_E2E_MAX_TIME`
  (default 1800 s) for the chat call and polls for the claim for
  `HERMES_E2E_POLL_TRIES` × `HERMES_E2E_POLL_SLEEP` (default 900 × 2 s).
- **Acceptance:** `task hermes:e2e` runs `hack/hermes-e2e.sh`, the real
  gate. It asserts: 401 without / 200 with the bearer key; pre-flight refusal
  when stray claims already exist; HTTP 200 on the chat task; a claim labeled
  `agent-sandbox.hermes/owner=<gateway id>` observed *during* the turn; the
  adopted pod running `runtimeClassName=kata` on the node labeled
  `workload.hermes.io/sandbox=true`; in-guest evidence (the literal sentinel
  `E2E_GUEST_MARKER` plus a kernel release that differs from the host's);
  cleanup (warm pool back to `status.replicas=1`, no stray pods, no
  gateway-owned claims); and backend identity (`agent_sandbox` in
  `hermes plugins list`, `terminal.backend == agent_sandbox`). It prints
  `E2E PASS` on success and exits non-zero otherwise.
- **Kata smoke:** `task kata:smoke` schedules a `runtimeClassName: kata`
  pod, resolves the expected node by the sandbox label, and checks the guest
  kernel plus the absence of a serviceaccount token in the guest.
- **Plugin live suite:** `hack/hermes-plugin-integration.sh` requires
  `AGENT_SANDBOX_SEED` (the 32-byte Ed25519 seed, hex or base64) and runs
  `pytest -m integration -rs` against a port-forwarded Router
  (`GET /healthz` probe). Set `AGENT_SANDBOX_IN_CLUSTER=1` for the exec
  assertions — from a pod in the `hermes` namespace, because sandbox
  ingress allows gRPC exec only from there — or `0` for the deliberate
  non-exec subset (skips are printed). Seed extraction:
  ```bash
  sops -d kubernetes/infrastructure/home/hermes/secret.sops.yaml \
    | grep AGENT_SANDBOX_ROUTER_TOKEN | awk '{print $2}' \
    | base64 -d | xxd -p
  ```

### Model choice
`model` names either the default route of the `agentgateway` provider or one of
the three Synthetic tiers — one gateway Service and one JWT for all of them,
selected purely by path/model id:

| Alias (`model`) | Gateway path | Upstream model | Live latency |
|---|---|---|---|
| `local` (default) | `/v1` → `local-p100` → `llama-kv-broker` | `qwen36-35b` on the P100 | ~125 s/call |
| `syn-small` | `/v1/synthetic-small` | `zai-org/GLM-4.7-Flash` | ~2.6 s |
| `syn-large` | `/v1/synthetic-large` | `zai-org/GLM-5.3-Flash` | ~4.0 s |
| `syn-deepseek` | `/v1/synthetic-deepseek` | `deepseek-ai/DeepSeek-V4.1-Flash` | ~9.6 s |

All three tiers are declared in `hermes/configmap.yaml` (`providers.syn-*`,
each with its own tier-suffixed `base_url` — the pod-local relay, see Streaming
below — plus `key_env: OPENAI_API_KEY` and an
**explicit** `models:` list — Synthetic answers `GET <tier>/models` with 400, so
discovery can never populate the catalog; the ids are Synthetic's own aliases,
not the upstream names). Two ways to select one, plus the global default:

- **Per request** (API server): pass Synthetic's model id *and* the provider —
  `{"model":"syn:small:text","provider":"custom:syn-small", …}`. The body's
  `provider` always wins, so no other caller is affected.
- **By alias**: `{"model":"syn-small", …}` with no `provider` resolves through
  `platforms.api_server.extra.model_routes` (`syn-small` → `{model:
  syn:small:text, provider: custom:syn-small}`); the same mapping is what
  `GET /v1/models` advertises.
- **Global default**: the dashboard's **Models** page — the only path that
  moves the default off `local`, which is deliberately left as the P100
  (`provider: custom:agentgateway`, `model: local`).

#### Streaming: the agentgateway chunked-body defect (Synthetic tiers only)
A **streamed** turn against any of the three Synthetic tiers fails on
agentgateway **v1.5.0** (chart + image, `agentgateway/helmrelease.yaml`) even
though the same call without `"stream": true` is byte-perfect. The gateway never
terminates the chunked body of a streamed response whose upstream is *remote*
(`api.synthetic.new`; the same path shape applies to OpenRouter), so the last two
body bytes — including the `0 CRLF CRLF` terminator — never arrive, while every
SSE event up to `data: [DONE]` does. Signatures, all observed live 2026-09-17
from inside `hermes-0`:

```bash
# the missing terminator: curl exits 18 (partial transfer), not 0
kubectl -n hermes exec hermes-0 -c hermes -- sh -c \
  'curl -sN -X POST http://agentgateway.agentgateway.svc.cluster.local:80/v1/synthetic-small/chat/completions \
     -H "Authorization: Bearer $OPENAI_API_KEY" -H "content-type: application/json" \
     -d "{\"model\":\"syn:small:text\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}],\"stream\":true}" \
   > /tmp/t.bin; echo "curl_exit=$?"; tail -c 5 /tmp/t.bin | od -c'
# → curl_exit=18, body ends `data: [DONE]` with no trailing newlines and no
#   `0 \r \n \r \n`. The identical request to /v1/chat/completions (the local
#   P100 route) exits 0 and ends `data: [DONE] \n \n` — the defect is upstream-
#   side, not path-side.
```

- gateway log: `warn proxy::gateway proxy error: error from user's Body stream`
  (`kubectl -n agentgateway logs deploy/agentgateway`)
- httpx / OpenAI SDK: `RemoteProtocolError('peer closed connection without
  sending complete message body (incomplete chunked read)')` at the end of the
  stream
- Hermes turn: `hermes={completed:false, partial:true, error_code:'output_truncated'}`
  with `"error": "Response remained truncated after 4 continuation attempts"` and
  the user-visible "No visible answer was produced…" — *also* for a
  non-streaming API request, because the turn streams to the model regardless of
  the client's `stream` flag
- an HTTP/1.0 client completes: its body is delimited by connection close rather
  than by chunked framing

**No Hermes provider key fixes this** — the alternatives were measured, not
assumed. `providers.<name>.extra_body: {stream: false}` rewrites only the
request body while the SDK still parses the response as SSE (a silent **0-chunk**
stream → `EmptyStreamError` → "empty response stream after 4 attempts").
`api_mode`/`transport` select a wire protocol
(`chat_completions`/`codex_responses`/`anthropic_messages`/`bedrock_converse`),
not a streaming mode, and there is no `stream` key in
`hermes_cli/config_providers._KNOWN_PROVIDER_KEYS` (an unknown provider key is
warned about and ignored). Per-model `capabilities` are lifted onto
`agent.capabilities` but consumed only by native compaction/delegation, and
`model_routes` accepts only model/provider/api_key/base_url. The one switch
Hermes does honour, `model.streaming: false` (`agent_init._apply_display_config`
→ `agent._disable_streaming` → `_should_stream`), is **global**: it would stop
the P100 streaming too.

**Mitigation (in force):** the three `providers.syn-*` `base_url`s point at a
pod-local relay — `hermes/stream-relay.yaml` (script) plus the `syn-stream-relay`
container in `hermes/statefulset.yaml`, listening on `127.0.0.1:8643`. The relay
forwards the request to the very same agentgateway Service and returns the
response **close-delimited** (no `Transfer-Encoding`, no `Content-Length`, one
`Connection: close`) — the HTTP/1.0-equivalent shape that completes; everything
else is untouched (same path, same `Authorization` JWT, same `stream: true`, SSE
still arrives incrementally — a live syn-small turn streams in ~3 s). The P100
provider keeps calling the gateway Service directly and still streams; the relay
binds loopback only, so **no CNP, Service or gateway object changes with it**
(the pod's existing `agentgateway` :80 egress rule carries the relay's
forwarding, and a localhost listener needs no ingress rule). It logs
`upstream body ended early (RemoteProtocolError) - closing body` once per
repaired response.

**Removal condition — agentgateway ≥ the version that fixes remote-upstream body
framing.** The relay is scaffolding for a gateway defect, nothing more: when an
agentgateway upgrade terminates the chunked body for remote upstreams, delete it
and point the three `base_url`s back at
`http://agentgateway.agentgateway.svc.cluster.local:80/v1/synthetic-<tier>`
(drop `stream-relay.yaml` from `kustomization.yaml`, the container + volume in
`statefulset.yaml`, then bump `config-rev` + the pod annotation again). The gate
is exactly the probe above, aimed at the gateway Service: it must print
`curl_exit=0` and end with `0 \r \n \r \n`.

### Dashboard exposure (edge + authentik SSO)
The dashboard runs its own OIDC login
(`plugins/dashboard_auth/self_hosted`, authorization-code + PKCE as a
**public** client), so the edge route backs onto the app directly — the
authentik outpost is NOT used (its proxy breaks the app's `/auth/callback`,
the headlamp/langfuse lesson). Wiring, all of it in this repo:

| Piece | Where |
|---|---|
| Provider `hermes-dashboard-oidc` + app `hermes-dashboard` (+ akadmin binding) | `authentik/seed.yaml` — `client_type: public`, `signing_key: authentik Internal JWT Certificate` (without it authentik signs HS256 and the JWKS is empty, which the dashboard's RS256 verifier rejects), strict redirect `https://hermes.ngoldack.de/auth/callback` |
| Route `hermes-dashboard-edge` → `hermes:9119` | `hermes/httproute-edge.yaml` (Gateway `edge`, listener `https-apex`); requires the namespace label `gateway.ngoldack.de/edge-ingress: "true"` (`hermes/namespace.yaml`) |
| Pod env | `hermes/statefulset.yaml`: `HERMES_DASHBOARD_PUBLIC_URL=https://hermes.ngoldack.de` (makes the redirect URI absolute), `HERMES_DASHBOARD_OIDC_ISSUER=https://authentik.ngoldack.de/application/o/hermes-dashboard/`, `HERMES_DASHBOARD_OIDC_CLIENT_ID=hermes-dashboard-oidc` |
| Cilium | `hermes/cilium-policy.yaml`: ingress from the cilium `ingress`/`health` entities on **9119 only** (8642 stays in-cluster), egress to the `authentik` namespace for discovery/token/JWKS — paired with a `fromEndpoints: hermes` ingress rule in `authentik/cilium-allowlist.yaml`, because Gateway traffic is checked against the route's backend (portal-bridge) with the client's identity; a CIDR-only rule yields 403 "Access denied" |

No client secret exists: the provider is a public PKCE client, so nothing in
`hermes/secret.sops.yaml` covers OIDC. The `HERMES_DASHBOARD_BASIC_AUTH_*`
credentials stay valid — they back the dashboard's `basic` password provider
(the `/login` form and `POST /auth/password-login`, which mints the session
cookies Desktop and port-forward sessions use); HTTP Basic itself is not
accepted once a session provider is configured. The dashboard is the only
published port; `https://hermes.ngoldack.de/:8642` does not exist.

### Observability (Langfuse)
Hermes exports every turn to the in-cluster Langfuse through the **bundled**
`observability/langfuse` plugin (`/opt/hermes/plugins/observability/langfuse`
in the base image; hooks for API requests, LLM calls, tool calls and session
lifecycle). The plugin **fails open**: with the SDK or the credentials missing
its hooks no-op and nothing is exported — no error surfaces, which is why the
image build asserts the SDK and the tables below exist.

| Piece | Where |
|---|---|
| Enablement | `hermes/configmap.yaml`: `plugins.enabled: [agent_sandbox, observability/langfuse]` (seeded by the `config-rev` bump) |
| Credentials | `hermes-secret` keys `HERMES_LANGFUSE_PUBLIC_KEY` / `HERMES_LANGFUSE_SECRET_KEY` (StatefulSet env) — the same Langfuse project as the agentgateway OTLP exporter (`agentgateway/langfuse-otel.sops.yaml`) |
| Endpoint | `HERMES_LANGFUSE_BASE_URL=http://langfuse-web.langfuse.svc.cluster.local:3000`; the plugin's default is `cloud.langfuse.com`, which the egress allowlist blocks |
| SDK | baked into the gateway image by `image-builds/hermes-agent-sandbox-plugin/Dockerfile` (`langfuse==4.15.4`, installed into `/opt/hermes/plugin-venv` and exposed to the gateway interpreter through the same `.pth` as the agent_sandbox plugin). A runtime install is impossible by design: the venv is read-only and the container runs `HERMES_DISABLE_LAZY_INSTALLS=1` |
| Network | `hermes/cilium-policy.yaml` egress to the `langfuse` namespace on **3000**, paired with `langfuse/cilium-allowlist.yaml` ingress from `hermes` — that namespace is default-deny, so one half alone silently drops the export |

A turn appears as a `Hermes turn` chain with one generation per LLM call and a
span per tool call, tagged `hermes`/`langfuse`. Content capture is the plugin
default (`sanitized`: secret-pattern redaction, then truncation to
`HERMES_LANGFUSE_MAX_CHARS`, default 12000); set
`HERMES_LANGFUSE_CAPTURE=metadata` for sizes/ids/usage only,
`HERMES_LANGFUSE_ENV`/`_RELEASE` for tagging. All of these are optional — the
two required vars are the key pair.

Ingestion lag (observed 2026-09-17): the SDK's OTLP exporter sends only
`x-langfuse-sdk-name`/`-version`/`-public-key`, not
`x-langfuse-ingestion-version: 4`, so the server (langfuse 4.24.0 here) takes
its slow ingestion path — a finished turn showed up **≈4–5 min** after the
response (measured: turn span 16:37:40Z, visible via the API by 16:42). Two
further v4-vs-plugin notes: the plugin's trace-input update
(`span.update_trace(...)`, a v3 API) is wrapped in `_failsafe` and therefore
skipped, so a trace's `input` stays empty while the root span, metadata,
generations and timing are intact; and the SDK's `Failed to detach context`
ERRORs in the gateway log come from entering the observation context in one
task and leaving it in another (the plugin is fail-open, spans still export).

**Reading traces back — the legacy endpoint is disabled here.** This
deployment runs Langfuse v4 in *events_only* mode, so the v3 list endpoints
(`/api/public/traces`, `/api/public/observations`, `/api/public/metrics/daily`)
answer **404** with `{"message":"This endpoint is not available on deployments
running in Langfuse v4 events_only mode"}`. That 404 body parses as an empty
JSON object, so a naive poller reports "0 traces" instead of an error — use the
v4 endpoint:

```bash
kubectl -n hermes exec hermes-0 -c hermes -- curl -s -u \
  "$HERMES_LANGFUSE_PUBLIC_KEY:$HERMES_LANGFUSE_SECRET_KEY" \
  'http://langfuse-web.langfuse.svc.cluster.local:3000/api/public/v2/observations?limit=100' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(o["traceId"], o["type"], o["name"], o["startTime"]) for o in d["data"]]'
```

A hermes turn appears there as a `CHAIN`/`Hermes turn` root observation with a
`GENERATION`/`LLM call N` child per model call (verified live: trace
`0797ec353ce89cfe6bd01983495f84b6`, root `662af44369b3149c`, generation
`ef8c5c1dce20e820`, latency 37.8 s).

Verify the wiring from inside the pod (the container env carries the keys, so
no secret is typed):

```bash
kubectl -n hermes exec hermes-0 -c hermes -- /opt/hermes/.venv/bin/python -c \
  "import langfuse; print(langfuse.__version__)"
kubectl -n hermes exec hermes-0 -c hermes -- hermes plugins list | grep langfuse
kubectl -n hermes exec hermes-0 -c hermes -- curl -s --max-time 20 \
  -u "$HERMES_LANGFUSE_PUBLIC_KEY:$HERMES_LANGFUSE_SECRET_KEY" \
  -o /dev/null -w '%{http_code}\n' \
  'http://langfuse-web.langfuse.svc.cluster.local:3000/api/public/health'
```

## Secrets inventory & rotation

All keys live in `hermes/secret.sops.yaml`.

| Key | Consumed as | Rotation |
|---|---|---|
| `OPENAI_API_KEY` (`stringData`) | agentgateway client-credentials JWT — the `agentgateway` custom provider's key | re-encrypt a fresh JWT; `kubectl -n hermes rollout restart statefulset/hermes` |
| `API_SERVER_KEY` (`stringData`) | bearer token for the `:8642` API | new random value; restart the pod |
| `HERMES_DASHBOARD_BASIC_AUTH_USERNAME/PASSWORD/SECRET` (`stringData`) | dashboard auth only (the dashboard runs in the gateway container) | re-encrypt; restart the pod |
| `HERMES_LANGFUSE_PUBLIC_KEY` / `HERMES_LANGFUSE_SECRET_KEY` (`stringData`) | Langfuse project key pair for the `observability/langfuse` plugin (trace export to `langfuse-web`) | rotate in the Langfuse project → re-encrypt **both** here and `agentgateway/langfuse-otel.sops.yaml` (same project pair) → restart the pod; the plugin rejects keys without the `pk-lf-`/`sk-lf-` prefixes and drops every event at flush time |
| `AGENT_SANDBOX_ROUTER_TOKEN` (**`data:`**) | Ed25519 seed mounted at `/opt/data/router-token` | new 32-byte seed → derive pubkey → update `agent-sandbox/router-auth-keys.yaml` (`kid: hermes-1`) → rollout Router → update Secret → verify `wc -c /opt/data/router-token` == 32 |
| `OPENROUTER_API_KEY` (`stringData`) | **unused** — no pod consumes it; the model path is agentgateway | retained for now; not injected |

The token file must stay base64-encoded under `data:` — a `stringData`
entry would mount the 44-char base64 text and the plugin's doctor row
(`agent_sandbox token file (32 B Ed25519 seed)`) fails, because the seed
must be exactly 32 bytes.

Plugin-side knobs (StatefulSet env): `AGENT_SANDBOX_GATEWAY_ID=hermes`
(claim ownership id; falls back to the pod hostname) and
`AGENT_SANDBOX_MAX_CONCURRENT` (default 1 — the process-wide capacity
semaphore).

## Upgrade / rebuild (gateway + runtime images)

Both build Jobs are Flux-managed (`image-builds/kustomization.yaml`), so a
rebuild is git-driven. Jobs are immutable: a new Job name is what triggers
Flux to apply it and prune its predecessor.

1. Commit the change (plugin source / Dockerfile) and push.
2. Bump the Job name suffix and BOTH the `--opt=context` SHA and the image
   tag in `image-builds/hermes-agent-sandbox-plugin.yaml` (gateway) or
   `hermes-sandbox-runtime.yaml` (runtime). The checked-in context SHAs
   predate the 2026-09-17 pinning change, so they must be bumped to the
   commit that carries the Dockerfile being built before a rebuild.
3. Flux reconciles the new Job; wait for completion.
4. Read the pushed digest: the gateway prints it with `task hermes:repin`
   (digest of the newest `build-hermes-agent-sandbox-plugin-*` Job); for the
   runtime read it from the Job log (`kubectl -n buildkit logs job/…`).
5. Re-pin the digest: `hermes/statefulset.yaml` (gateway; all three image
   references in that file) or `hermes-sandbox/sandbox-template.yaml`
   (runtime, `registry.ngoldack.de/hermes-sandbox-runtime:1.0.3-<sha>` tags
   move with their context SHA). Commit, push, reconcile.
6. `task hermes:e2e`.

Build details worth knowing: both Dockerfiles pin their base images by
digest, the runtime builds sandboxd from the v1.0.2 commit with upstream
version stamping, and the runtime cache uses one ref
(`buildcache-v3`) for `--export-cache` and `--import-cache` — bump both
lines together. The plugin image installs
`k8s-agent-sandbox[grpc]==1.0.2` and appends its venv through a `.pth`
(no `PYTHONPATH`), asserting at build time that the plugin is importable
and its entry point is discoverable.

### Upgrade-hermes (Hermes base image)
Bump `FROM docker.io/nousresearch/hermes-agent:v2026.9.7@sha256:<index
digest>` in the plugin Dockerfile (tag + digest together), verify the
plugin's import paths against the new layout (`agent.*` at v2026.9.7;
`hermes plugins compat` inside the image), rebuild per above. The build's
import/entry-point assertion fails loudly if the layout moved.

### CRD/stack upgrades (agent-sandbox)
`upstream/sandbox-with-extensions.yaml` is vendored verbatim (see its
README for tag + sha256). Kustomize patches carry all local changes. Treat
CRD upgrades as manual-review (v1beta1); validate with `kubectl explain`
before adopting new fields.

Renovate is not configured in this repo; pinning + digest updates are
manual per the steps above.

## Rollback
1. `git revert` (or checkout) the offending commit — never force-push.
2. If a claim/sandbox storm is in progress: scale the warm pool to 0
   (`kubectl -n hermes-sandbox scale sandboxwarmpool hermes-go --replicas=0`),
   delete stray claims.
3. If the gateway misbehaves: `kubectl -n hermes rollout undo statefulset/hermes`.
4. Confirm via `task hermes:e2e`.

## Troubleshooting

| Symptom | Check |
|---|---|
| Gateway pod CrashLoop: s6 preinit | Image must ENTRYPOINT the wrapper (`agent-sandbox-entrypoint.sh`) directly — s6 cannot run as uid 10000; rebuild if missing |
| `Unknown TERMINAL_ENV 'agent_sandbox'` | Plugin not registered: `hermes plugins list` inside the pod must show `agent_sandbox`; the entry point must name the module (`hermes_agent_sandbox.provider`, `register(ctx)` on it). The image build asserts importability + entry point, so a failed build is the earlier signal |
| 403 on sandboxclaims | Pod is not using SA `hermes` (`serviceAccountName`), or the RoleBinding in `hermes-sandbox` is missing (`hermes/rbac.yaml`) |
| Claim created but exec fails | Warm pool down (`task hermes:e2e` asserts `status.replicas=1` after cleanup); sandbox ingress: 9090 only from the `hermes` namespace, Router 8080 only from the Router's own pod labels |
| Model calls fail ("offline" / "can't reach the model provider") | Check the *provider* first: `grep -E '^provider:' /opt/data/config.yaml` must say `custom:agentgateway` — with `auto` Hermes calls OpenRouter, which the egress allowlist blocks. Then the network path: the `agentgateway` namespace on :80 (`hermes/cilium-policy.yaml`) plus a valid `OPENAI_API_KEY` JWT; probe from the pod with `curl http://agentgateway.agentgateway.svc.cluster.local:80/v1/models` |
| agentgateway rejects the call 401 "token header is malformed" | The `providers.agentgateway` entry lost its `key_env: OPENAI_API_KEY` (or the Secret's `OPENAI_API_KEY` is empty) — the provider then sends an empty bearer token |
| Model turn interrupted ("waiting for model response") | The 600 s budget is gone: keep `providers.agentgateway.request_timeout_seconds: 600` **and** the `HERMES_API_TIMEOUT=600` env in the StatefulSet (a single call is ~125 s and tool turns take minutes; on v2026.9.7 the env is what the client actually reads for a named custom provider — see the Operation bullets) |
| A Synthetic call behaves like the P100 (≈40 s, `qwen36-35b`-class answer) | The tier did not route and the request fell back to the global default. Check (a) the provider name matches the one the request/route sent (`custom:syn-small`/`-large`/`-deepseek`), (b) `providers.syn-*` still carries its own `base_url` **with the `/v1/synthetic-<tier>` suffix** — without it the call hits `/v1`, i.e. the P100 — plus `key_env: OPENAI_API_KEY` and an explicit `models:` list (Synthetic rejects `GET <tier>/models` with 400, so a missing list leaves the provider with no catalog), and (c) the pod actually restarted onto the new config: the same `config-rev`/annotation bump rule as above (`grep -A3 'syn-small:' /opt/data/config.yaml`). The tiers answer in single-digit seconds; the P100 id or ~40 s means fallback, not the tier |
| A Synthetic turn returns "No visible answer was produced…" / `error_code: output_truncated` | Streaming defect: agentgateway never terminates the chunked body for a remote upstream (see Model choice → Streaming). Check the relay first: `kubectl -n hermes get pod hermes-0 -o jsonpath='{.spec.containers[*].name}'` must list `syn-stream-relay`; `kubectl -n hermes exec hermes-0 -c hermes -- curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8643/healthz` must print `200`; `kubectl -n hermes logs hermes-0 -c syn-stream-relay` must show the request (and `upstream body ended early (RemoteProtocolError) - closing body`); and `grep -A1 'syn-small:' /opt/data/config.yaml` must show `base_url: http://127.0.0.1:8643/v1/synthetic-small`. A pod with no `syn-stream-relay` container is still on the pre-relay config — `config-rev` and the pod annotation must both be at `7` |
| Turn runs but no trace appears in Langfuse | In order: `hermes plugins list` shows `observability/langfuse` enabled (`plugins.enabled` in `hermes/configmap.yaml`, needs the `config-rev` bump); `/opt/hermes/.venv/bin/python -c "import langfuse"` prints a version (without the SDK the plugin fails open — rebuild the image); the pod log has no "credentials look like placeholders" warning (the pair must be `pk-lf-`/`sk-lf-`); `curl http://langfuse-web.langfuse.svc.cluster.local:3000/api/public/health` answers from the pod (if it hangs, the Cilium pair is incomplete: egress in `hermes/cilium-policy.yaml` + ingress in `langfuse/cilium-allowlist.yaml`). **Do not poll `/api/public/traces`** — this deployment is Langfuse v4 *events_only*, where that endpoint 404s with an "events_only mode" message that parses as an empty result; read `/api/public/v2/observations` instead. Events are batched and the SDK sends no `x-langfuse-ingestion-version: 4`, so allow ~5 min before concluding anything |
| Second concurrent session fails immediately | `agent_sandbox capacity: 1 active sandbox is supported on this node; retry after the running session finishes` — expected on a one-slot pool (`AGENT_SANDBOX_MAX_CONCURRENT`, default 1); serialize sessions or add real node headroom first (see Capacity) |
| A command returns 124 | `command exceeded {timeout}s and was cancelled; the sandbox was recycled, so /workspace state is gone — re-run setup if needed` — the gRPC deadline (`DEADLINE_EXCEEDED`) fired and the sandbox was torn down; other transport failures are reported as command errors, not 124 |
| `router-token` wrong size | Must be exactly 32 bytes under `data:` (base64 of the raw seed) — never `stringData` (44-char base64 text). The plugin fails the doctor row instead of the first file operation |
| Sessions run LOCAL despite `terminal.backend: agent_sandbox` | The seed is revision-aware: `/opt/data/config.yaml` is re-copied (with a `config.yaml.bak` backup) whenever `hermes-config`'s `config-rev` differs from `/opt/data/.config-rev`. To change config, edit `hermes/configmap.yaml` and bump BOTH `config-rev` and the pod-template annotation `hermes.ngoldack.de/config-rev`. `TERMINAL_ENV=agent_sandbox` is the hard override; verify `hermes config get terminal.backend` == `agent_sandbox` |
| Dashboard 9119 unreachable or WS drops (~1s) | The dashboard runs inside the gateway container, started and supervised by the image entrypoint (`hermes dashboard … 9119`), and the container's readiness probe checks both listeners — reach it via `https://hermes.ngoldack.de` (edge, authentik SSO) or `kubectl port-forward svc/hermes 9119:9119` and the `basic` password provider (the `HERMES_DASHBOARD_BASIC_AUTH_*` credentials; HTTP Basic is not accepted); its config must live on the PVC (seeded by the initContainer), never a mounted ConfigMap (a read-only mount breaks persistence) |
| Edge dashboard login loops back to the login page | The issuer must be the **browser-visible** host (`https://authentik.ngoldack.de/application/o/hermes-dashboard/`) so the ID token's `iss` matches; a trailing-slash difference is tolerated (the plugin `rstrip`s). Also confirm the pod can reach it: `kubectl -n hermes exec hermes-0 -c hermes -- curl -sS https://authentik.ngoldack.de/application/o/hermes-dashboard/.well-known/openid-configuration` must return JSON — if it says `Access denied`, the hermes↔authentik Cilium pair is missing (`hermes/cilium-policy.yaml` egress + `authentik/cilium-allowlist.yaml` ingress) |
| OIDC token exchange fails (`invalid_client`) | The provider must be `client_type: public` — the dashboard is a PKCE public client and sends no `client_secret`. Setting it to `confidential` requires a secret: add `HERMES_DASHBOARD_OIDC_CLIENT_SECRET` to `hermes/secret.sops.yaml` + the StatefulSet, and mount the same value into authentik as a `seed-secrets-*` volume with `client_secret: !File` (the headlamp shape) |
| Dashboard rejects the ID token (signature / empty JWKS) | The provider lost `signing_key` (see `authentik/seed.yaml`): without it authentik signs HS256 and `/application/o/hermes-dashboard/jwks/` returns `{}` while the plugin verifies RS256/ES256 |
| 302/404 on `https://hermes.ngoldack.de/` with no redirect to authentik | Route not attached: the `hermes` namespace needs `gateway.ngoldack.de/edge-ingress: "true"`, and the CNP must allow the cilium `ingress` entity on 9119 |
| Edge name will not resolve from a LAN workstation although the route is live | The LAN resolver can serve a cached NXDOMAIN (and plain port-53 probes are intercepted here, so `dig @1.1.1.1` lies the same way — google/cloudflare/Hetzner all returning an identical blank answer is the tell). Read the truth over DoH (`curl 'https://dns.google/resolve?name=hermes.ngoldack.de&type=A'`) and bypass the cache for the request itself (`curl --resolve hermes.ngoldack.de:443:2.28.31.116 https://hermes.ngoldack.de/`) |
| Sandbox `go test` fails "read-only file system" on build cache | Runtime image must set `HOME=/tmp` (writable emptyDir); runtime ≥1.0.3 has it |
| `task check` fails | `yamllint` / `tofu fmt` / `kustomize` / `lint:orphans` / sops — run at repo root with `SOPS_AGE_KEY_FILE` set |

## Capacity
One warm + one active sandbox maximum, sized from the **shared** node's
headroom rather than a dedicated VM: `wk-main-performance` (`talos-919-w9u`,
16 CPU / 48 GiB VM, ~46.5 GiB allocatable) also carries qwen36-35b (4 CPU /
8 GiB requests), nomic-embed-v15 (2 CPU / 4 GiB), buildkitd (12 CPU /
20 GiB limits, mostly idle) and roughly three dozen other pods. Warm pool
`replicas: 1`; sandbox limits 4 CPU / 5 GiB.

The node is deliberately mixed-use (operator decision 2026-09-17, replacing
the earlier dedicated-worker carve): the `workload.hermes.io/sandbox`
label only **attracts** sandbox pods — it excludes nothing, and no taint is
applied. Accepted consequences: a sandbox session competes with GPU
inference and the image builder for CPU/RAM, so a runaway session can
starve inference and an inference burst can starve or OOM-kill a session;
a host-side Kata/QEMU escape would land on a node that also runs
`sandbox-router`, the nvidia device plugin, tetragon, crowdsec and the
`truenas-csi` node plugin. To support more concurrency, raise real headroom
first (more RAM on the VM, or move a tenant off it), then re-derive the
limits — never raise replicas without the capacity.

## Monitoring
`monitoring/scrapes.yaml` scrapes `sandbox-router` (pod scrape, :9090) and
the agent-sandbox controller (service scrape, :8080). The VMRule group
`hermes-sandbox` in `monitoring/rules.yaml` carries the alerts:
`HermesSandboxWarmPoolNotReady`, `HermesSandboxClaimStuck`,
`HermesGatewayNotReady`, `HermesGatewayRestarting`,
`HermesSandboxPodOOMKilled`, `HermesSandboxPodEvicted`,
`HermesSandboxNodeMemoryPressure`, `HermesSandboxNodeMemoryLow`. The node
name (`talos-919-w9u`) and node-exporter instance (`10.30.0.22:9100`) are
pinned in the expressions because node labels are not queryable from
metrics here — update them with the live node if it is renamed.

## Backup
`data-hermes-0` PVC (10 GiB, `truenas-fast-nfs`) is covered by the repo's
NAS snapshot policy (see README "Storage safety policy"); restore = create
a PVC from the snapshot and point the StatefulSet's `volumeClaimTemplates`
selector at it. Sandbox emptyDirs, warm-pool pods and runtime caches are
never backed up.

## Known gaps (tracked, deliberate)
- **Sandbox egress is DNS-only; module/package downloads fail** — the
  sandbox CNP (`hermes-sandbox/cilium-policy.yaml`) allows cluster DNS and
  nothing else; the old github.com/proxy.golang.org `toFQDNs` rule was
  deleted because the Cilium DNS proxy is not in transparent mode
  (`tofu/home/cilium.tf`), so an FQDN allow could never learn IPs. A coding
  task that needs downloads requires an internal mirror selected with
  `toEndpoints`, or a deliberate CIDR allow added there. The plugin injects
  the same facts into every session prompt.
- **`write_file`/`read_file` on sandbox paths** — the host-side write root
  stays `/opt/data` (the data PVC; the container rootfs is read-only), and the
  plugin implements BOTH halves of the Router file path: `fetch_file`
  (`GET /v1/files`, v2 scoped tokens) and, since the Phase-2 capability layer,
  `write_file`/`read_file`/`list_dir` over `PUT`/`GET /v1/files`
  (`hermes-agent-sandbox-plugin/files.py`, `transport.SandboxTransport.put_file`;
  sandboxd v1.0.2 serves `PUT` with atomic temp+rename and auto-created
  parents). The plugin's own surface is therefore complete for guest-side file
  I/O; the host-side write root was deliberately NOT widened.
  **Status of the observed consequence (2026-09-17):** the deployed local model
  reached for `write_file` rather than the terminal, got `Write denied: …`
  (root `/opt/data`), and the turn produced no sandbox claim at all. The plugin
  write path now exists, but whether Hermes' `write_file` tool routes through it
  or through the host-side root is a **live re-check item** (the plugin's
  methods are additive to the `execute()` contract Hermes' tools use). The
  terminal path itself is verified independently of the model: a claim → adopt
  → gRPC exec → teardown run inside the gateway pod returns the guest kernel,
  uid 10001 and `/workspace`.
- **Monitoring does not scrape the gateway, by design** — hermes exposes no
  Prometheus endpoint (`:8642/metrics` → 404, `:9119/metrics` → 302 to
  `/login?next=%2Fmetrics`); do not re-probe. A scrape (plus a monitoring
  ingress allow) only becomes worthwhile if Hermes ever ships `/metrics`.
- **No Renovate** — pin/digest updates are manual (see Upgrade).
