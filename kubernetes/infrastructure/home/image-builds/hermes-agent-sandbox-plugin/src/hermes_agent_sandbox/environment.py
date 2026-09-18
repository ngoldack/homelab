"""Hermes execution environment for the agent_sandbox backend.

Duck-typed against ``tools.environments.base.BaseEnvironment`` (Hermes passes
a duck type, not a subclass, so we avoid importing any in-tree environment
module). The contract we honor is the one Hermes actually calls:

* ``execute(command, cwd="", *, timeout=None, stdin_data=None, ...) -> {
  "output": str, "returncode": int }``
* ``cleanup()`` / ``close()`` — release the sandbox claim.
* ``fetch_file()`` / ``fetch_realpath()`` — base64-over-exec, matching the
  base class's own implementation so Hermes file tools keep working.

Every command runs as one remote ``/bin/sh -c`` in the sandbox's /workspace.

CAPABILITY LAYER (Phase 2) adds first-class sandbox operations on top of that
contract; Hermes' own tools keep going through ``execute``, while these give
native file writes/reads, real stdin, artifacts/checkpoints, a bounded session
queue and background processes:

* ``write_file`` / ``read_file`` / ``list_dir`` — Router REST (PUT/GET) with
  scoped tokens, /workspace-confined (see ``files.py`` for the transport
  evidence and the path contract).
* ``execute(..., stdin_data=...)`` — native ``Start`` + ``WriteStdin`` frames,
  with a workspace-temp-file fallback for a runtime that lacks them.
* ``export_artifact`` / ``import_artifact`` / ``checkpoint`` /
  ``restore_checkpoint`` — tarballs produced in the guest and stored on the
  gateway's PVC with an opaque id and a TTL (``artifacts.py``).
* ``start_process`` / ``process_logs`` / ``stop_process`` — native streaming
  background exec with bounded log rings (``processes.py``).
* every command submission passes through a bounded session FIFO
  (``queue.py``), so one session cannot stack unbounded work on its sandbox.
"""

from __future__ import annotations

import logging
import os
import posixpath
import secrets
import shlex
import threading
import time
from pathlib import Path
from typing import Any, Dict, List, Optional

from . import artifacts as artifacts_mod
from . import egress
from . import files as files_mod
from .config import (
    STDIN_AUTO,
    STDIN_NATIVE,
    WORKSPACE,
    AgentSandboxConfig,
)
from .errors import (
    CwdNotAllowedError,
    SandboxCommandError,
    SandboxCreateError,
    SandboxCreateTimeoutError,
    SessionQuarantinedError,
    SandboxFileSizeError,
    SandboxTimeoutError,
    SandboxTransportError,
    SandboxUnsupportedError,
)
from .processes import ProcessRegistry
from .queue import SessionCommandQueue
from .transport import KubeClaimsClient, SandboxTransport, sanitize_claim_name

log = logging.getLogger(__name__)

TRUNCATION_SUFFIX = "\n... [output truncated]"
_WORKSPACE = WORKSPACE

# The warm pool exposes ONE sandbox slot on this node and one gateway process
# serves every Hermes session, so this semaphore is the process-wide capacity
# gate: a second concurrent session fails fast with a retryable error instead
# of racing the first into a second claim (which the pool cannot adopt, and
# which one side would leak).
_MAX_CONCURRENT_SANDBOXES = int(os.environ.get("AGENT_SANDBOX_MAX_CONCURRENT", "1"))
_CAPACITY = threading.Semaphore(_MAX_CONCURRENT_SANDBOXES)
_CAPACITY_EXHAUSTED = (
    "agent_sandbox capacity: 1 active sandbox is supported on this node; "
    "retry after the running session finishes"
)

# Substrings Kyverno's hermes-session-quarantine denial carries: the policy
# name (policies.kyverno.io/title) and the quarantine ledger it reads. An
# admission denial that mentions BOTH is the quarantine mapping condition —
# cheap, and matched on the ERROR TEXT because the SDK raises opaque
# exceptions for admission failures (no typed admission error to inspect).
_QUARANTINE_MARKERS = (
    "hermes-session-quarantine",
    "hermes-quarantine",
)


def _is_quarantine_denial(exc: BaseException) -> bool:
    """True when an admission denial flags the session as quarantined.

    Matched on the error text, case-insensitive, because the python SDK
    surfaces admission failures as opaque exceptions whose only stable
    content is the apiserver's own message. A false match is impossible in
    practice: both markers are this repo's own names, and an unrelated
    failure never mentions them together.
    """
    text = str(exc).lower()
    return all(marker in text for marker in _QUARANTINE_MARKERS)


class AgentSandboxEnvironment:
    """A single task-scoped sandbox; creates its claim lazily on first command."""

    def __init__(
        self,
        config: AgentSandboxConfig,
        *,
        task_id: str = "default",
        cwd: str = "/workspace",
        timeout: int = 300,
        claims: Optional[KubeClaimsClient] = None,
        transport: Optional[SandboxTransport] = None,
    ):
        self.config = config
        self.task_id = task_id
        self.cwd = self._normalize_cwd(cwd)
        self.timeout = timeout
        self._claims = claims or KubeClaimsClient(config)
        self._transport = transport
        self._claim_name: Optional[str] = None
        self._sandbox_name: Optional[str] = None
        self._sandbox_uid: Optional[str] = None
        # Stable session identity (egress guard): minted lazily at the first
        # claim create, stamped as the claim's session-hash label and carried
        # into the proxy token. Survives claim recycles (the SAME session
        # keeps ONE hash across _fresh_claim), so a quarantine by the reaper
        # catches every claim this environment ever created.
        self._session_hash: Optional[str] = None
        self._created_at: Optional[float] = None
        self._last_use: float = time.monotonic()
        self._closed = False
        # Re-entrant: _recycle() calls _ensure_ready() while holding it, and
        # _ensure_ready()/_cleanup() call _teardown_sandbox() inside it.
        self._lock = threading.RLock()
        self._capacity_held = False
        # Session-scoped admission gate for every command submission (one
        # environment == one Hermes session == one sandbox).
        self._queue = SessionCommandQueue(
            max_concurrent=config.queue_max_concurrent,
            max_queued=config.queue_max_queued,
            wait_timeout=config.queue_timeout_seconds,
            name=f"sandbox:{task_id}",
        )
        # Built lazily: both hold the transport, which for a provider-created
        # environment does not exist until the first _ensure_ready().
        self._artifact_store: Optional[artifacts_mod.ArtifactStore] = None
        self._process_registry: Optional[ProcessRegistry] = None

    # ---------------- lifecycle ----------------
    def _ensure_ready(self) -> None:
        """Create the claim (once) and wait for adoption/readiness.

        Serialized by ``self._lock`` so two threads on one environment cannot
        both create a claim, and gated by the process-wide capacity semaphore
        so a second concurrent session fails fast with a retryable error
        instead of racing for the warm pool's single sandbox slot.
        """
        with self._lock:
            if self._closed:
                raise SandboxCommandError("environment is closed")
            if self._claim_name is not None and self._sandbox_name is None:
                # A previous teardown could not delete the claim: retry now so
                # the pool's only slot is not stranded by our own leak.
                self._teardown_sandbox()
                if self._claim_name is not None:
                    raise SandboxCreateError(_CAPACITY_EXHAUSTED)
            if self._claim_name is not None:
                if time.monotonic() - self._last_use > self.config.idle_timeout_seconds:
                    self._recycle()
                return
            if not self._capacity_held:
                if not _CAPACITY.acquire(blocking=False):
                    raise SandboxCreateError(_CAPACITY_EXHAUSTED)
                self._capacity_held = True
            claim_name = sanitize_claim_name(
                self.task_id, self.config.namespace, self.config.template
            )
            try:
                # Stable session identity: minted once per environment, stamped
                # as the workload.hermes.io/session-hash label on the claim
                # (the controller propagates it to the sandbox + pods). The
                # egress reaper keys quarantine + claim deletion on this label
                # and Kyverno's hermes-session-quarantine policy matches it.
                if self._session_hash is None:
                    self._session_hash = egress.mint_session_hash()
                self._claims.create_claim(
                    claim_name,
                    pod_labels={egress.SESSION_HASH_LABEL: self._session_hash},
                )
                self._claim_name = claim_name
                sandbox_name = self._claims.wait_ready(
                    claim_name, self.config.create_timeout_seconds
                )
                self._sandbox_name = sandbox_name
                self._sandbox_uid = self._claims.get_sandbox_uid(sandbox_name)
                pod_ip = self._claims.get_sandbox_ip(sandbox_name)
            except SandboxCreateTimeoutError as exc:
                # Read the controller's own conditions before the claim is
                # deleted; they are the only explanation of a stuck create.
                detail = self._claim_status_detail(claim_name)
                self._teardown_sandbox()
                raise SandboxCreateTimeoutError(
                    f"{exc} (claim {claim_name}: {detail})"
                ) from exc
            except Exception as exc:
                # The admission-denial mapping (Unit 3.4): Kyverno's
                # hermes-session-quarantine policy denies re-claims for a
                # quarantined session; its denial names the policy + the
                # quarantine ConfigMap. Map it to the PERMANENT error type so
                # Hermes never recycles or retries — every retry fails the
                # same way until the quarantine key is cleared.
                if _is_quarantine_denial(exc):
                    self._teardown_sandbox()
                    raise SessionQuarantinedError(
                        egress.quarantine_error_text(self._session_hash or "")
                    ) from exc
                # Never leave a half-adopted claim or a held capacity slot:
                # delete it and reset so a subsequent command re-creates.
                self._teardown_sandbox()
                raise
            self._created_at = time.time()
            if self._transport is None:
                # Provider-created environments carry no transport; build the
                # transport lazily the same way claims are built.
                self._transport = SandboxTransport(
                    self.config, self.config.load_token()
                )
            self._transport.attach(sandbox_name, self._sandbox_uid, pod_ip)
            # Egress identity (Unit 3.5): apply the proxy env for EVERY
            # command on this claim. The token is minted fresh here (expiry =
            # now + TTL, so a recycled claim re-mints with a later expiry);
            # the session hash is stable for the environment's life. An unset
            # HMAC knob submits no env (Cilium still confines the guest).
            self._apply_egress_env()
            log.info(
                "Sandbox %s adopted claim %s (uid=%s, session-hash=%s)",
                sandbox_name, claim_name, self._sandbox_uid, self._session_hash,
            )

    def _apply_egress_env(self) -> None:
        """Set (or clear) the per-process proxy env on the adopted transport."""
        set_env = getattr(self._transport, "set_command_env", None)
        if set_env is None:
            # A transport without env plumbing (tests' guest emulator, future
            # alternate connectors): submit no env rather than crash — the
            # sandboxd ProcessConfig.env_vars field is the only enforcement
            # surface here, and a connector that lacks it cannot deliver env
            # anyway.
            return
        secret = self.config.load_egress_secret()
        if secret is None or self._session_hash is None:
            # A claim without identity submits NO env: the guest's own
            # defaults apply (Cilium confines egress to DNS-only).
            set_env(None)
            return
        env = egress.proxy_env(
            self.config,
            self._session_hash,
            self.config.egress_profile,
            secret,
        )
        set_env(env)

    def _claim_status_detail(self, claim_name: str) -> str:
        """Best-effort ``status.conditions`` digest for a claim that never bound.

        Surfaced in the create-timeout text so the operator sees the
        controller's own reason (Pending / Unschedulable / Insufficient cpu)
        instead of a bare deadline.
        """
        try:
            claim = self._claims.get_claim(claim_name)
        except Exception as exc:  # noqa: BLE001 - diagnostics must not mask the timeout
            return f"conditions unavailable ({type(exc).__name__}: {exc})"
        conditions = ((claim or {}).get("status") or {}).get("conditions") or []
        parts = []
        for cond in conditions:
            if not isinstance(cond, dict):
                continue
            reason = cond.get("reason") or cond.get("type") or "unknown"
            message = cond.get("message") or ""
            parts.append(f"{reason}: {message}".strip().rstrip(":"))
        return "; ".join(parts) if parts else "no status.conditions yet"

    def _recycle(self) -> None:
        """Idle timeout hit: release this claim and force a fresh one next use."""
        log.info("Sandbox idle >%ss; recycling claim", self.config.idle_timeout_seconds)
        self._fresh_claim()

    def _fresh_claim(self) -> None:
        """Drop the current claim and adopt a brand-new one (blocking).

        Used by the idle recycle and by ``restore_checkpoint(fresh=True)``: a
        checkpoint restore must land in a sandbox that has no leftover state, so
        the old guest (and everything running in it) is destroyed first.
        """
        self._teardown_sandbox()
        self._last_use = time.monotonic()
        self._ensure_ready()  # re-create immediately

    def _release_capacity(self) -> None:
        if self._capacity_held:
            self._capacity_held = False
            _CAPACITY.release()

    def _teardown_sandbox(self) -> None:
        """Delete the claim and release transport state (idempotent).

        Claim deletion is the only real kill: the warm pool's
        shutdownPolicy=Delete tears the Kata guest down, which terminates
        every process it hosts. Dropping the transport connection alone
        leaves the remote process running. When the delete fails the claim
        name is kept (so a later teardown or the startup sweep can retry) and
        the capacity slot stays held, because it is still occupied remotely.
        """
        with self._lock:
            claim_name = self._claim_name
            if claim_name is not None:
                try:
                    deleted = self._claims.delete_claim(claim_name)
                except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                    log.error("claim %s delete raised: %s", claim_name, exc)
                    deleted = False
                if deleted is False:
                    log.error(
                        "claim %s could not be deleted; keeping it so a later "
                        "teardown can retry",
                        claim_name,
                    )
                else:
                    self._claim_name = None
            self._sandbox_name = None
            self._sandbox_uid = None
            if self._transport is not None:
                try:
                    self._transport.close()
                except Exception:  # noqa: BLE001
                    pass
                # A recycled claim must not inherit the previous adoption's
                # proxy env: clear it here; the next adoption re-applies with
                # a fresh token. getattr-safe: a transport without env
                # plumbing (test emulators, alternate connectors) submits no
                # env by construction.
                set_env = getattr(self._transport, "set_command_env", None)
                if set_env is not None:
                    set_env(None)

    def _cancel_remote(self) -> None:
        """Cancel the remote process for real: tear the sandbox down so the
        next command lazily creates a fresh claim."""
        self._teardown_sandbox()

    def _delete_claim(self) -> None:
        """Final claim deletion for cleanup(); keeps the name when it fails."""
        with self._lock:
            if self._claim_name is None:
                return
            try:
                deleted = self._claims.delete_claim(self._claim_name)
            except Exception as exc:  # noqa: BLE001 - best-effort cleanup
                log.error("claim %s delete raised: %s", self._claim_name, exc)
                deleted = False
            if deleted is False:
                log.error(
                    "claim %s could not be deleted; leaving it for the orphan "
                    "sweep to reclaim",
                    self._claim_name,
                )
                return
            self._claim_name = None
            self._release_capacity()

    def cleanup(self) -> None:
        """Delete the claim; idempotent and safe to call multiple times."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
            # Order matters: stop background processes (so their streams end
            # deliberately and their final output is recorded) BEFORE the claim
            # delete kills the guest, then release waiters so no thread sits in
            # the queue while the environment is going away.
            if self._process_registry is not None and self._transport is not None:
                try:
                    self._process_registry.stop_all()
                except Exception as exc:  # noqa: BLE001 - teardown must not raise
                    log.warning("stopping background processes failed: %s", exc)
            self._queue.close()
            self._delete_claim()
            if self._transport is not None:
                self._transport.close()

    close = cleanup  # alias Hermes and callers both use

    # ---------------- cwd handling ----------------
    @staticmethod
    def _normalize_cwd(cwd: str) -> str:
        """Resolve *cwd* and require it inside /workspace (symlink-safe)."""
        if not cwd or cwd == "/":
            return _WORKSPACE
        # Expand a leading /workspace prefix; anything else that isn't absolute
        # is rejected right away (a relative host cwd can never be a sandbox cwd).
        if not posixpath.isabs(cwd):
            raise CwdNotAllowedError(f"cwd must be absolute inside {_WORKSPACE}: {cwd!r}")
        resolved = posixpath.realpath(cwd)
        if resolved != _WORKSPACE and not resolved.startswith(_WORKSPACE + "/"):
            raise CwdNotAllowedError(f"cwd {cwd!r} resolves outside {_WORKSPACE}")
        return resolved

    # ---------------- execution ----------------
    def execute(
        self,
        command: str,
        cwd: str = "",
        *,
        timeout: Optional[int] = None,
        stdin_data: Optional[Any] = None,
        **kwargs: Any,
    ) -> Dict[str, Any]:
        """Run one command; unknown kwargs are accepted and ignored (the Hermes
        factory may pass extra keys in future releases — never break for that).

        ``stdin_data`` (str or bytes) is delivered as real stdin. The pinned
        runtime supports it natively (ProcessService Start + WriteStdin, see
        transport.drive_start_with_stdin), and ``AGENT_SANDBOX_STDIN_MODE=auto``
        falls back to a workspace temp file + `exec 0< file` only when the
        sandboxd build answers UNIMPLEMENTED.
        """
        del kwargs  # forward-compat: accept-and-ignore unknown keyword args
        target_cwd = self._normalize_cwd(cwd) if cwd else self.cwd
        self._ensure_ready()
        self._last_use = time.monotonic()
        effective_timeout = self._clamp_timeout(timeout)
        full = f"cd {shlex.quote(target_cwd)} && {command}"
        # No blind recovery here: _do_run already converts transport
        # failures into SandboxCommandError (timeout path cancels + 124
        # THERE). Any exception escaping _do_run is a genuine bug and must
        # propagate so Hermes logs it — swallowing it as 124 with empty
        # output (and tearing the claim down via _cancel_remote) would hide
        # it and destroy a live sandbox mid-task.
        if stdin_data is None:
            stdout, stderr, code = self._run_queued(
                lambda: self._do_run(full, effective_timeout)
            )
        else:
            payload = self._stdin_payload(stdin_data)
            stdout, stderr, code = self._run_with_stdin(full, payload, effective_timeout)
        output = stdout + stderr if stderr and stdout else (stdout or stderr)
        result = {
            "output": self._truncate(output),
            "returncode": code,
            "cwd": target_cwd,
        }
        return result

    def _run_queued(self, fn: Any) -> Any:
        """Run *fn* under the session's bounded FIFO admission gate."""
        return self._queue.submit(fn, wait_timeout=self.config.queue_timeout_seconds)

    def _stdin_payload(self, stdin_data: Any) -> bytes:
        """Normalize and size-check a stdin payload (str -> utf-8 bytes)."""
        if isinstance(stdin_data, bytes):
            payload = stdin_data
        elif isinstance(stdin_data, bytearray):
            payload = bytes(stdin_data)
        elif isinstance(stdin_data, str):
            payload = stdin_data.encode("utf-8")
        else:
            raise SandboxCommandError(
                f"stdin_data must be str or bytes, got {type(stdin_data).__name__}"
            )
        limit = self.config.stdin_max_bytes
        if len(payload) > limit:
            raise SandboxFileSizeError(
                f"stdin payload is {len(payload)} bytes, over the {limit} byte limit "
                "(AGENT_SANDBOX_STDIN_MAX_BYTES); write it to a file instead"
            )
        return payload

    def _run_with_stdin(self, full: str, payload: bytes, timeout: int):
        """Deliver *payload* on stdin, native first when the mode allows it."""
        mode = self.config.stdin_mode
        if mode in (STDIN_NATIVE, STDIN_AUTO):
            try:
                return self._run_queued(
                    lambda: self._do_run_stdin(full, payload, timeout)
                )
            except SandboxUnsupportedError as exc:
                if mode == STDIN_NATIVE:
                    raise
                # Only an UNIMPLEMENTED RPC reaches this branch (see
                # _translate_rpc_error): the runtime, not the request, is
                # missing the capability — so the file fallback is safe. Any
                # other stdin failure propagates untouched.
                log.warning(
                    "native stdin unavailable in this sandboxd build (%s); "
                    "falling back to a workspace temp file",
                    exc,
                )
        return self._run_queued(
            lambda: self._run_stdin_via_file(full, payload, timeout)
        )

    def _do_run_stdin(self, full: str, payload: bytes, timeout: int):
        """Native stdin path, with the same timeout/124 contract as _do_run."""
        try:
            return self._transport.run_with_stdin(full, payload, timeout)
        except SandboxTimeoutError:
            self._cancel_remote()
            return (
                "",
                f"command exceeded {timeout}s and was cancelled; the sandbox "
                "was recycled, so /workspace state is gone — re-run setup if needed",
                124,
            )
        except SandboxCommandError:
            raise
        except Exception as exc:  # noqa: BLE001 - unexpected transport failure
            raise SandboxCommandError(f"sandbox transport error: {exc}") from exc

    def _run_stdin_via_file(self, full: str, payload: bytes, timeout: int):
        """Fallback stdin: write the payload into the workspace, redirect, delete.

        The temp file lives in the plugin scratch namespace (never /tmp, which
        the REST API cannot reach) and is removed in a ``finally`` so a failed
        command cannot leave a payload behind. The command is wrapped in a brace
        group — ``{ exec 0< file; <command>; }`` — because a bare ``< file``
        would redirect only the LAST statement of a multi-statement command, and
        ``exec`` on the group's shell is what makes the redirection apply to all
        of them.
        """
        tmp = files_mod.scratch_file_path(
            "stdin", f"{secrets.token_hex(8)}"
        )
        files_mod.write_scratch_file(self._transport, self.config, tmp, payload)
        wrapped = f"{{ exec 0< {shlex.quote(tmp)}; {full}; }}"
        try:
            return self._do_run(wrapped, timeout)
        finally:
            try:
                self._transport.delete_file(tmp)
            except Exception as exc:  # noqa: BLE001 - cleanup must not mask the result
                log.warning("could not remove stdin scratch file %s: %s", tmp, exc)

    def _clamp_timeout(self, requested: Optional[int]) -> int:
        """Commands are capped at min(hermes timeout, config command timeout)."""
        candidates = [self.config.command_timeout_seconds]
        if requested and requested > 0:
            candidates.append(int(requested))
        if self.timeout and self.timeout > 0:
            candidates.append(int(self.timeout))
        return max(1, min(candidates))

    def _do_run(self, full: str, timeout: int):
        """Run through the transport; a real remote deadline recycles the sandbox.

        Only a classified :class:`SandboxTimeoutError` (gRPC
        DEADLINE_EXCEEDED) becomes the 124 result — the old wall-clock
        heuristic misreported any slow failure as a timeout and returned the
        raw exception text. Every other failure is a command error and must
        not tear a live sandbox down.
        """
        try:
            return self._transport.run(full, timeout)
        except SandboxTimeoutError:
            self._cancel_remote()
            return (
                "",
                f"command exceeded {timeout}s and was cancelled; the sandbox "
                "was recycled, so /workspace state is gone — re-run setup if needed",
                124,
            )
        except SandboxCommandError:
            raise
        except Exception as exc:  # noqa: BLE001 - unexpected transport failure
            raise SandboxCommandError(f"sandbox transport error: {exc}") from exc

    # ---------------- output cap ----------------
    def _truncate(self, output: str) -> str:
        limit = self.config.output_limit_bytes
        if len(output) <= limit:
            return output
        keep = limit - len(TRUNCATION_SUFFIX)
        keep = max(0, keep)
        return output[:keep] + TRUNCATION_SUFFIX

    # ---------------- file helpers (base-compatible) ----------------
    def fetch_file(
        self, remote_path: str, local_dest: Path, *, max_bytes: int
    ) -> None:
        """Copy a file out of the sandbox through the authenticated Router
        (GET /v1/files with a per-request v2 scoped token; sandboxd REST)."""
        self._ensure_ready()  # builds transport + adopts claim if first op
        try:
            data = self._transport.fetch_file(remote_path)
        except SandboxTransportError as exc:
            raise SandboxCommandError(
                f"could not read {remote_path!r} in the sandbox: {exc}"
            ) from exc
        if len(data) > max_bytes:
            raise SandboxCommandError(
                f"{remote_path!r} exceeds the {max_bytes // 1024} KB delivery limit"
            )
        Path(local_dest).write_bytes(data)

    def fetch_realpath(self, remote_path: str) -> Optional[str]:
        result = self.execute(
            f"readlink -f {shlex.quote(remote_path)} 2>/dev/null",
        )
        if int(result.get("returncode") or 0) != 0:
            return None
        for ln in reversed((result.get("output") or "").splitlines()):
            if ln.strip().startswith("/"):
                return ln.strip()
        return None

    def get_temp_dir(self) -> str:
        return "/tmp"

    # ---------------- capability layer properties ----------------
    @property
    def artifacts(self) -> artifacts_mod.ArtifactStore:
        """Gateway-side artifact store (created on first use)."""
        if self._artifact_store is None:
            self._artifact_store = artifacts_mod.ArtifactStore(self.config.artifact_root)
        return self._artifact_store

    @property
    def _processes(self) -> ProcessRegistry:
        """Session-scoped background-process registry (created on first use).

        Holds ``self._transport`` by reference: the transport OBJECT is stable
        across recycles (attach() rebuilds its connector), so a registry built
        once keeps working.
        """
        if self._process_registry is None:
            self._process_registry = ProcessRegistry(
                self._transport,
                max_processes=self.config.process_max_concurrent,
                log_buffer_bytes=self.config.process_log_buffer_bytes,
                log_tail_bytes=self.config.process_log_tail_bytes,
                stop_grace_seconds=self.config.process_stop_grace_seconds,
            )
        return self._process_registry

    def queue_stats(self) -> Dict[str, Any]:
        """Admission-gate counters for one session (diagnostics)."""
        return self._queue.stats()

    # ---------------- file operations (Unit 2.1) ----------------
    def write_file(
        self,
        path: str,
        content: Any,
        *,
        create_parents: bool = False,
        timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Write *content* (str|bytes) to a /workspace path.

        Content-oriented signature per the capability-layer contract: the model
        supplies BYTES and a guest path, and the plugin owns the transport
        (Router PUT). Hermes' own read/write/patch tools are unaffected — they
        reach the sandbox through ``execute()`` (see the module docstring), so
        this name does not intercept the environment protocol's host-side file
        handling.
        """
        self._ensure_ready()
        self._last_use = time.monotonic()
        return files_mod.write_file(
            self._transport,
            self.config,
            path,
            content,
            create_parents=create_parents,
            timeout=timeout,
        )

    def read_file(
        self,
        path: str,
        *,
        max_bytes: Optional[int] = None,
        encoding: str = "utf-8",
        timeout: Optional[int] = None,
    ) -> str:
        """Read a regular file from the sandbox as text (size-capped)."""
        self._ensure_ready()
        self._last_use = time.monotonic()
        result = files_mod.read_file(
            self._transport,
            self.config,
            path,
            max_bytes=max_bytes,
            encoding=encoding,
            timeout=timeout,
        )
        return result["content"]

    def list_dir(
        self,
        path: str = WORKSPACE,
        *,
        max_entries: Optional[int] = None,
        timeout: Optional[int] = None,
    ) -> List[Dict[str, Any]]:
        """List a workspace directory (sandboxd DirectoryListing)."""
        self._ensure_ready()
        self._last_use = time.monotonic()
        kwargs = {"timeout": timeout}
        if max_entries is not None:
            kwargs["max_entries"] = int(max_entries)
        result = files_mod.list_dir(self._transport, self.config, path, **kwargs)
        return result["entries"]

    # ---------------- artifacts + checkpoints (Units 2.3 + 2.6) ----------------
    def export_artifact(
        self,
        paths: Any,
        *,
        ttl_hours: Optional[float] = None,
        max_bytes: Optional[int] = None,
        exclude: Any = (),
        timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Tar *paths* inside the sandbox and store the tarball with a TTL."""
        self._ensure_ready()
        self._last_use = time.monotonic()
        ref = self.artifacts.export(
            self._transport,
            self.config,
            list(paths) if not isinstance(paths, str) else [paths],
            exclude=list(exclude),
            ttl_hours=ttl_hours,
            max_bytes=max_bytes,
            session=self.task_id,
            timeout=timeout,
        )
        return ref.as_dict()

    def import_artifact(
        self,
        artifact_id: str,
        *,
        fresh: bool = False,
        timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Restore a stored artifact's tree into this sandbox's /workspace.

        ``fresh=True`` destroys the current guest first and adopts a new claim,
        so the restored tree cannot be mixed with leftovers of the previous
        session (the point of ``restore_checkpoint``'s default).
        """
        if fresh:
            self._fresh_claim()
        else:
            self._ensure_ready()
        self._last_use = time.monotonic()
        ref = self.artifacts.import_artifact(
            self._transport, self.config, artifact_id, timeout=timeout
        )
        return ref.as_dict()

    def list_artifacts(self) -> List[Dict[str, Any]]:
        """Every unexpired artifact this gateway holds (newest first)."""
        return [ref.as_dict() for ref in self.artifacts.list_artifacts()]

    def checkpoint(
        self,
        *,
        ttl_hours: Optional[float] = None,
        max_bytes: Optional[int] = None,
        timeout: Optional[int] = None,
    ) -> Dict[str, Any]:
        """Snapshot /workspace (minus internal scratch) as a checkpoint artifact.

        The plain workspace stays disposable: it lives on an emptyDir and dies
        with the claim, so a session that needs its files after a recycle or a
        claim expiry must checkpoint first.
        """
        self._ensure_ready()
        self._last_use = time.monotonic()
        ref = artifacts_mod.checkpoint(
            self.artifacts,
            self._transport,
            self.config,
            session=self.task_id,
            ttl_hours=ttl_hours,
            max_bytes=max_bytes,
            timeout=timeout,
        )
        return ref.as_dict()

    def restore_checkpoint(
        self, artifact_id: str, *, fresh: bool = True, timeout: Optional[int] = None
    ) -> Dict[str, Any]:
        """Import a checkpoint into a FRESH claim (default) or the current one.

        ``fresh=True`` destroys the current guest first, so the restored tree
        cannot be mixed with leftovers of the previous session — the point of a
        checkpoint is a known workspace, and a merge would silently keep stale
        files that the snapshot no longer contains.
        """
        ref = self.import_artifact(artifact_id, fresh=fresh, timeout=timeout)
        if ref["kind"] != "checkpoint":
            log.warning(
                "restore_checkpoint restored artifact %s of kind %r",
                artifact_id,
                ref["kind"],
            )
        return ref

    # ---------------- background processes (Unit 2.5) ----------------
    def start_process(self, command: str, *, cwd: str = "") -> Dict[str, Any]:
        """Start *command* in the background; returns the process handle.

        The command is a single ``/bin/sh -c`` string, like ``execute``; state
        is NOT carried over from a previous command (no `cd` persistence), so
        the caller must quote a cwd explicitly when it matters.
        """
        self._ensure_ready()
        self._last_use = time.monotonic()
        target_cwd = self._normalize_cwd(cwd) if cwd else self.cwd
        full = f"cd {shlex.quote(target_cwd)} && {command}"
        handle = self._processes.start(
            full, start_timeout=self.config.process_start_timeout_seconds
        )
        return handle.as_dict()

    def process_logs(
        self, process_id: str, *, tail_bytes: Optional[int] = None
    ) -> Dict[str, Any]:
        """Return a process' current state plus the tail of its output."""
        tail = int(tail_bytes) if tail_bytes else self.config.process_log_tail_bytes
        return self._processes.get(process_id).logs(tail)

    def stop_process(self, process_id: str) -> Dict[str, Any]:
        """Stop a background process (TERM -> grace -> KILL -> stream cancel)."""
        return self._processes.stop(process_id)

    def list_processes(self) -> List[Dict[str, Any]]:
        """Every background process this session has started."""
        if self._process_registry is None:
            return []
        return self._process_registry.list()