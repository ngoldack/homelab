"""Live end-to-end: claim lifecycle, Kata exec, plugin semantics, cleanup.

Exec assertions require an in-cluster caller (sandbox ingress policy admits
only agent-sandbox-system + hermes namespaces on the gRPC port); they skip
with AGENT_SANDBOX_IN_CLUSTER unset. The Phase-7 e2e driver sets it when
running from inside the Hermes gateway's namespace.
"""

from __future__ import annotations

import os
import time
import urllib.parse
import urllib.request
import uuid

import pytest

from conftest import kubectl, pytestmark  # noqa: F401

HERMES_SANDBOX_NS = "hermes-sandbox"

IN_CLUSTER = os.environ.get("AGENT_SANDBOX_IN_CLUSTER") == "1"


def _exec_gate():
    if not IN_CLUSTER:
        pytest.skip(
            "exec requires an in-cluster caller (sandbox ingress admits only "
            "admitted namespaces on gRPC); set AGENT_SANDBOX_IN_CLUSTER=1"
        )


def test_exec_runs_in_kata_guest(sandbox_env):
    _exec_gate()
    result = sandbox_env.execute("uname -s && pwd && echo HI")
    assert result["returncode"] == 0, result
    assert result["output"] == "Linux\n/workspace\nHI\n"
    assert result["cwd"] == "/workspace"


def test_exit_code_propagates(sandbox_env):
    _exec_gate()
    assert sandbox_env.execute("false")["returncode"] == 1
    assert sandbox_env.execute("true")["returncode"] == 0


def test_go_toolchain_available(sandbox_env):
    _exec_gate()
    # The hermes-go runtime image ships a Go toolchain (Phase 4 image).
    result = sandbox_env.execute("go version")
    assert result["returncode"] == 0, result
    assert "go" in result["output"].lower()
    result = sandbox_env.execute("cd /tmp && rm -rf gotest && mkdir gotest && cd gotest && "
                                 "go mod init example.com/t >/dev/null 2>&1 && "
                                 "printf 'package t\\nfunc F() int { return 42 }\\n' > t.go && "
                                 "go test ./...")
    assert result["returncode"] == 0, result


def test_no_serviceaccount_token_in_sandbox(sandbox_env):
    _exec_gate()
    result = sandbox_env.execute(
        "test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token && echo NO_SA || echo HAS_SA"
    )
    assert result["output"].strip() == "NO_SA"


def test_command_timeout_returns_124(sandbox_env):
    _exec_gate()
    start = time.monotonic()
    result = sandbox_env.execute("sleep 120", timeout=2)
    elapsed = time.monotonic() - start
    assert result["returncode"] == 124, result
    assert elapsed < 60, "timeout took too long"


def test_sandbox_pod_runs_kata_on_sandbox_worker(sandbox_env):
    pod = sandbox_env._sandbox_name
    assert pod, "no adopted sandbox name recorded"
    runtime = kubectl(
        "-n", HERMES_SANDBOX_NS, "get", "pod", pod,
        "-o", "jsonpath={.spec.runtimeClassName}",
    )
    assert runtime.strip() == "kata", f"runtimeClassName={runtime!r} (runc fallback?)"
    node = kubectl(
        "-n", HERMES_SANDBOX_NS, "get", "pod", pod, "-o", "jsonpath={.spec.nodeName}"
    ).strip()
    import json

    labels = json.loads(kubectl("get", "node", node, "-o", "jsonpath={.metadata.labels}"))
    assert labels.get("workload.hermes.io/sandbox") == "true", (
        f"pod on wrong node {node}: {labels}"
    )


def test_real_provider_path_executes(sandbox_env):
    """Drive the exact production entry point: AgentSandboxProvider.
    create_environment() builds the environment WITHOUT a transport; the
    lazy transport init must make commands work (regression guard).
    """
    _exec_gate()
    from hermes_agent_sandbox.provider import AgentSandboxProvider

    provider = AgentSandboxProvider()
    assert provider.is_available()  # AGENT_SANDBOX_* envs set by the driver
    env = provider.create_environment(
        cwd="/workspace",
        timeout=60,
        task_id=f"itest-prov-{uuid.uuid4().hex[:8]}",
    )
    try:
        result = env.execute("echo PROVIDER_OK && pwd")
        assert result["returncode"] == 0, result
        assert result["output"] == "PROVIDER_OK\n/workspace\n"
        assert result["cwd"] == "/workspace"
        assert env._transport is not None
        assert env._transport._target is not None  # attached to an adopted sandbox
    finally:
        env.cleanup()


def test_file_roundtrip_through_authenticated_router(sandbox_env, config):
    """PUT a file into the sandbox and fetch it back via the plugin's
    Router-authenticated GET /v1/files path (workstation-runnable: traffic
    enters the sandbox through the Router, which the policy admits)."""
    payload = b"hermes-file-roundtrip-" + os.urandom(8)
    remote = f"/workspace/itest-{uuid.uuid4().hex[:8]}.bin"
    escaped = urllib.parse.quote(remote, safe="/")
    target = sandbox_env._transport._target
    assert target is not None

    req = urllib.request.Request(
        f"{config.router_url.rstrip('/')}/v1/files{escaped}",
        data=payload,
        headers=target.headers("PUT", f"/v1/files{escaped}"),
        method="PUT",
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        assert resp.status in (200, 204)  # sandboxd PUT returns 204 No Content

    local = __import__("pathlib").Path("/tmp/itest-roundtrip.bin")
    sandbox_env.fetch_file(remote, local, max_bytes=10_000)
    assert local.read_bytes() == payload
    local.unlink()

    delete_req = urllib.request.Request(
        f"{config.router_url.rstrip('/')}/v1/files{escaped}",
        headers=target.headers("DELETE", f"/v1/files{escaped}"),
        method="DELETE",
    )
    with urllib.request.urlopen(delete_req, timeout=30) as resp:
        assert resp.status in (200, 204)


def test_claim_deleted_after_cleanup(sandbox_env, claims):
    from hermes_agent_sandbox.transport import OWNER_LABEL

    # Identity must be captured BEFORE cleanup(): cleanup() clears
    # _claim_name/_sandbox_name, so asserting against them afterwards passes
    # no matter what the teardown actually did.
    gateway_id = claims.config.resolve_gateway_id()
    claim_name = sandbox_env._claim_name
    pod = sandbox_env._sandbox_name
    assert claim_name, "no claim recorded before cleanup"
    assert pod, "no adopted sandbox name recorded before cleanup"
    sandbox_env.cleanup()
    # Claim must be gone (SDK delete), and the pool still has its warm pod.
    names = claims.list_owned_claims(gateway_id)
    assert claim_name not in {n.name for n in names}, (
        f"claim {claim_name} still owned by {gateway_id} after cleanup: "
        f"{[n.name for n in names]}"
    )
    strays = kubectl(
        "-n", HERMES_SANDBOX_NS, "get", "sandboxclaims",
        "-l", f"{OWNER_LABEL}={gateway_id}", "-o", "name",
    )
    assert claim_name not in strays, f"claim {claim_name} still present: {strays!r}"
    assert f"pod/{pod}" not in kubectl(
        "-n", HERMES_SANDBOX_NS, "get", "pods", "-o", "name"
    ).split(), f"sandbox pod {pod} survived cleanup"
    out = kubectl("-n", HERMES_SANDBOX_NS, "get", "sandboxwarmpool", "hermes-go",
                  "-o", "jsonpath={.status.replicas}")
    # Warm pool self-heals back toward 1 after claim teardown.
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        out = kubectl("-n", HERMES_SANDBOX_NS, "get", "sandboxwarmpool", "hermes-go",
                      "-o", "jsonpath={.status.replicas}")
        if out.strip() == "1":
            break
        time.sleep(5)
    assert out.strip() == "1", f"warm pool did not replenish, replicas={out!r}"