"""Live-cluster integration fixtures for the agent_sandbox plugin.

These tests drive the REAL stack: SDK K8sHelper -> SandboxClaim against the
hermes-go warm pool -> Router-mediated exec into a Kata sandbox on the
sandbox worker. They require, and are skipped without:

* AGENT_SANDBOX_ROUTER_URL     (port-forwarded sandbox-router svc, e.g.
  http://127.0.0.1:8080)
* AGENT_SANDBOX_ROUTER_TOKEN_FILE (raw 32-byte Ed25519 seed bytes)
* KUBECONFIG                   (kubeconfig-home.yaml with cluster access)

Use hack/hermes-plugin-integration.sh to set all of these up.
"""

from __future__ import annotations

import os
import sys
import uuid
from pathlib import Path

# Same src/ bootstrap the unit conftest uses.
SRC = Path(__file__).resolve().parents[2] / "src"
sys.path.insert(0, str(SRC))
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from _hermes_stub import install_hermes_stub  # noqa: E402

install_hermes_stub()

import pytest  # noqa: E402

from hermes_agent_sandbox.config import AgentSandboxConfig
from hermes_agent_sandbox.transport import KubeClaimsClient, SandboxTransport

pytestmark = pytest.mark.integration


def _required_env(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        pytest.skip(f"{name} not set — live integration requires the cluster")
    return value


def _raw_seed(path: str) -> bytes:
    data = open(path, "rb").read().strip()
    if len(data) == 32:
        return data
    # Hex-encoded seed (64 chars) tolerated for driver convenience.
    return bytes.fromhex(data.decode("ascii"))


@pytest.fixture(scope="module")
def config() -> AgentSandboxConfig:
    from hermes_agent_sandbox.config import from_env

    return from_env()


@pytest.fixture(scope="module")
def claims(config) -> KubeClaimsClient:
    return KubeClaimsClient(config)


@pytest.fixture(scope="module")
def transport(config) -> SandboxTransport:
    seed = _raw_seed(config.token_file)
    return SandboxTransport(config, secret=seed)


@pytest.fixture(scope="module")
def sandbox_env(config, claims, transport):
    from hermes_agent_sandbox.environment import AgentSandboxEnvironment

    env = AgentSandboxEnvironment(
        config,
        task_id=f"itest-{uuid.uuid4().hex[:8]}",
        claims=claims,
        transport=transport,
    )
    # Adopt a real sandbox up front: exec tests may skip (workstation runs),
    # but pod/policy/file tests still need a live adopted claim.
    env._ensure_ready()
    yield env
    env.cleanup()


def kubectl(*args: str) -> str:
    import subprocess

    kubeconfig = os.environ.get("KUBECONFIG")
    if not kubeconfig:
        pytest.skip("KUBECONFIG not set")
    proc = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, *args],
        capture_output=True,
        text=True,
        timeout=60,
    )
    if proc.returncode != 0:
        raise AssertionError(f"kubectl {' '.join(args)} failed: {proc.stderr}")
    return proc.stdout