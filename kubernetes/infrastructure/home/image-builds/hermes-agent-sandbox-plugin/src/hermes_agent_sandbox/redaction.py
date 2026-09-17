"""Credential redaction helpers for the sandbox plugin.

WHY this module exists: Hermes stamps provider, messaging, and gateway
credentials into the gateway process environment, and the model can author
arbitrary commands. Two mechanisms guard that boundary — ``is_secret_name`` /
``strip_env_names`` back the provider's ``strip_env_keys`` classification
attribute so Hermes never forwards a credential-style variable into a
subprocess this plugin spawns, and ``ENV_ALLOWLIST`` / ``sanitize_env``
rebuild a least-privilege environment.

NOTE: the gRPC transport forwards no host environment at all (the execute
request carries only the command and a timeout), so ``sanitize_env`` is not on
any current code path — it is kept as the allowlist authority for future
callers and is covered by tests/unit/test_redaction.py.
"""

from __future__ import annotations

import os
import re
from typing import FrozenSet, Mapping, Optional

# Credential-style environment names, matched case-insensitively anywhere in
# the variable name (``API_KEY``, ``GITHUB_TOKEN``, ``AWS_SECRET_ACCESS_KEY``,
# ``MYSQL_PASSWORD``, ``SERVICE_ACCOUNT_CREDENTIALS``, ``AUTHORIZATION``...).
SECRET_NAME_RE = re.compile(r"(?i)(token|key|secret|password|credential|authorization)")

# The only variables a sandbox command ever inherits from the gateway. Kept
# deliberately small: sandboxes have their own toolchain environment and must
# never observe host-side state (paths, proxy creds, session ids).
ENV_ALLOWLIST: FrozenSet[str] = frozenset({
    "HOME",
    "PATH",
    "LANG",
    "LC_ALL",
    "LC_COLLATE",
    "LC_CTYPE",
    "LC_MESSAGES",
    "LC_MONETARY",
    "LC_NUMERIC",
    "LC_TIME",
    "SHELL",
    "TERM",
    "TERMINFO",
    "TZ",
    "USER",
})


def is_secret_name(name: str) -> bool:
    """True when *name* looks like a credential variable."""
    return bool(SECRET_NAME_RE.search(name or ""))


def sanitize_env(env: Mapping[str, str]) -> dict:
    """Return the least-privilege, redaction-safe copy of *env*.

    Only allowlisted names survive, and even those are dropped when their
    name is secret-shaped (a future allowlist addition must not be able to
    leak a credential under a plausible-looking name).
    """
    if env is None:
        return {}
    clean = {}
    for name, value in env.items():
        if name in ENV_ALLOWLIST and not is_secret_name(name):
            clean[name] = value
    return clean


def strip_env_names(environ: Optional[Mapping[str, str]] = None) -> FrozenSet[str]:
    """Frozenset of currently-present env names that look like credentials.

    Feeds ``TerminalEnvironmentProvider.strip_env_keys``: Hermes strips these
    names from every subprocess the agent spawns, so a model-authored command
    can never read a live credential 
    """
    environ = os.environ if environ is None else environ
    return frozenset(name for name in environ if is_secret_name(name))