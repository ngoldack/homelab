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
import posixpath
import shlex
import time
from pathlib import Path
from typing import Any, Dict, Optional

from .config import AgentSandboxConfig
from .errors import (
    CwdNotAllowedError,
    SandboxCommandError,
    SandboxTransportError,
)
from .redaction import sanitize_env
from .transport import KubeClaimsClient, SandboxTransport, sanitize_claim_name

log = logging.getLogger(__name__)

TRUNCATION_SUFFIX = "\n... [output truncated]"
_WORKSPACE = "/workspace"


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

    # ---------------- lifecycle ----------------
    def _ensure_ready(self) -> None:
        """Create the claim (once) and wait for adoption/readiness."""
        if self._closed:
            raise SandboxCommandError("environment is closed")
        if self._claim_name is not None:
            if time.monotonic() - self._last_use > self.config.idle_timeout_seconds:
                self._recycle()
            return
        claim_name = sanitize_claim_name(
            self.task_id, self.config.namespace, self.config.template
        )
        self._claims.create_claim(claim_name)
        self._claim_name = claim_name
        try:
            sandbox_name = self._claims.wait_ready(
                claim_name, self.config.create_timeout_seconds
            )
            self._sandbox_name = sandbox_name
            self._sandbox_uid = self._claims.get_sandbox_uid(sandbox_name)
            pod_ip = self._claims.get_sandbox_ip(sandbox_name)
        except Exception:
            # Never leave a half-adopted claim: delete it and reset so a
            # subsequent command lazily re-creates cleanly.
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

    def _recycle(self) -> None:
        """Idle timeout hit: release this claim and force a fresh one next use."""
        log.info("Sandbox idle >%ss; recycling claim", self.config.idle_timeout_seconds)
        self._teardown_sandbox()
        self._last_use = time.monotonic()
        self._ensure_ready()  # re-create immediately

    def _teardown_sandbox(self) -> None:
        """Delete the claim and release transport state (idempotent).

        Claim deletion is the only real kill: the warm pool's
        shutdownPolicy=Delete tears the Kata guest down, which terminates
        every process it hosts. Dropping the transport connection alone
        leaves the remote process running.
        """
        if self._claim_name is not None:
            try:
                self._claims.delete_claim(self._claim_name)
            except Exception:  # noqa: BLE001 - best-effort cleanup
                pass
        self._claim_name = None
        self._sandbox_name = None
        self._sandbox_uid = None
        if self._transport is not None:
            try:
                self._transport.close()
            except Exception:  # noqa: BLE001
                pass

    def _cancel_remote(self) -> None:
        """Cancel the remote process for real: tear the sandbox down so the
        next command lazily creates a fresh claim."""
        self._teardown_sandbox()

    def _delete_claim(self) -> None:
        if self._claim_name is not None:
            self._claims.delete_claim(self._claim_name)
            self._claim_name = None

    def cleanup(self) -> None:
        """Delete the claim; idempotent and safe to call multiple times."""
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
        try:
            stdout, stderr, code = self._do_run(full, effective_timeout)
        except SandboxCommandError:
            raise
        except Exception as exc:  # noqa: BLE001 - transport -> recoverable 124
            log.warning("sandbox exec failed: %s", exc)
            self._cancel_remote()
            return self._timeout_result("")
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
        """Run through the transport; on timeout cancel the remote process first."""
        started = time.monotonic()
        try:
            return self._transport.run(full, timeout)
        except Exception as exc:  # noqa: BLE001 - treat transport failure as timeout-124
            if time.monotonic() - started >= timeout:
                self._cancel_remote()
                return "", str(exc), 124
            raise SandboxCommandError(f"sandbox transport error: {exc}") from exc

    def _cancel_remote(self) -> None:
        """Cancel the remote process for real: tear the sandbox down so the
        next command lazily creates a fresh claim."""
        self._teardown_sandbox()

    def _timeout_result(self, partial: str) -> Dict[str, Any]:
        return {"output": self._truncate(partial), "returncode": 124, "cwd": self.cwd}

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