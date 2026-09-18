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

# Fixed sandbox root. It is sandboxd's `--root-dir` (runtime Dockerfile ENTRYPOINT
# defaults), the SandboxTemplate's only writable emptyDir, and the value every
# FilesystemService path and ProcessService cwd is confined to. Not a knob: the
# template and the daemon would have to move together, and a session choosing
# its own root would escape the confinement both of them enforce.
WORKSPACE = "/workspace"

# ---- runtime profiles (Phase-2 capability layer) ----
# A profile is a NAME the model may select; it is never an image, runtimeClass,
# namespace, serviceAccount, mount or network policy. The mapping below is the
# single authority for profile -> SandboxTemplate.
#
# SELECTABLE NAMES ARE ONLY PROFILES WHOSE TEMPLATE ACTUALLY EXISTS. Verified
# against the shipped manifests by the profile unit (Unit 2.4): hermes-go
# (retained), hermes-core and hermes-offline are the only SandboxTemplates, all
# on runtime digest sha256:8317b00f8aa3801b9dc5746f18178a0f4ec3df1e1b51a24e8468b1a3352117f1.
# `python` is deliberately NOT selectable: the runtime image installs no Python
# (hermes-sandbox-runtime/Dockerfile apt list = ca-certificates procps curl jq
# ripgrep git coreutils util-linux + the Go toolchain), so a `hermes-python`
# claim would either fail to exist or hand the session an image without python3.
# Advertising it would turn a truthful "not available yet" into a confusing
# SandboxCreateError naming a warm pool that does not exist.
PROFILE_LABEL = "workload.hermes.io/profile"
PROFILE_CORE = "core"
PROFILE_OFFLINE = "offline"
PROFILE_GO = "go"  # the retained hermes-go template the backend has always used
PROFILE_TEMPLATES = {
    PROFILE_CORE: "hermes-core",        # baseline: shell + coreutils + git (no toolchain)
    PROFILE_OFFLINE: "hermes-offline",  # baseline + vendored caches, DNS-only egress
    PROFILE_GO: "hermes-go",            # baseline + Go 1.26.5 toolchain (legacy default)
}
PROFILE_NAMES = tuple(PROFILE_TEMPLATES)
# Known-but-not-yet-existing profiles from the batch contract. Listed so that
# selecting one produces an explicit "planned, no template yet" error instead of
# a silent fallback onto another profile's image/toolchain.
PROFILE_PLANNED = {
    "python": "hermes-python",  # blocked: runtime image ships no python3
    "node": "hermes-node",
    "web": "hermes-web",
}


def template_for_profile(profile: str) -> str:
    """Map a profile NAME onto its SandboxTemplate; unknown names are fatal.

    Deliberately strict: a typo must never silently fall back to another
    profile's image/toolchain, because the profile is what the model's task
    assumed (a missing toolchain surfaces as a confusing command failure).
    """
    if profile in PROFILE_PLANNED:
        raise ConfigError(
            f"sandbox profile {profile!r} is planned but has no SandboxTemplate yet "
            f"(expected {PROFILE_PLANNED[profile]!r}); available profiles: "
            + ", ".join(PROFILE_NAMES)
        )
    try:
        return PROFILE_TEMPLATES[profile]
    except KeyError:
        raise ConfigError(
            f"unknown sandbox profile {profile!r}; available profiles: "
            + ", ".join(PROFILE_NAMES)
        ) from None

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


# stdin transport modes for execute(stdin_data=...).
#   native — sandboxd ProcessService Start + WriteStdin (real stdin frames).
#   file   — write the payload into /workspace, exec with `exec 0< file`, delete.
#   auto   — try native, fall back to file on gRPC UNIMPLEMENTED ONLY.
# Verified capability (why `auto` normally never falls back): sandboxd v1.0.2 at
# the pinned commit 9a85153590e54cb980f3241f9e7a9228449412c9 serves
# `WriteStdin(WriteStdinRequest{process_id, input, eof})` for any process started
# with the streaming `Start` RPC (packages/sandboxd/pkg/server/process.go,
# WriteStdin) and creates a stdin pipe for the non-PTY path (same file, Start).
STDIN_NATIVE = "native"
STDIN_FILE = "file"
STDIN_AUTO = "auto"
STDIN_MODES = (STDIN_AUTO, STDIN_NATIVE, STDIN_FILE)


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

    # ---- capability layer: files (Unit 2.1/2.2) ----
    # 8 MiB mirrors the plan's write cap constant: larger payloads belong in an
    # artifact (tar over REST), not in a single PUT body, and the gateway holds
    # a write body in memory while streaming it.
    file_write_max_bytes: int = 8388608
    # Reads land in the model's context, so the default is far below the
    # transport's own capability; raise it for a session that must ingest a
    # generated file, and use export_artifact for anything bigger.
    file_read_max_bytes: int = 8388608
    # stdin transport: `auto` tries native ProcessService streams first and
    # falls back to a workspace temp file; `native`/`file` pin one path.
    stdin_mode: str = "auto"
    stdin_max_bytes: int = 4194304

    # ---- capability layer: artifacts + checkpoints (Unit 2.3/2.6) ----
    # Gateway-side store for exported tarballs. /opt/data is the Hermes PVC
    # (statefulset volumeClaimTemplate `data`, 10 GiB, writable by uid 10000);
    # the container's root filesystem is read-only, so /tmp or a relative path
    # would lose every artifact on pod restart.
    artifact_root: str = "/opt/data/agent-sandbox/artifacts"
    # Artifacts default to one day; the ceiling keeps a session from pinning
    # the shared PVC for a week by passing a huge TTL. Both are enforced.
    artifact_default_ttl_hours: int = 24
    artifact_max_ttl_hours: int = 168
    # import_artifact holds the whole tarball in memory for the PUT body, and
    # the gateway container's memory limit is 2 GiB — 64 MiB leaves room for
    # the rest of the turn while covering realistic source checkpoints.
    artifact_max_bytes: int = 67108864

    # ---- capability layer: session queue (Unit 2.5) ----
    # Bounded FIFO admission for command submissions on ONE session/sandbox.
    # Concurrency stays above 1 because file-path probes and REST transfers must
    # not queue behind a long-running command, while the bound still stops a
    # runaway loop of tool calls from stacking unbounded work on a 1-CPU sandbox.
    queue_max_concurrent: int = 4
    queue_max_queued: int = 8
    queue_timeout_seconds: int = 60

    # ---- capability layer: background processes (Unit 2.5) ----
    process_start_timeout_seconds: int = 30
    process_max_concurrent: int = 4
    # Reader threads keep only the tail of each stream in memory: a chatty
    # background process must not grow the gateway's RSS without bound. The ring
    # is per stream (stdout/stderr separately) and the read view is tailed.
    process_log_buffer_bytes: int = 1048576
    process_log_tail_bytes: int = 65536
    process_stop_grace_seconds: int = 5

    # ---- egress guard (Phase 3, Unit 3.5) ----
    # Mounted file holding the raw HMAC secret (hermes-egress/secret.sops.yaml,
    # key EGRESS_HMAC_SECRET — the same bytes the authorizer verifies against).
    # Empty disables proxy identity entirely: commands run with no proxy env
    # (Cilium still confines the sandbox to DNS-only, so nothing is opened).
    egress_hmac_secret_file: str = ""
    # Proxy token TTL: minted at claim create with expiry = now + ttl. Must
    # exceed the claim lifetime cap (max_lifetime_seconds, default 4 h) so a
    # token never expires mid-session; the guard accepts up to 24 h.
    egress_token_ttl_seconds: int = 86400
    # The profile the proxy identity carries (token vocabulary: the fixed
    # profile set). Defaults to the backend's template's profile — the plugin
    # never reads it from a caller.
    egress_profile: str = "go"

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
            "file_write_max_bytes",
            "file_read_max_bytes",
            "stdin_max_bytes",
            "artifact_default_ttl_hours",
            "artifact_max_ttl_hours",
            "artifact_max_bytes",
            "queue_max_concurrent",
            "queue_timeout_seconds",
            "process_start_timeout_seconds",
            "process_max_concurrent",
            "process_log_buffer_bytes",
            "process_log_tail_bytes",
            "process_stop_grace_seconds",
        ):
            value = getattr(self, name)
            if not isinstance(value, int) or value <= 0:
                raise ConfigError(
                    f"{name} must be a positive integer, got {value!r}"
                )
        # 0 is a meaningful value for the queue depth: "never wait, reject the
        # moment the session is busy" (fail fast instead of parking threads).
        if not isinstance(self.queue_max_queued, int) or self.queue_max_queued < 0:
            raise ConfigError(
                f"queue_max_queued must be a non-negative integer, got {self.queue_max_queued!r}"
            )
        # The token must outlive the claim: an expiry inside the claim lifetime
        # would answer 403 token-expired mid-session (fail-closed, but a
        # self-inflicted outage). The guard accepts up to 24 h; a TTL above
        # that would be rejected by its EGRESS_TOKEN_MAX_TTL_S.
        if self.egress_token_ttl_seconds < self.max_lifetime_seconds:
            raise ConfigError(
                "egress_token_ttl_seconds must not be shorter than "
                f"max_lifetime_seconds ({self.egress_token_ttl_seconds} < "
                f"{self.max_lifetime_seconds}): the token would expire "
                "mid-session"
            )
        if self.egress_token_ttl_seconds > 86400:
            raise ConfigError(
                "egress_token_ttl_seconds must not exceed 86400 (the guard's "
                f"EGRESS_TOKEN_MAX_TTL_S), got {self.egress_token_ttl_seconds}"
            )
        # The profile rides the HMAC: an unknown profile answers 403
        # profile-unknown for every request (fail-closed), so a typo is caught
        # here where it is loud, not at first request.
        if self.egress_profile not in PROFILE_NAMES:
            raise ConfigError(
                f"egress_profile must be one of {', '.join(PROFILE_NAMES)}, "
                f"got {self.egress_profile!r}"
            )
        if self.stdin_mode not in STDIN_MODES:
            raise ConfigError(
                f"stdin_mode must be one of {', '.join(STDIN_MODES)}, got {self.stdin_mode!r}"
            )
        # A default above the ceiling would make every export fail with a TTL
        # error the caller never asked for; reject the pair instead.
        if self.artifact_default_ttl_hours > self.artifact_max_ttl_hours:
            raise ConfigError(
                "artifact_default_ttl_hours must not exceed artifact_max_ttl_hours "
                f"({self.artifact_default_ttl_hours} > {self.artifact_max_ttl_hours})"
            )
        if not self.artifact_root.strip():
            raise ConfigError("artifact_root must be a non-empty path")
        # Keep the read view inside the retained ring: a tail larger than the
        # buffer silently promises more history than the reader keeps.
        if self.process_log_tail_bytes > self.process_log_buffer_bytes:
            raise ConfigError(
                "process_log_tail_bytes must not exceed process_log_buffer_bytes "
                f"({self.process_log_tail_bytes} > {self.process_log_buffer_bytes})"
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

    def load_egress_secret(self) -> Optional[bytes]:
        """Read the raw egress HMAC secret; None when the knob is unset.

        Empty or missing disables proxy identity (commands run with no proxy
        env; Cilium still confines the sandbox), but a CONFIGURED file that
        fails to read or is empty is a ConfigError — a misconfigured mount
        must be loud, not a silent DNS-only downgrade. The value is the raw
        secret bytes the hermes-egress guard's auth.py HMACs with; no base64
        unwrap (the Secret ships `stringData`, matching the router-token
        mount pattern).
        """
        if not self.egress_hmac_secret_file:
            return None

        try:
            data = Path(self.egress_hmac_secret_file).read_bytes().strip()
        except OSError as exc:
            raise ConfigError(
                f"AGENT_SANDBOX_EGRESS_HMAC_SECRET_FILE configured but unreadable: {exc}"
            ) from exc
        if not data:
            raise ConfigError(
                "AGENT_SANDBOX_EGRESS_HMAC_SECRET_FILE is empty: mount the "
                "hermes-egress egress-hmac Secret's EGRESS_HMAC_SECRET key"
            )
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


def _parse_nonnegative_int(name: str, raw: Optional[str], default: int) -> int:
    """Like :func:`_parse_int` but 0 is meaningful (queue depth)."""
    if raw is None or raw == "":
        return default
    try:
        value = int(raw)
    except (TypeError, ValueError) as exc:
        raise ConfigError(f"{name} must be an integer, got {raw!r}") from exc
    if value < 0:
        raise ConfigError(f"{name} must be a non-negative integer, got {value}")
    return value


def _parse_choice(name: str, raw: Optional[str], default: str, allowed) -> str:
    """Parse a string enum knob; unknown values are a ConfigError (never a silent default)."""
    value = (raw or "").strip().lower() or default
    if value not in allowed:
        raise ConfigError(f"{name} must be one of {', '.join(allowed)}, got {value!r}")
    return value


def _parse_str(name: str, raw: Optional[str], default: str) -> str:
    value = (raw or "").strip()
    return value or default


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
        # ---- capability layer (files, artifacts, queue, processes) ----
        file_write_max_bytes=_parse_int(
            "AGENT_SANDBOX_FILE_WRITE_MAX_BYTES",
            env.get("AGENT_SANDBOX_FILE_WRITE_MAX_BYTES"),
            8388608,
        ),
        file_read_max_bytes=_parse_int(
            "AGENT_SANDBOX_FILE_READ_MAX_BYTES",
            env.get("AGENT_SANDBOX_FILE_READ_MAX_BYTES"),
            8388608,
        ),
        stdin_mode=_parse_choice(
            "AGENT_SANDBOX_STDIN_MODE",
            env.get("AGENT_SANDBOX_STDIN_MODE"),
            STDIN_AUTO,
            STDIN_MODES,
        ),
        stdin_max_bytes=_parse_int(
            "AGENT_SANDBOX_STDIN_MAX_BYTES",
            env.get("AGENT_SANDBOX_STDIN_MAX_BYTES"),
            4194304,
        ),
        artifact_root=_parse_str(
            "AGENT_SANDBOX_ARTIFACT_ROOT",
            env.get("AGENT_SANDBOX_ARTIFACT_ROOT"),
            "/opt/data/agent-sandbox/artifacts",
        ),
        artifact_default_ttl_hours=_parse_int(
            "AGENT_SANDBOX_ARTIFACT_DEFAULT_TTL_HOURS",
            env.get("AGENT_SANDBOX_ARTIFACT_DEFAULT_TTL_HOURS"),
            24,
        ),
        artifact_max_ttl_hours=_parse_int(
            "AGENT_SANDBOX_ARTIFACT_MAX_TTL_HOURS",
            env.get("AGENT_SANDBOX_ARTIFACT_MAX_TTL_HOURS"),
            168,
        ),
        artifact_max_bytes=_parse_int(
            "AGENT_SANDBOX_ARTIFACT_MAX_BYTES",
            env.get("AGENT_SANDBOX_ARTIFACT_MAX_BYTES"),
            67108864,
        ),
        queue_max_concurrent=_parse_int(
            "AGENT_SANDBOX_QUEUE_MAX_CONCURRENT",
            env.get("AGENT_SANDBOX_QUEUE_MAX_CONCURRENT"),
            4,
        ),
        queue_max_queued=_parse_nonnegative_int(
            "AGENT_SANDBOX_QUEUE_MAX_QUEUED",
            env.get("AGENT_SANDBOX_QUEUE_MAX_QUEUED"),
            8,
        ),
        queue_timeout_seconds=_parse_int(
            "AGENT_SANDBOX_QUEUE_TIMEOUT_SECONDS",
            env.get("AGENT_SANDBOX_QUEUE_TIMEOUT_SECONDS"),
            60,
        ),
        process_start_timeout_seconds=_parse_int(
            "AGENT_SANDBOX_PROCESS_START_TIMEOUT_SECONDS",
            env.get("AGENT_SANDBOX_PROCESS_START_TIMEOUT_SECONDS"),
            30,
        ),
        process_max_concurrent=_parse_int(
            "AGENT_SANDBOX_PROCESS_MAX_CONCURRENT",
            env.get("AGENT_SANDBOX_PROCESS_MAX_CONCURRENT"),
            4,
        ),
        process_log_buffer_bytes=_parse_int(
            "AGENT_SANDBOX_PROCESS_LOG_BUFFER_BYTES",
            env.get("AGENT_SANDBOX_PROCESS_LOG_BUFFER_BYTES"),
            1048576,
        ),
        process_log_tail_bytes=_parse_int(
            "AGENT_SANDBOX_PROCESS_LOG_TAIL_BYTES",
            env.get("AGENT_SANDBOX_PROCESS_LOG_TAIL_BYTES"),
            65536,
        ),
        process_stop_grace_seconds=_parse_int(
            "AGENT_SANDBOX_PROCESS_STOP_GRACE_SECONDS",
            env.get("AGENT_SANDBOX_PROCESS_STOP_GRACE_SECONDS"),
            5,
        ),
        # ---- egress guard (Phase 3, Unit 3.5) ----
        egress_hmac_secret_file=_parse_str(
            "AGENT_SANDBOX_EGRESS_HMAC_SECRET_FILE",
            env.get("AGENT_SANDBOX_EGRESS_HMAC_SECRET_FILE"),
            "",
        ),
        egress_token_ttl_seconds=_parse_int(
            "AGENT_SANDBOX_EGRESS_TOKEN_TTL_SECONDS",
            env.get("AGENT_SANDBOX_EGRESS_TOKEN_TTL_SECONDS"),
            86400,
        ),
        egress_profile=_parse_str(
            "AGENT_SANDBOX_EGRESS_PROFILE",
            env.get("AGENT_SANDBOX_EGRESS_PROFILE"),
            "go",
        ),
        gateway_id=env.get("AGENT_SANDBOX_GATEWAY_ID", "").strip(),
    )
    return cfg