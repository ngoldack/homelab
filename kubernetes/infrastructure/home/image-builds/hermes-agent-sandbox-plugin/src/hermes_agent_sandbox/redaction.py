"""Credential redaction and environment allowlisting for sandbox commands.

WHY two layers: Hermes stamps provider, messaging, and gateway credentials
into the gateway process environment, and the model can author arbitrary
commands. The environment handed to each sandbox command must therefore be
rebuilt from an explicit allowlist (least privilege) AND scrubbed of any
secret-shaped name (defense in depth, in case allowlist drift ever admits
one). The same secret-name matcher backs the provider's ``strip_env_keys``
classification attribute, so Hermes never forwards a credential-style
variable into a subprocess this plugin spawns.
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