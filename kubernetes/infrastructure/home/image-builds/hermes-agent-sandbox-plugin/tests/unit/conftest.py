"""Shared fixtures + import stubs for agent_sandbox plugin unit tests.

Hermes cannot be pip-installed (retired channel); the import stub is shared
with the integration suite via tests/_hermes_stub.py. Real-import conformance
runs inside the built image (`hermes plugins compat` gate), not here.
"""

from __future__ import annotations

import sys
from pathlib import Path

# Make `import hermes_agent_sandbox` resolve against src/.
SRC = Path(__file__).resolve().parents[2] / "src"
sys.path.insert(0, str(SRC))

# Make tests/_hermes_stub importable (conftests live one level below tests/).
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from _hermes_stub import install_hermes_stub  # noqa: E402

install_hermes_stub()

import pytest  # noqa: E402

from hermes_agent_sandbox.config import AgentSandboxConfig  # noqa: E402


@pytest.fixture
def token_file(tmp_path: Path) -> Path:
    """A token file containing a synthetic 32-byte Ed25519 seed."""
    p = tmp_path / "router-token"
    p.write_bytes(b"x" * 32)
    return p


@pytest.fixture
def config(token_file: Path) -> AgentSandboxConfig:
    return AgentSandboxConfig(
        router_url="http://router.svc.cluster.local:8080",
        token_file=str(token_file),
    )


@pytest.fixture
def env_with_router(tmp_path: Path, monkeypatch):
    """AGENT_SANDBOX_ROUTER_URL + a real temp token file for from_env()."""
    tok = tmp_path / "tok"
    tok.write_bytes(b"y" * 32)
    monkeypatch.setenv("AGENT_SANDBOX_ROUTER_URL", "http://router:8080")
    monkeypatch.setenv("AGENT_SANDBOX_ROUTER_TOKEN_FILE", str(tok))
    return tmp_path


@pytest.fixture(autouse=True)
def fresh_sandbox_capacity():
    """Give every unit test a pristine process-wide sandbox capacity gate.

    Unit tests never run Hermes' session teardown, so the one-slot semaphore
    would otherwise be drained by the first test that executes a command and
    starve every later test in the same process.
    """
    import threading

    from hermes_agent_sandbox import environment

    environment._CAPACITY = threading.Semaphore(environment._MAX_CONCURRENT_SANDBOXES)