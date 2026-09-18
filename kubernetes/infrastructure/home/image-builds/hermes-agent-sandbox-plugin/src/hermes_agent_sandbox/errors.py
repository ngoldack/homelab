"""Error hierarchy for the agent_sandbox Hermes terminal backend.

Every failure this plugin can produce derives from :class:`AgentSandboxError`
(a :class:`RuntimeError` subclass) so Hermes infrastructure treats it as an
environment/infrastructure failure: ``tools.terminal_tool`` maps
``RuntimeError`` to a structured "degraded" result and re-creates the
environment on the next call instead of caching a broken backend.
"""


class AgentSandboxError(RuntimeError):
    """Base class for every agent_sandbox plugin failure."""


class ConfigError(AgentSandboxError):
    """Invalid or missing AGENT_SANDBOX_* configuration.

    Raised when a required variable is absent or malformed, or when a caller
    tries to override the hardcoded namespace/template allowlist.
    """


class SandboxCreateError(AgentSandboxError):
    """The SandboxClaim could not be created, adopted, or bound to a sandbox."""


class SandboxCreateTimeoutError(SandboxCreateError):
    """The claim never reached Ready within the configured create timeout."""


class SandboxTransportError(AgentSandboxError):
    """A request to the router or sandbox failed at the transport layer."""


class SandboxCommandError(AgentSandboxError):
    """A command could not be started or completed in the sandbox."""


class SandboxTimeoutError(SandboxCommandError):
    """The command outran its remote deadline (gRPC DEADLINE_EXCEEDED).

    The environment answers this with returncode 124 and recycles the
    sandbox, because the cancelled remote process cannot be recovered.
    """


class CwdNotAllowedError(AgentSandboxError, ValueError):
    """The requested cwd does not resolve inside /workspace."""


class SandboxUnsupportedError(SandboxCommandError):
    """The pinned sandbox runtime does not implement the requested capability.

    Raised from a gRPC ``UNIMPLEMENTED`` (or an HTTP 501) so the caller can
    fall back to a slower path (e.g. stdin via a workspace temp file when
    ``ProcessService.Start``/``WriteStdin`` are missing from the sandboxd
    build). Auto-fallback happens ONLY for this type: a transport outage must
    never be silently retried over a different channel.
    """


class SandboxPathError(AgentSandboxError, ValueError):
    """A path violates the /workspace confinement contract.

    A ValueError subclass like :class:`CwdNotAllowedError`: a bad path is a
    caller error, not an infrastructure failure, and Hermes' tool layer is
    expected to surface it to the model instead of recycling the sandbox.
    """


class SandboxFileSizeError(SandboxPathError):
    """A file or artifact exceeds the configured byte cap."""


class SandboxQueueFullError(SandboxCommandError):
    """The session command queue is at capacity (max_queued waiters)."""


class SandboxQueueTimeoutError(SandboxCommandError):
    """A queued command waited longer than the queue timeout for a slot."""


class SandboxProcessError(SandboxCommandError):
    """A background-process operation (start/logs/stop) failed."""


class SandboxArtifactError(AgentSandboxError):
    """Artifact export, import, or store maintenance failed."""


class SandboxArtifactTooLargeError(SandboxArtifactError, SandboxFileSizeError):
    """An artifact exceeds the configured byte cap."""


class SandboxArtifactExpiredError(SandboxArtifactError):
    """The artifact id is unknown or its TTL has passed."""