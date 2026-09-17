"""Hermes ABC stub injection shared by unit and integration conftests.

Hermes cannot be pip-installed (wheel/PyPI channels retired at v0.20.0), so
the plugin's import of ``agent.terminal_env_provider`` is stubbed
with a minimal same-shaped ABC here. The stub only fixes the import boundary;
every other module under test is the real implementation. The authoritative
import conformance check runs against the REAL Hermes inside the built image
(``hermes plugins compat`` + ``hermes doctor`` gates) — a stub can never
prove path freshness, and is not claimed to.
"""

from __future__ import annotations

import sys
import types

_ABC_TERMINAL = """

class TerminalEnvironmentProvider:
    is_remote = False
    is_container = False
    session_isolated_when_nonpersistent = False

    @property
    def strip_env_keys(self):
        return frozenset()

    def create_environment(self, *args, **kwargs):
        raise NotImplementedError
"""


def _make_module(full_name: str, source: str = "") -> types.ModuleType:
    mod = types.ModuleType(full_name)
    mod.__file__ = f"<stub:{full_name}>"
    mod.__path__ = []
    if source:
        exec(compile(source, f"<stub:{full_name}>", "exec"), mod.__dict__)
    sys.modules[full_name] = mod
    return mod


def install_hermes_stub() -> None:
    """Idempotently install the hermes_agent stub tree into sys.modules.

    The stubbed import paths match the installed v2026.9.7 layout (top-level
    ``agent`` package), mirrored by src/hermes_agent_sandbox/provider.py.
    """
    if "agent" not in sys.modules:
        _make_module("agent")
        _make_module("agent.terminal_env_provider", _ABC_TERMINAL)