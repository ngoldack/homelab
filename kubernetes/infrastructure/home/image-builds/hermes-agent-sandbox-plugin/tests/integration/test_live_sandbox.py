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


def test_stdin_roundtrip(sandbox_env):
    """stdin_data reaches the guest process as real stdin (native WriteStdin
    in this sandboxd build; the file fallback is for UNIMPLEMENTED builds).
    Unit 2.2's acceptance: `cat` round-trips the payload back."""
    _exec_gate()
    payload = f"stdin-{uuid.uuid4().hex}\n"
    result = sandbox_env.execute("cat", stdin_data=payload, timeout=30)
    assert result["returncode"] == 0, result
    assert result["output"] == payload, result


def test_stdin_over_limit_is_rejected_before_guest(sandbox_env):
    """The stdin size cap is enforced at the plugin boundary — a payload over
    AGENT_SANDBOX_STDIN_MAX_BYTES never touches the wire."""
    _exec_gate()
    from hermes_agent_sandbox.errors import SandboxFileSizeError

    limit = sandbox_env.config.stdin_max_bytes
    with pytest.raises(SandboxFileSizeError):
        sandbox_env.execute("cat", stdin_data=b"x" * (limit + 1), timeout=30)


def test_background_process_streams_logs_and_stops(sandbox_env):
    """Unit 2.5's contract against the real sandbox: a long-running process
    survives the exec call, output accumulates, and stop() terminates it."""
    _exec_gate()
    registry = sandbox_env._processes  # ProcessRegistry, lazy-created
    handle = registry.start(
        "for i in 1 2 3 4 5; do echo tick-$i; sleep 1; done", start_timeout=30
    )
    assert handle.remote_process_id > 0, handle.as_dict()
    assert handle.running

    deadline = time.monotonic() + 30
    logs = {}
    while time.monotonic() < deadline:
        logs = handle.logs(tail_bytes=4096)
        if "tick-3" in logs.get("output", ""):
            break
        time.sleep(0.5)
    assert "tick-1" in logs.get("output", ""), f"output never accumulated: {logs}"
    assert "tick-2" in logs.get("output", ""), f"output truncated mid-stream: {logs}"

    stopped = registry.stop(handle.id)
    assert stopped.get("state") in ("stopped", "exited", "finished"), stopped
    after = registry.get(handle.id)
    assert not after.running, f"process still running after stop: {after.as_dict()}"


def test_artifact_export_import_roundtrip(sandbox_env):
    """Unit 2.3's core loop against the real sandbox: write a workspace file,
    export it to the gateway artifact store, delete it in the guest, import it
    back, and read the restored content."""
    _exec_gate()
    tag = uuid.uuid4().hex[:8]
    remote_dir = f"/workspace/art-{tag}"
    make = sandbox_env.execute(
        f"mkdir -p {remote_dir} && printf 'payload-{tag}\\n' > {remote_dir}/data.txt",
        timeout=30,
    )
    assert make["returncode"] == 0, make

    store = sandbox_env.artifacts
    ref = store.export(
        sandbox_env._transport,
        sandbox_env.config,
        [remote_dir],
        ttl_hours=1,
        session=f"itest-{tag}",
        kind="itest",
        timeout=120,
    )
    try:
        assert ref.bytes > 0, ref.as_dict()
        assert ref.sha256, "export recorded no digest"
        from pathlib import Path

        stored = Path(store.data_path(ref.id)).read_bytes()
        assert len(stored) == ref.bytes, "stored tarball size differs from the ref"
        assert ref.sha256 == __import__("hashlib").sha256(stored).hexdigest()

        # Guest copy is gone (import is the only road back).
        rm = sandbox_env.execute(f"rm -rf {remote_dir}", timeout=30)
        assert rm["returncode"] == 0, rm
        restored = store.import_artifact(
            sandbox_env._transport, sandbox_env.config, ref.id, timeout=120
        )
        assert restored.id == ref.id, restored.as_dict()
        read = sandbox_env.execute(f"cat {remote_dir}/data.txt", timeout=30)
        assert read["returncode"] == 0, read
        assert read["output"].strip() == f"payload-{tag}", read
    finally:
        store._remove_files(ref.id)


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