"""Configuration for the agent_sandbox Hermes terminal backend.

ONLY these AGENT_SANDBOX_* environment variables are read. The namespace and
sandbox template are hardcoded allowlist values, NOT overridable by callers:
Hermes sessions and model-authored commands must never be able to aim the
backend at a different namespace or template than the dedicated,
admission-policed ``hermes-sandbox`` + ``hermes-go`` pair.
"""

from __future__ import annotations

import logging
import socket
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path
from typing import Dict, Optional
from urllib.parse import urlparse

from .errors import ConfigError

log = logging.getLogger(__name__)

# Hardcoded, non-overridable addressable backend identity. Only warm-pool
# adoption against hermes-go inside hermes-sandbox is ever allowed.
NAMESPACE = "hermes-sandbox"
TEMPLATE = "hermes-go"

# Exact size of the Ed25519 seed the Router signs scoped tokens with. The
# documented failure mode is a Secret mounted with `stringData` whose value is
# a base64 string (44 chars for a 32-byte seed): the mount is fine, so only a
# length check catches it before Ed25519.from_private_bytes does.
TOKEN_SEED_BYTES = 32

# Standard claim-name length ceiling and drop-in suffix randomness for the
# DNS-1123-safe claim identifier (see environment.claim).
MAX_CLAIM_NAME_LEN = 63
CLAIM_SUFFIX_BYTES = 6  # 12 hex chars of entropy


class CommandTimeoutMode(Enum):
    """How a timeout is reported to Hermes."""

    RETURN_124 = "return_124"  # partial output + returncode 124 (Hermes contract)


@dataclass(frozen=True)
class AgentSandboxConfig:
    """Validated AGENT_SANDBOX_* settings."""

    router_url: str
    token_file: str
    namespace: str = NAMESPACE
    template: str = TEMPLATE
    create_timeout_seconds: int = 120
    command_timeout_seconds: int = 300
    max_lifetime_seconds: int = 14400
    idle_timeout_seconds: int = 1800
    output_limit_bytes: int = 1048576
    gateway_id: str = ""

    token_file_path: Path = field(init=False, repr=False)
    token_loaded: Optional[bytes] = field(init=False, repr=False, default=None)

    def __post_init__(self) -> None:
        object.__setattr__(self, "token_file_path", Path(self.token_file))
        parsed = urlparse(self.router_url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ConfigError(
                f"AGENT_SANDBOX_ROUTER_URL must be an http(s) URL, got {self.router_url!r}"
            )
        # Integer bounds hold regardless of construction path (from_env parses
        # strings and delegates here); a non-positive value breaks timeouts,
        # truncation, and recycle logic in subtle ways.
        for name in (
            "create_timeout_seconds",
            "command_timeout_seconds",
            "max_lifetime_seconds",
            "idle_timeout_seconds",
            "output_limit_bytes",
        ):
            value = getattr(self, name)
            if not isinstance(value, int) or value <= 0:
                raise ConfigError(
                    f"{name} must be a positive integer, got {value!r}"
                )

    # ---- token filesystem state (no network; used by is_available + doctor) ----
    def token_file_readable(self) -> bool:
        """True when the configured token file holds exactly one 32-byte seed.

        A non-empty check is not enough: a base64 `stringData` mount reads as a
        healthy file and only explodes at the first sandbox file operation.
        """
        try:
            size = len(self.token_file_path.read_bytes().strip())
        except OSError:
            return False
        if size != TOKEN_SEED_BYTES:
            log.error(
                "AGENT_SANDBOX_ROUTER_TOKEN_FILE %s holds %d bytes, expected exactly "
                "%d (an Ed25519 seed): if the Secret uses `stringData`, base64-encode "
                "a 32-byte seed or move the value to `data`",
                self.token_file,
                size,
                TOKEN_SEED_BYTES,
            )
            return False
        return True

    def load_token(self) -> bytes:
        """Read the raw 32-byte Ed25519 seed; strips surrounding whitespace."""
        try:
            data = self.token_file_path.read_bytes().strip()
        except OSError as exc:
            raise ConfigError(f"AGENT_SANDBOX_ROUTER_TOKEN_FILE unreadable: {exc}") from exc
        if len(data) != TOKEN_SEED_BYTES:
            raise ConfigError(
                f"AGENT_SANDBOX_ROUTER_TOKEN_FILE must hold exactly "
                f"{TOKEN_SEED_BYTES} bytes (an Ed25519 seed), got {len(data)}: "
                "if the Secret uses `stringData`, base64-encode a 32-byte seed"
            )
        object.__setattr__(self, "token_loaded", data)
        return data

    def resolve_gateway_id(self) -> str:
        """Gateway id used to tag and reconcile owned claims."""
        return self.gateway_id or socket.gethostname()


def _parse_int(name: str, raw: Optional[str], default: int) -> int:
    if raw is None or raw == "":
        return default
    try:
        value = int(raw)
    except (TypeError, ValueError) as exc:
        raise ConfigError(f"{name} must be an integer, got {raw!r}") from exc
    if value <= 0:
        raise ConfigError(f"{name} must be a positive integer, got {value}")
    return value


def from_env(environ: Optional[dict] = None) -> AgentSandboxConfig:
    """Build a config from the environment, validating all AGENT_SANDBOX_* keys.

    Router URL and token file are required (their absence marks the backend
    unavailable); every other variable falls back to its contract default.
    """
    env = dict(environ) if environ is not None else dict(__import__("os").environ)
    router_url = env.get("AGENT_SANDBOX_ROUTER_URL", "").strip()
    token_file = env.get("AGENT_SANDBOX_ROUTER_TOKEN_FILE", "").strip()
    if not router_url or not token_file:
        raise ConfigError(
            "AGENT_SANDBOX_ROUTER_URL and AGENT_SANDBOX_ROUTER_TOKEN_FILE are required"
        )
    cfg = AgentSandboxConfig(
        router_url=router_url,
        token_file=token_file,
        create_timeout_seconds=_parse_int(
            "AGENT_SANDBOX_CREATE_TIMEOUT_SECONDS",
            env.get("AGENT_SANDBOX_CREATE_TIMEOUT_SECONDS"),
            120,
        ),
        command_timeout_seconds=_parse_int(
            "AGENT_SANDBOX_COMMAND_TIMEOUT_SECONDS",
            env.get("AGENT_SANDBOX_COMMAND_TIMEOUT_SECONDS"),
            300,
        ),
        max_lifetime_seconds=_parse_int(
            "AGENT_SANDBOX_MAX_LIFETIME_SECONDS",
            env.get("AGENT_SANDBOX_MAX_LIFETIME_SECONDS"),
            14400,
        ),
        idle_timeout_seconds=_parse_int(
            "AGENT_SANDBOX_IDLE_TIMEOUT_SECONDS",
            env.get("AGENT_SANDBOX_IDLE_TIMEOUT_SECONDS"),
            1800,
        ),
        output_limit_bytes=_parse_int(
            "AGENT_SANDBOX_OUTPUT_LIMIT_BYTES",
            env.get("AGENT_SANDBOX_OUTPUT_LIMIT_BYTES"),
            1048576,
        ),
        gateway_id=env.get("AGENT_SANDBOX_GATEWAY_ID", "").strip(),
    )
    return cfg