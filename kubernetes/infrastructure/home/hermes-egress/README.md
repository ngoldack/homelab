# hermes-egress — the Hermes sandbox egress guard (plan Phase 3)

One namespace, three services, one quarantine ledger:

| Service | Address | Contract |
|---|---|---|
| Forward proxy | `hermes-egress.hermes-egress.svc.cluster.local:3128` | CONNECT-capable explicit proxy; every request gated by ext_authz |
| Authorizer | `egress-authorizer.hermes-egress.svc.cluster.local:8080` | `POST /check` → 200 allow / 403 deny+strike / 403 + `x-egress-kill: 1`; `GET /healthz` |
| Reaper | `sandbox-reaper.hermes-egress.svc.cluster.local:8080` | `POST /events` (signed), `GET /healthz`; writes quarantine + deletes claims |

Sources of truth for behavior: `image-builds/hermes-egress-guard/go/` — `auth.go` (token + event HMAC), `policy.go` (rules engine + policy schema), `authorizer.go` (`/check` protocol), `k8sapi.go` (RBAC + CRD groups). This README documents the wire layout those files implement; on conflict the code wins.

## Session identity (Unit 3.5)

Identity is proven by ONE channel (a gateway-minted HMAC token), and the cluster
side links an event to objects by the claim label — never by anything the
sandbox can assert:

1. **Token (the only identity the authorizer uses).** The Hermes gateway mints a `Proxy-Authorization: Basic` token per session and injects it through `HTTP_PROXY`/`HTTPS_PROXY` proxy userinfo (see below). The authorizer verifies the HMAC over session hash + expiry + profile and nothing else: it is deliberately STATELESS and makes no Kubernetes API call (`authorizer.go`: "the authorizer makes no other network call (no DNS, no Kubernetes API)"). The sandbox controls its own HTTP headers, so anything but this verified token is untrusted — a sandbox cannot mint or forge another session's token without the HMAC secret (which never enters the sandbox; only the derived token does).
2. **Cluster objects are linked by the CLAIM LABEL, not by source address.** The ext_authz `CheckRequest` does carry `attributes.source.address.socketAddress.address` = the sandbox pod IP (Envoy `use_remote_address` on the listener) and the authorizer copies it into the signed event as `source_ip`; the reaper records it in the quarantine ledger for forensics. **No pod-IP → pod → SandboxClaim lookup is implemented** (a stolen token replayed from a different sandbox is *not* detected by the guard today): wiring that would require the authorizer to carry a Kubernetes API client plus pods/claims read RBAC, which the landed authorizer explicitly does not have. The reaper's enforcement leg instead resolves the session's claims by the `workload.hermes.io/session-hash` LABEL — the claim label is what links an event to cluster objects, and it is stamped by the plugin at claim creation, never by the sandbox.

## Token layout (`auth.py`, authoritative)

Proxy userinfo injected into the sandbox environment:

    http://<session-hash>:<token>@hermes-egress.hermes-egress.svc.cluster.local:3128

Envoy turns the userinfo into `Proxy-Authorization: Basic base64(<session-hash>:<token>)` and forwards it verbatim to the ext_authz callout, so:

- Basic **username** = the session hash (`workload.hermes.io/session-hash` label value, `[A-Za-z0-9._-]{1,128}`)
- Basic **password** = `v1.<expiry_unix>.<profile>.<mac>` with

      mac = b64url_nopad(HMAC_SHA256(secret,
                b"egress-token-v1|" + session_hash + b"|" + expiry_unix + b"|" + profile))

  `expiry_unix` in ASCII decimal; profile ∈ {core, offline, python, go, node, web} (`[a-z0-9-]{1,32}`).

Accepted window: `now - 60s` (skew) through `now + 86400s` (max TTL). Tokens are not single-use: every proxied request re-presents the same credentials; replay protection applies to **events**, not the session token.

The HMAC secret (`egress-hmac` Secret, `EGRESS_HMAC_SECRET` key, namespace `hermes-egress`) is shared by the authorizer (verifies tokens) and the reaper (verifies event signatures). The **gateway** uses a copy of the same value in its own namespace — `hermes-secret` key `EGRESS_HMAC_SECRET` (`kubernetes/infrastructure/home/hermes/secret.sops.yaml`), mounted as a file and pointed at by `AGENT_SANDBOX_EGRESS_HMAC_SECRET_FILE=/opt/data/egress-hmac` (the plugin's `config.py` knob; the same bytes must exist in both secrets or every proxied request answers 403 `token-invalid`). Rotating it invalidates every live token at once; sessions re-mint on claim create.

## Event signing (`auth.py`)

Authorizer → reaper on every deny/kill decision:

    {"event_id": "<uuid4 hex>", "ts": <unix int>, "kind": "deny"|"kill",
     "session_hash": "<hash>"|null, "profile": ..., "target_host": ...,
     "target_port": ..., "reason": ..., "strikes": <int>, "source_ip": ...}

Header `x-egress-signature: v1=<hex HMAC_SHA256(secret, raw body bytes)>` — verified over the raw received bytes, then parsed. The reaper's replay guard checks event id format, ±300 s timestamp window and seen-id LRU, and records the id only after the event is processed successfully.

## Decision protocol (Unit 3.2)

`POST /check` accepts the Envoy `AuthorizationRequest` JSON shape (`attributes.request.http` + `attributes.source`) or a flat shape for direct callers (`{method, target, proxy_authorization, source_ip, bytes}`). Deterministic order (`policy.py:EgressEngine.evaluate`):

1. unauthenticated → 403 `unauthenticated` (no strike)
2. instant-kill IP target → 403 + `x-egress-kill: 1` `kill-destination` (quarantine now)
3. unknown profile → 403 `profile-unknown` (no strike — a policy mistake, not sandbox behavior)
4. host/port not allowlisted → 403 `not-allowlisted` (strike; at the 3-denies/60 s threshold the same response escalates to `kill`/`strikes`)
5. byte budget exceeded → 403 `budget-exhausted` (strike)

Every decision carries `x-egress-decision` / `x-egress-reason` / `x-egress-strikes` / `x-egress-policy-version` (+ `x-egress-session` / `x-egress-profile` when authenticated). Envoy is configured fail-closed: authorizer down or slow → 403, never open.

## Quarantine path (Unit 3.3)

`kill` decision → signed event → reaper (`reaper.py`), in this order:

1. verify `x-egress-signature` over the RAW body bytes (`auth.verify_event`), then parse;
2. replay guard (event-id format, ±300 s window, seen-id LRU recorded only after successful processing — so the authorizer's retry still works);
3. write the `hermes-quarantine` ConfigMap (namespace `hermes-sandbox`, key = session hash, value = JSON reason/strikes/quarantined_at/ttl_s/source_ip/target). A missing ConfigMap answers 503 — fail-closed, never a silent pass;
4. resolve the session's claims by the `workload.hermes.io/session-hash` label and delete each with a UID precondition (`k8sapi.py:delete_claim`); one claim's delete failure never strands the others. If NO live claim carries the hash (the session recycled since the event), the hash is still quarantined — the LEDGER is the admission gate, the delete is the containment leg.

Kyverno's `hermes-session-quarantine` policy (audit today, `failurePolicy: Fail`) then reports/denies re-claims for that session hash; the plugin maps the admission denial to a permanent quarantine error. `source_ip` is recorded in the ledger for forensics only (see Session identity — no pod-IP lookup is performed).

### Clearing a quarantine

The reaper creates the (initially empty) `hermes-quarantine` ConfigMap at startup. Clearing is a deliberate, reviewable key deletion:

    kubectl -n hermes-sandbox get configmap hermes-quarantine -o yaml   # see keys + reasons
    kubectl -n hermes-sandbox patch configmap hermes-quarantine \
      --type=json -p '[{"op":"remove","path":"/data/<session-hash>"}]'

False positives affect only one session. There is no bulk clear.

## Wiring (Units 3.1/3.4)

- `hermes-sandbox/cilium-policy.yaml` permits sandbox egress ONLY to the proxy `:3128` + cluster DNS. A bypass attempt (direct CONNECT from a pod) is default-deny and Hubble-observable.
- `kubernetes/clusters/home/hermes-egress.yaml` (with `wait: true` + healthChecks on all three Deployments) exists and is listed in `kubernetes/clusters/home/kustomization.yaml`; its `dependsOn` must cover agent-sandbox (the CRDs the reaper's RBAC and the Kyverno policy kinds touch). The Flux Kustomization must NOT be reconciled before the guard images are digest-pinned: both Deployment `image:` lines currently carry a tag + PENDING-digest comment (see Images).

## Images

`registry.ngoldack.de/hermes-egress-guard` — one image, two services selected by the ROLE passed as the first argv (`authorizer` | `reaper`) to the Go binary `/guard` (see `image-builds/hermes-egress-guard/go/main.go`). Digests recorded by the build Jobs; pin before wiring.
