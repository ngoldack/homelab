# hermes-agent-sandbox

Hermes Agent terminal backend that executes every command in a Kubernetes
Agent Sandbox (kubernetes-sigs/agent-sandbox v1.0.2) Kata sandbox via the
`hermes-go` warm pool.

See the plugin `plugin.yaml` for metadata and `src/hermes_agent_sandbox/` for
implementation; the module docs under `config.py`, `client.py`, `transport.py`
explain the v2 scoped-token wire format and the claim lifecycle.

## Execution architecture (as deployed)

The pinned stack splits execution and file transfer:

- **Commands — direct gRPC to `sandboxd`, user-approved fallback.** v1.0.2
  exposes command execution only as the gRPC `ProcessService` on
  `podIP:9090` (verified: sandboxd's REST surface is `/v1/files`,
  `/v1/health`, `/v1/metadata`; `sandboxd --help` shows `grpc-port 9090`,
  `rest-port 8080`). The Go Router is an HTTP-only reverse proxy (no
  `/execute` API, `ForceAttemptHTTP2: false`), so Router-mediated execution
  does not exist in v1.0.2. Per user decision, the plugin executes via gRPC
  straight to the adopted pod's IP. Cilium confines this: ingress to the
  sandboxes on 9090 is admitted ONLY from `agent-sandbox-system` (Router)
  and `hermes` (gateway) namespaces (`hermes-sandbox/cilium-policy.yaml`).
  The gateway's claim ownership is the trust boundary (no random pod can
  create a claim for the warm pool).
- **Files — through the Router with v2 scoped tokens.** `fetch_file` uses
  `GET /v1/files` with a per-request Ed25519 v2 token (`client.py`,
  byte-parity with the Go verifier in `authorizer.go`/`scopedtoken_v2.go`).
  Paths keep literal `/` (percent-encoded `%2F` does not survive the Go
  request reparse and 403s — see `transport.py` comment).

## Capability layer (Phase 2)

The plugin now exposes sandbox-native operations on top of the Hermes
`execute()` contract. Hermes' own terminal/read/write/patch tools keep going
through `execute()`; these are additional, first-class capabilities
(`AgentSandboxEnvironment` methods).

| Capability | API | Transport |
|---|---|---|
| Write a file | `write_file(path, content, create_parents=False)` | `PUT /v1/files/{path}` (Router, v2 scoped token, `application/octet-stream`) |
| Read a file | `read_file(path, max_bytes=None)` | `GET /v1/files/{path}` (Router, v2 scoped token) |
| List a directory | `list_dir(path, max_entries=None)` | `GET /v1/files/{path}` → sandboxd `DirectoryListing` JSON |
| stdin | `execute(cmd, stdin_data=…)` | gRPC `Start` + `WriteStdin` frames (+ EOF), file-redirect fallback |
| Export | `export_artifact(paths, ttl_hours=None, max_bytes=None)` | `tar -czf` in the guest → `GET /v1/files` → gateway PVC |
| Import | `import_artifact(id)` | gateway PVC → `PUT /v1/files` → `tar -xzf` into /workspace |
| Checkpoint | `checkpoint()` | export of `/workspace` minus `.hermes` scratch |
| Restore | `restore_checkpoint(id, fresh=True)` | import into a FRESH claim |
| Background process | `start_process(cmd, cwd="")`, `process_logs(id, tail_bytes=None)`, `stop_process(id)`, `list_processes()` | gRPC `Start` stream drained by a reader thread; `SendSignal` to stop |
| Admission control | `queue_stats()` | bounded session FIFO around every command submission |

### Transport decisions and the evidence behind them

1. **File writes use the real upload path, not base64-over-exec.** sandboxd's
   FilesystemService is `GET`/`HEAD`/`PUT`/`DELETE /v1/files/{path}`
   (`packages/sandboxd/pkg/server/filesystem.go` at the shipped commit
   `9a85153590e54cb980f3241f9e7a9228449412c9`, pinned by
   `hermes-sandbox-runtime/Dockerfile`): `PUT` streams the raw body into an
   atomic temp+rename write, creates parents and answers 204; `GET` on a
   directory returns `DirectoryListing{path, entries[]}`
   (`packages/sandboxd/pkg/server/filesystem_types.go`). The same surface is
   documented in `packages/sandboxd/USER_GUIDE.md`, and the plugin's
   Router-authenticated PUT/DELETE path is already exercised by
   `tests/integration/test_live_sandbox.py::test_file_roundtrip_through_authenticated_router`.
   Base64-over-exec would inflate every payload by 4/3 and cap writes at the
   gRPC message limit, so it is not used.
2. **stdin uses the native streaming RPC.** `ProcessService.Execute` has no
   stdin field at all (`ExecuteRequest = {config}`); `WriteStdin{process_id,
   input, eof}` addresses a `process_id` that only the streaming `Start` RPC
   creates, and `Start` allocates a stdin pipe for the non-PTY path
   (`packages/sandboxd/pkg/server/process.go`). `AGENT_SANDBOX_STDIN_MODE`
   selects `auto` (default: native, fall back on gRPC `UNIMPLEMENTED` only),
   `native` (pin) or `file`. The file fallback writes the payload into
   `/workspace/.hermes/stdin/<id>`, runs `{ exec 0< <file>; <cmd>; }` and
   deletes the payload afterwards — including when the command fails.
3. **Background processes use the native streaming RPC, not `nohup` + log
   file.** `Start` is a server-streaming RPC that emits `InitEvent` →
   `stdout`/`stderr` chunks → `ExitEvent{exit_code}` and `SendSignal`
   (`INT`/`TERM`/`KILL`) signals the process *group*; the runtime is pinned to
   a commit where both exist, so the fallback would be strictly worse: a
   `nohup` log file would live in `/workspace` (polluting checkpoints and the
   model's view), exit codes would be lost and pids would go stale. The trade
   is one reader thread plus a byte-bounded ring buffer per process, both
   capped by config (`AGENT_SANDBOX_PROCESS_LOG_BUFFER_BYTES`).
   Draining is mandatory: sandboxd sends stream events synchronously, so a
   client that stops reading eventually blocks the remote process on a full
   pipe.
4. **Artifacts are tarred inside the guest and pulled over REST.** The exec
   channel returns one gRPC message (4 MiB default) and decodes stdout as
   UTF-8, so a binary tarball cannot travel over it; `GET /v1/files` streams a
   file with no server-side size cap (`http.ServeContent`). The tarball is
   written to `/workspace/.hermes/artifacts/` — scratch must live under the
   sandbox root because sandboxd refuses any path outside `/workspace` with
   403, so `/tmp` is not usable for a REST transfer — sized with `stat` before
   the download, then deleted in the guest.

### Path confinement

Two layers, deliberately redundant (`files.py`):

- **Enforcement (server side).** sandboxd resolves symlinks and refuses
  anything outside its `--root-dir` with `403 PERMISSION_DENIED`
  (`packages/sandboxd/pkg/pathutil/sandbox.go`, symlink-aware, nearest
  existing ancestor for not-yet-existing targets). A 403 is surfaced to
  callers as a typed `SandboxPathError`, not a bare transport failure.
- **Policy (client side).** Every path is normalized lexically (rejecting
  `..` traversal, absolute paths outside the root, `~`, NUL, and the
  `/workspace-evil` prefix trap), then resolved *inside the guest* with
  `realpath -m` + `test -f/-d/-L` in one exec probe. Reads require a regular
  file, listings require a directory, missing parents are an explicit error
  (`create_parents=True` opts into creation), and a guest without
  `realpath`/`stat` fails loudly instead of degrading to a lexical-only check.
  A TOCTOU window between probe and request remains; the server-side layer is
  what makes the guarantee hold.

The plugin's own scratch namespace `/workspace/.hermes/` (stdin payloads,
artifact tarballs) is never addressable through the public file API and is
excluded from exports and checkpoints.

### Limits and knobs

All `AGENT_SANDBOX_*` knobs are read in `config.py` (`from_env()`) and validated
there; the defaults are documented next to each field.

| Knob | Default | Meaning |
|---|---|---|
| `AGENT_SANDBOX_FILE_WRITE_MAX_BYTES` | 8 MiB | Refuse a larger write before any request |
| `AGENT_SANDBOX_FILE_READ_MAX_BYTES` | 8 MiB | Refuse a larger read from the probe's `stat` |
| `AGENT_SANDBOX_STDIN_MODE` | `auto` | `auto` \| `native` \| `file` (see above) |
| `AGENT_SANDBOX_STDIN_MAX_BYTES` | 4 MiB | stdin payload cap |
| `AGENT_SANDBOX_ARTIFACT_ROOT` | `/opt/data/agent-sandbox/artifacts` | Gateway PVC path (container rootfs is read-only) |
| `AGENT_SANDBOX_ARTIFACT_DEFAULT_TTL_HOURS` | 24 | TTL when a caller does not pass one |
| `AGENT_SANDBOX_ARTIFACT_MAX_TTL_HOURS` | 168 | Ceiling; a larger `ttl_hours` is refused |
| `AGENT_SANDBOX_ARTIFACT_MAX_BYTES` | 64 MiB | Export and import cap (import body is held in memory) |
| `AGENT_SANDBOX_QUEUE_MAX_CONCURRENT` | 4 | Commands allowed to run at once per session |
| `AGENT_SANDBOX_QUEUE_MAX_QUEUED` | 8 | Waiters before submissions are rejected (`0` = fail fast) |
| `AGENT_SANDBOX_QUEUE_TIMEOUT_SECONDS` | 60 | Max queue wait (execution time is separate) |
| `AGENT_SANDBOX_PROCESS_MAX_CONCURRENT` | 4 | Background processes per session |
| `AGENT_SANDBOX_PROCESS_LOG_BUFFER_BYTES` | 1 MiB | Per-stream ring buffer (older bytes are dropped) |
| `AGENT_SANDBOX_PROCESS_LOG_TAIL_BYTES` | 64 KiB | Default tail returned by `process_logs` |
| `AGENT_SANDBOX_PROCESS_STOP_GRACE_SECONDS` | 5 | Wait after TERM before KILL, and after KILL before cancel |
| `AGENT_SANDBOX_PROCESS_START_TIMEOUT_SECONDS` | 30 | Max wait for a `Start` stream's `InitEvent` |

Artifact ids are opaque 32-hex strings with a JSON sidecar next to the tarball
(`id`, bytes, created/expires, sha256, paths, session, kind). Expired artifacts
and orphan sidecars are swept on every export, so the shared PVC cannot fill
with artifacts nobody will read again. Import verifies the sha256 and refuses
absolute or `..`-bearing archive members before extracting.

### Profiles

`config.py` holds the only profile→`SandboxTemplate` mapping. Only profiles
with a shipped template are selectable (`core` → `hermes-core`, `offline` →
`hermes-offline`, `go` → `hermes-go`, the retained legacy default); `python`,
`node` and `web` are listed in `PROFILE_PLANNED` and raise a
"planned but has no SandboxTemplate yet" `ConfigError`, because a session that
selected them would otherwise fail at claim creation (or get an image without
the toolchain). The plugin selects a template by NAME only and never supplies
image, runtimeClass, namespace, serviceAccount, mounts or network policy.

## Operation notes

- The Router token file (`AGENT_SANDBOX_ROUTER_TOKEN_FILE`) holds the raw
  32-byte Ed25519 seed whose public key lives in
  `agent-sandbox/router-auth-keys.yaml` (`kid: hermes-1`). Rotation: new
  seed → new public key there → roll the Router deployment → update the
  `hermes` Secret.
- Commands run as `/bin/sh -c <cmd>` with cwd confined to `/workspace`;
  output capped at 1 MiB with the truncated marker; timeouts cancel the gRPC
  call and return 124 (Hermes contract); claims are deleted on cleanup and
  orphan-reconciled past their shutdown deadline. `cleanup()` also stops
  background processes (TERM → grace → KILL → stream cancel) before deleting
  the claim.
- The plain workspace is DISPOSABLE: it is an emptyDir destroyed with the
  claim (idle recycle, `AGENT_SANDBOX_IDLE_TIMEOUT_SECONDS`, or claim expiry at
  `AGENT_SANDBOX_MAX_LIFETIME_SECONDS`). Work that must survive has to be
  `checkpoint()`ed first; `restore_checkpoint()` then re-imports it into a
  fresh claim.
- The gRPC channel is plaintext and only ever dials the adopted pod IP on
  9090 — never the Kubernetes API.

## Tests

- `tests/unit/` — offline suite (Hermes ABC stubbed via `tests/_hermes_stub.py`;
  real-import conformance runs in the built image with `hermes plugins compat`).
  The capability layer is covered by table-driven tests over an in-process
  guest emulator (`tests/unit/_guest.py`) that models sandboxd's probe
  protocol, its symlink-aware REST confinement and real tarballs:
  `test_paths.py` (confinement matrix), `test_files.py` (write/read/list, caps,
  missing parents), `test_stdin.py` (native `cat` round-trip over real
  protobuf frames + file fallback), `test_rest_transport.py` (real HTTP PUT/
  GET/DELETE with the scoped token inspected), `test_queue.py` (FIFO bounds),
  `test_artifacts.py` (export/import, TTL, caps, member validation,
  checkpoints), `test_processes.py` (streaming background exec).
- `tests/integration/` — live-cluster suite; run via
  `hack/hermes-plugin-integration.sh` (requires kubeconfig-home.yaml, a
  seeded router keypair, and a port-forwarded Router). Exec assertions are
  gated behind `AGENT_SANDBOX_IN_CLUSTER=1` (sandbox ingress admits only
  in-cluster namespaces on gRPC); the Phase-7 e2e runs them from the Hermes
  namespace.
- Not yet covered by a live run (cluster-dependent): Router PUT/DELETE against
  the real sandboxd, native stdin/`WriteStdin` against the real sandboxd,
  background-process streaming and signals, and artifact export/import through
  `hack/hermes-e2e.sh` (planned as Unit 2.7).
