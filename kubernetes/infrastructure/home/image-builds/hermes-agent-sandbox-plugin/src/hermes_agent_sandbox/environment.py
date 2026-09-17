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
"""

from __future__ import annotations

import logging
import os
import posixpath
import shlex
import threading
import time
from pathlib import Path
from typing import Any, Dict, Optional

from .config import AgentSandboxConfig
from .errors import (
    CwdNotAllowedError,
    SandboxCommandError,
    SandboxCreateError,
    SandboxCreateTimeoutError,
    SandboxTimeoutError,
    SandboxTransportError,
)
from .transport import KubeClaimsClient, SandboxTransport, sanitize_claim_name

log = logging.getLogger(__name__)

TRUNCATION_SUFFIX = "\n... [output truncated]"
_WORKSPACE = "/workspace"

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
        self._created_at: Optional[float] = None
        self._last_use: float = time.monotonic()
        self._closed = False
        # Re-entrant: _recycle() calls _ensure_ready() while holding it, and
        # _ensure_ready()/_cleanup() call _teardown_sandbox() inside it.
        self._lock = threading.RLock()
        self._capacity_held = False

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
                self._claims.create_claim(claim_name)
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
            except Exception:
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
            log.info(
                "Sandbox %s adopted claim %s (uid=%s)",
                sandbox_name, claim_name, self._sandbox_uid,
            )

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
            if self._claim_name is None:
                self._release_capacity()

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
        stdin_data: Optional[str] = None,
        **kwargs: Any,
    ) -> Dict[str, Any]:
        """Run one command; unknown kwargs are accepted and ignored (the Hermes
        factory may pass extra keys in future releases — never break for that).
        """
        del kwargs  # forward-compat: accept-and-ignore unknown keyword args
        if stdin_data is not None:
            # The python-router HTTP contract has no stdin channel; honest
            # failure beats silently dropping input Hermes expected delivered.
            raise SandboxCommandError(
                "stdin_data is not supported by the agent_sandbox HTTP transport"
            )
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
        stdout, stderr, code = self._do_run(full, effective_timeout)
        output = stdout + stderr if stderr and stdout else (stdout or stderr)
        result = {
            "output": self._truncate(output),
            "returncode": code,
            "cwd": target_cwd,
        }
        return result

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