"""Hermes terminal-environment provider for the agent_sandbox backend.

Subclasses the real ``TerminalEnvironmentProvider`` ABC from Hermes
(``agent.terminal_env_provider`` — the installed distribution's top-level
layout at v2026.9.7 / 0.21.1: the editable install exposes ``agent``,
``hermes_cli``, ``gateway``, ``utils`` as top-level packages; verified
inside the built image).

Classification is the most conservative isolated set:

* ``is_remote`` / ``is_container`` — commands run off-host in a Kata guest.
* ``session_isolated_when_nonpersistent`` — non-persistent sessions get their
  own sandbox so two ephemeral runs can't destroy each other's claim.
* ``skip_container_guards`` — disposable, short-lived sandbox.
* ``strip_env_keys`` — every credential-shaped env name present in the gateway
  process, so a model-authored command can never read one.
"""

from __future__ import annotations

import logging
import urllib.error
import urllib.request
from typing import Any, Dict, List, Optional, Tuple

from agent.terminal_env_provider import TerminalEnvironmentProvider

from . import redaction
from .config import from_env
from .environment import AgentSandboxEnvironment
from .errors import AgentSandboxError, ConfigError
from .transport import KubeClaimsClient

log = logging.getLogger(__name__)

BACKEND_NAME = "agent_sandbox"

# One startup orphan sweep per process (see _sweep_orphans_once).
_reconciled = False

# One truthful, bounded description of the execution environment. It is
# rendered into every new session prompt (env_description + system-prompt
# section) so the model does not hallucinate network or toolchain
# availability. VERIFIED FACTS: the sandbox's toFQDNs egress rules are
# inert in this cluster (DNS proxy transparent mode off), so the guest has
# DNS-only egress; the Go toolchain lives in the hermes-sandbox-runtime
# image at /usr/local/go (1.26.5); /workspace and /tmp are emptyDir mounts.
SANDBOX_ENV_FACTS = (
    "terminal commands run in a Kubernetes Kata sandbox: Debian bookworm "
    "guest with Go 1.26.5 at /usr/local/go/bin (on PATH), git, curl, jq, "
    "ripgrep. Writable directories: /workspace (the project root, recreated "
    "per session — state does not survive sandbox recycle) and /tmp "
    "(HOME=/tmp, so toolchain caches work). Network: DNS only — there is NO "
    "external network egress from the sandbox; package/module downloads "
    "(PyPI, Go proxy, GitHub) fail unless the operator later allows them. "
    "Kubernetes, Talos, Proxmox and all internal services are unreachable. "
    "Use existing dependencies and local files; do not attempt downloads."
)


def _sweep_orphans_once() -> None:
    """Delete claims a previous gateway instance left behind, exactly once.

    An orphan claim holds the warm pool's single sandbox slot until its
    shutdownTime (~4 h), so the sweep must run at startup — and must never
    prevent the backend from registering, hence the blanket catch.
    """
    global _reconciled
    if _reconciled:
        return
    _reconciled = True
    try:
        cfg = from_env()
        deleted = KubeClaimsClient(cfg).reconcile_orphans(cfg.resolve_gateway_id())
        if deleted:
            log.info("agent_sandbox startup sweep reclaimed %d orphan claim(s)", len(deleted))
    except Exception as exc:  # noqa: BLE001 - startup must never fail on cleanup
        log.warning("agent_sandbox orphan reconcile at startup failed: %s", exc)


def register(ctx) -> None:
    """Hermes plugin entry-point contract (plugins_loader.py): the loader
    imports the entry-point MODULE and calls ``register(ctx)`` on it —
    the module-level function, not the class method."""
    ctx.register_terminal_environment_provider(AgentSandboxProvider())
    ctx.register_system_prompt_section(
        "agent_sandbox_env",
        SANDBOX_ENV_FACTS,
        position="after_memory",
    )
    _sweep_orphans_once()


class AgentSandboxProvider(TerminalEnvironmentProvider):
    """`terminal.backend: agent_sandbox` — run commands in a Kubernetes Agent
    Sandbox (Kata) pod via hermes-go warm pool, through the authenticated Router.
    """

    name: str = BACKEND_NAME

    is_remote: bool = True
    is_container: bool = True
    session_isolated_when_nonpersistent: bool = True

    @property
    def display_name(self) -> str:
        return "Agent Sandbox (Kubernetes Kata)"

    def env_description(self) -> str:
        """Injected into the session so the model knows what the sandbox is
        and what to expect (consumed by agent/prompt_builder.py)."""
        return SANDBOX_ENV_FACTS

    @property
    def skip_container_guards(self) -> bool:
        # Sandbox is disposable and network-isolated; commands need no
        # dangerous-command approval prompts beyond Hermes' own guardrails.
        return True

    @property
    def strip_env_keys(self) -> frozenset:
        # Keep Hermes from ever forwarding a credential/near-credential
        # variable into a model-authored subprocess (classification attr).
        return redaction.strip_env_names()

    # ---- availability (cheap, no network; runs during UI paints) ----
    def _config_or_none(self) -> Optional[Any]:
        try:
            return from_env()
        except ConfigError:
            return None

    def is_available(self) -> bool:
        """True when Router URL is set and the token file is readable.

        Never touches the network here: the picker calls this on every paint.
        """
        cfg = self._config_or_none()
        if cfg is None:
            return False
        return cfg.token_file_readable()

    def check_requirements(self, config: Dict[str, Any]) -> bool:
        ok = self.is_available()
        if not ok:
            log.error(
                "agent_sandbox backend requires AGENT_SANDBOX_ROUTER_URL and a readable "
                "AGENT_SANDBOX_ROUTER_TOKEN_FILE"
            )
        return ok

    # ---- creation ----
    def create_environment(
        self,
        *,
        cwd: str,
        timeout: int,
        task_id: str = "default",
        image: Optional[str] = None,
        container_config: Optional[Dict[str, Any]] = None,
        **kwargs: Any,
    ) -> AgentSandboxEnvironment:
        """Create a task-scoped agent_sandbox environment.

        ``image`` and ``container_config`` are accepted for forward-compat and
        ignored: the backend always targets the hardcoded hermes-go template in
        hermes-sandbox (never addressable by callers). Unknown kwargs are
        ignored per the ABC contract so the factory can evolve.
        """
        del image, container_config, kwargs  # allowlisted template only
        cfg = from_env()
        return AgentSandboxEnvironment(cfg, task_id=task_id, cwd=cwd, timeout=timeout)

    # ---- registration (entry-point module contract) ----
    def register(self, ctx) -> None:  # type: ignore[override]
        ctx.register_terminal_environment_provider(self)

    # ---- doctor ----
    def doctor_checks(self) -> List[Tuple[bool, str, str]]:
        """hermes doctor rows: config, token seed size, then router reachability+auth."""
        cfg = self._config_or_none()
        if cfg is None:
            return [
                (False, "agent_sandbox config", "AGENT_SANDBOX_ROUTER_URL/TOKEN_FILE missing")
            ]
        token_ok = cfg.token_file_readable()
        rows: List[Tuple[bool, str, str]] = [
            (True, "agent_sandbox config", "router URL set"),
            (
                token_ok,
                "agent_sandbox token file (32 B Ed25519 seed)",
                f"{cfg.token_file}: 32-byte seed readable"
                if token_ok
                else (
                    f"{cfg.token_file}: expected exactly 32 bytes (is the Secret "
                    "using stringData/base64?)"
                ),
            ),
        ]
        if not token_ok:
            return rows + [
                (False, "agent_sandbox router reachability", "not probed: token file invalid"),
                (False, "agent_sandbox router auth", "not probed: token file invalid"),
            ]
        reach, detail = self._probe_router(cfg)
        rows.append((reach, "agent_sandbox router reachability", detail))
        rows.append((reach, "agent_sandbox router auth", detail))
        return rows

    def _probe_router(self, cfg) -> Tuple[bool, str]:
        """Bounded, unauthenticated reachability probe; 401/403 prove auth wiring."""
        url = cfg.router_url.rstrip("/") + "/healthz"
        try:
            req = urllib.request.Request(url, method="GET")
            with urllib.request.urlopen(req, timeout=3) as resp:
                return (resp.status < 500, f"HTTP {resp.status}")
        except urllib.error.HTTPError as exc:
            # 401/403 mean the router is up and enforcing auth.
            if exc.code in (401, 403):
                return (True, f"auth enforced (HTTP {exc.code})")
            return (False, f"HTTP {exc.code}")
        except Exception as exc:  # noqa: BLE001
            return (False, str(exc))