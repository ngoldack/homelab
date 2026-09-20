# Go port contract — hermes-egress-guard (authorizer + reaper)

Full port of the Python `egress_guard` package to Go. Behavioral equivalence with
the Python, byte-for-byte on the wire. Read this ENTIRE document before writing code.

## Module layout (golang.org/x style, validated on go 1.27.1 darwin/arm64)

```
kubernetes/infrastructure/home/image-builds/hermes-egress-guard/
  go.mod                                  # module hermes-egress-guard, go 1.27
  go/                                     # NEW Go sources (package hermes_egress or main)
    auth.go  metrics.go  config.go  http.go  k8sapi.go  policy.go
    reaper.go  authorizer.go
    auth_test.go  metrics_test.go  config_test.go  http_test.go
    k8sapi_test.go  policy_test.go  reaper_test.go  authorizer_test.go
  Dockerfile                              # REWRITTEN: golang build stage + runtime
  src/egress_guard/                       # Python: REMOVED at P3 after Go parity
  tests/                                  # Python tests: REMOVED at P3
```

## Toolchain & build

- Build with `golang:1.27-alpine` (the cluster image, kv-broker precedent). Go binary
  lives at `cd go && go build -trimpath -ldflags="-s -w" -o /out/guard ./cmd/guard` (the
  go.mod is inside go/, so build from go/; the entrypoint is cmd/guard/main.go — a
  `package main` importing the root `package guard` library as `hermes-egress-guard`).
- Stdlib only. Do NOT run `go get`, do NOT add `require`/`go.sum`. The needed modules
  are std in 1.27: `encoding/json`, `encoding/base64`, `crypto/hmac`, `crypto/sha256`,
  `net/http` (server AND client), `net`, `net/url`, `strings`, `bytes`, `io`, `fmt`,
  `sort`, `strconv`, `sync`, `time`, `os`, `log`. NO `signal` package (not std).

## VERIFIED stdlib idioms (from kv-broker main.go + probes; DO NOT guess)

HTTP server (blocking, container SIGTERM terminates — kv-broker precedent, no signal hook):
```go
srv := &http.Server{
    Addr:    "0.0.0.0:8080",
    Handler: b,                       // b has ServeHTTP(w http.ResponseWriter, r *http.Request)
    ReadHeaderTimeout: 30 * time.Second,
}
if err := srv.ListenAndServe(); err != nil { log.Fatalf("serve: %v", err) }
```
Handler routing:
```go
func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    switch {
    case r.URL.Path == "/healthz":
        w.Header().Set("Content-Type", "application/json")
        io.WriteString(w, `{"status":"ok"}`)
    case strings.HasPrefix(r.URL.Path, "/check") && r.Method == http.MethodPost:
        ...
    default:
        http.NotFound(w, r)
    }
}
```
- Log to stdout/stderr: `log.Printf("...")` / `log.Fatalf("...: %v", err)`. NEVER
  `fmt.Fprintln(1, ...)` (1 is not an io.Writer; use `log.Printf`).
- JSON decode (event body / K8s responses, typed structs — mirror the Python field names):
```go
type kvBrokerEvent struct { ... }             // reflect JsonDecoder field naming
dec := json.NewDecoder(strings.NewReader(body))
var ev kvBrokerEvent
if err := dec.Decode(&ev); err != nil { ... }
```
- JSON encode: `json.Marshal(value)` → `(string, err)`. Unknown fields: keep decoder
  lenient by default (`dec.Decode(&x)`); do NOT DisallowUnknownFields unless needed.
- `io.WriteString(w, s)` writes the response; `w.Header().Set(name, value)`.
- `http.Error(w, msg, http.StatusForbidden)` / `http.NotFound(w, r)`.
- Strings: `strings.HasPrefix`, `strings.HasSuffix`, `strconv` for ints.

## Behavioral contract (byte-for-byte with the Python) — READ the source before writing

The Python files under `src/egress_guard/` are the SPEC. Your Go module MUST be
observably equivalent. Read each Python file + its test suite before porting.

### auth.go  (# src/egress_guard/auth.py)
- REGEX taken literally: SESSION_HASH_RE `[A-Za-z0-9._-]{1,128}`, PROFILE_RE
  `[a-z0-9-]{1,32}`, EVENT_ID_RE `[A-Za-z0-9_-]{8,64}`.
- token_mac_input = b"|".join([b"egress-token-v1", session_hash, str(expiry), profile]).
- token_mac = HMAC_SHA256 → b64url NOPAD hex.
- mint_token = "v1.{expiry}.{profile}.{mac}".
- verify_token: split on "." → 4 parts, part[0]=="v1", expiry isdigit, profile match,
  mac match (constant-time compare), expiry window [now-skew, now+max_ttl].
- sign_event = "v1=" + HMAC_SHA256(secret, body).hex.
- verify_event: startswith "v1=" + constant-time hex compare.
- EventReplayGuard: max_events 4096, skew 300; check(event_id, ts) → None|"malformed-event-id"
  |"malformed-timestamp"|"stale-timestamp"|"replay"; record() LRU evicts oldest.
  check validates event_id is a str + matches EVENT_ID_RE; ts is an int (not bool);
  abs(now-ts)<=skew; not seen.

### metrics.go  (# metrics.py)
- Prometheus text format, deterministic render. Declared counters with HELP text:
  hermes_quarantine_total, hermes_egress_denied_total, hermes_events_rejected_total,
  hermes_quarantine_sweep_total, hermes_quarantine_swept_total.
- inc(name, labels=None, amount=1); render() → "# HELP name help\ntype name counter\nname N\n"
  sorted by (name, labels), unlabelled counters seeded 0, integer values render as int.
- Thread-safe (sync mutex). Used by authorizer (denied_total) + reaper.

### config.go  (# config.py)
- env_str(name, default) (default when unset/empty); env_required(name) (exit 1 if unset);
  env_int(name, default) (exit 1 if non-int); env_float similarly.

### http.go  (# http.py)
- MAX_BODY_BYTES 64 KiB. read_body: Content-Length bounded; truncated → 400.
- json_result(status, payload, headers) → JSON body sorted_keys, content-type application/json.
- Request routing per Server contract above. The reaper MUST read the RAW body for HMAC.

### reaper.go  (# reaper.py) — INCLUDES the 2.E TTL sweep
- Env: EGRESS_HMAC_SECRET(required), EGRESS_QUARANTINE_NAMESPACE(hermes-sandbox),
  EGRESS_QUARANTINE_CONFIGMAP(hermes-quarantine), EGRESS_QUARANTINE_TTL_S(86400),
  EGRESS_QUARANTINE_SWEEP_S(3600), EGRESS_LISTEN_PORT(8080).
- POST /events: verify_event → 401 bad-signature | bad JSON → 400 | non-object → 400 |
  replay.check → 409 | kind=="kill" → quarantine else deny metric | K8s failure → 503
  (NOT recorded in replay) | success → record + 200 {status:processed,event_id}.
- quarantine: session_hash regex; list_claims by label `workload.hermes.io/session-hash`=
  <hash>; write ledger entry then delete each claim (UID precondition), one failure
  does not strand; ledger (admission gate) written BEFORE deletes. quarantine_entry:
  JSON {reason,strikes,quarantined_at,ttl_s,source_ip,target=host:port} sorted_keys,
  ttl_s = now+ttl_seconds (event ttl_s or default).
- _write_ledger: get ConfigMap, add key, replace with resourceVersion, 409→re-read+merge.
- _sweep_expired (2.E): re-read ledger, drop keys ttl_s<=now, rewrite 409→skip; sweep every
  EGRESS_QUARANTINE_SWEEP_S (daemon thread).
- GET /healthz {status:ok,role:reaper}; GET /metrics (Prometheus text).

### authorizer.go  (# authorizer.py) + policy.go (# policy.py)
- POST /check: parse CheckRequest {method,host,port,headers,source_ip,byte_hint};
  lower-case headers; split host[:port] (no resolution); basic credentials from
  Proxy-Authorization/Authorization; token verify → Identity or deny.
- EgressEngine: builtin kill CIDRs (RFC1918/loopback/link-local/CGNAT/etc + IPv6),
  kill_exact; per-session strike window (threshold 3 / 60s), byte budget (now inert);
  Decision(kind,reason,strikes,status): ALLOW 200, DENY 403, KILL 403. deny→strikes→KILL.
- Headers on response: x-egress-decision/reason/strikes/policy-version + session/profile
  if identity + x-egress-kill=1 if kill. Body {decision,reason,strikes,target}.
  deny/kill → emit_event (signed POST to reaper, never blocks decision).
- GET /healthz {status,role:authorizer,policy_version,profiles:sorted}.

## Rules
- Read the Python file + its tests FIRST; they are the ground truth.
- go vet + go build right after writing each file's .go.
- Do NOT run the global suite; do NOT format/lint. Build only.
- NO go get, NO require, NO go.sum.
## VERIFIED stdlib idioms — CRYPTO (cross-checked byte-for-byte vs Python)

HMAC-SHA256 (token MAC / event signature). CROSS-CHECKED: matches Python hmac.
```go
h := hmac.New(sha256.New, key)              // key []byte
h.Write(message)                            // message []byte  (feed BEFORE Sum)
sum := h.Sum(nil)                           // []byte digest — Sum(nil), NOT Sum(dst)
hex.EncodeToString(sum[:])                  // -> "f7bc83f4..."
```
Pitfalls: `Sum(dst)` with a dst buffer yields WRONG (empty) digest — use `Sum(nil)`.
`.Add()`/`.Update()` do NOT exist on hash.Hash; the feed method is `.Write([]byte)`.
`sha256.Sum256(data)` → `[32]byte` (value, no nil arg); slice `sum[:]` for EncodeToString.

Base64 urlsafe-nopad (token MAC b64url, mirrors Python b64url_nopad):
```go
enc := base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
s   := enc.EncodeToString(sum[:])           // urlsafe alphabet
s    = strings.ReplaceAll(s, "=", "")       // strip padding
```
(There is no `base64.URLEncoding` constant by that name; NewEncoding with the
urlsafe alphabet + strip `=` is the correct nopad form.)

SHA256 (policy fingerprints / non-HMAC hashing): `sha256.Sum256(data)` → `[32]byte`,
`hex.EncodeToString(sum[:])`.

## VERIFIED stdlib idioms — JSON
Typed decode (event body / K8s response; field names match the Python dict keys):
```go
dec := json.NewDecoder(strings.NewReader(body))
var ev Event
if err := dec.Decode(&ev); err != nil { ... }   // io.EOF at end
```
Encode (response body / K8s request): `json.Marshal(value)` → `(string, error)`.
`json.RawMessage(s)` wraps a pre-serialized string. Keep the decoder lenient
(do NOT call DisallowUnknownFields unless a strict contract needs it).

## VERIFIED stdlib idioms — strings
`strings.NewReader(s)` for a reader; `strings.HasPrefix/HasSuffix`; `strings.Join`.
String→bytes: `` []byte(`text`) `` (backtick literal). Bytes→string:
`strings.FromBytes(b)` if present, else decode via `strings.NewReader`+read.
Int↔string: `strconv` (fmt.Printf `%d`/`%s`). Log to stdout: `log.Printf(...)`.
NEVER `io.WriteString(1,...)`/`fmt.Fprintln(1,...)` — int 1 is not an io.Writer.
## TOOLCHAIN ENV (MANDATORY for all go commands)
The local `go` defaults to 1.25.3, which LACKS `encoding/json`/`net/http`. ALWAYS run:
    export PATH="$HOME/.proto/shims:$PATH"
    export GO_TOOLCHAIN=1.27.1
    go vet ./go/<file>.go      # compile-check ONLY
    go build -o /tmp/out ./go/<file>.go
NEVER `go get`; never add `require`/`go.sum` (the needed modules are std in 1.27.1).
