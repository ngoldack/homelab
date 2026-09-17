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