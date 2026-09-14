"""agent_sandbox — Hermes terminal backend that runs commands in a Kubernetes
Agent Sandbox (kubernetes-sigs/agent-sandbox, Kata runtime) via hermes-go warm
pool and the authenticated Router.
"""

from .config import AgentSandboxConfig, from_env
from .environment import AgentSandboxEnvironment
from .errors import AgentSandboxError
from .provider import AgentSandboxProvider

__version__ = "0.1.0"

__all__ = [
    "AgentSandboxProvider",
    "AgentSandboxEnvironment",
    "AgentSandboxConfig",
    "AgentSandboxError",
    "from_env",
    "__version__",
]