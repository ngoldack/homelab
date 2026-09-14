"""Environment lifecycle: lazy claims, exec semantics, timeout, recycle, files."""

from __future__ import annotations

import time
from pathlib import Path

import pytest

from hermes_agent_sandbox.config import AgentSandboxConfig
from hermes_agent_sandbox.environment import (
    TRUNCATION_SUFFIX,
    AgentSandboxEnvironment,
)
from hermes_agent_sandbox.errors import (
    CwdNotAllowedError,
    SandboxCommandError,
)


class FakeClaims:
    def __init__(self):
        self.created = []
        self.deleted = []
        self.waited = []
        self.uid_looked = []

    def create_claim(self, name):
        self.created.append(name)

    def wait_ready(self, name, timeout):
        self.waited.append((name, timeout))
        return "sbx-1"

    def get_sandbox_uid(self, name):
        self.uid_looked.append(name)
        return "uid-1"

    def get_sandbox_ip(self, name):
        return "10.0.0.5"

    def delete_claim(self, name):
        self.deleted.append(name)


class FakeTransport:
    def __init__(self, script=None):
        self.script = script or {}
        self.attached = []
        self.cancelled = 0
        self.closed = 0
        self.runs = []

    def attach(self, name, uid, pod_ip):
        self.attached.append((name, uid, pod_ip))

    def cancel(self):
        self.cancelled += 1

    def close(self):
        self.closed += 1

    def fetch_file(self, remote_path, timeout=300):
        if "missing" in remote_path:
            raise SandboxCommandError(f"{remote_path!r} not found in the sandbox")
        return b"file-bytes-" + remote_path.encode()

    def run(self, command, timeout):
        self.runs.append((command, timeout))
        for key, value in self.script.items():
            if key in command:
                return value
        return ("", "", 0)


def make_env(tmp_path, **overrides):
    cfg = AgentSandboxConfig(
        router_url="http://router:1",
        token_file=str(tmp_path / "tok"),
        **overrides,
    )
    return cfg


def test_lazy_claim_creation_and_attach(tmp_path):
    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    assert claims.created == []  # nothing until first command
    env.execute("echo hi")
    assert len(claims.created) == 1
    assert transport.attached == [("sbx-1", "uid-1", "10.0.0.5")]
    assert claims.waited == [(claims.created[0], 120)]


def test_execute_success_contract(tmp_path):
    claims, transport = FakeClaims(), FakeTransport(
        {"echo hi": ("hi\n", "", 0)}
    )
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    result = env.execute("echo hi")
    assert result == {"output": "hi\n", "returncode": 0, "cwd": "/workspace"}
    assert transport.runs[0][0] == "cd /workspace && echo hi"


def test_nonzero_exit_passthrough(tmp_path):
    claims, transport = FakeClaims(), FakeTransport(
        {"false": ("", "", 1)}
    )
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    assert env.execute("false")["returncode"] == 1


def test_stdout_stderr_merge(tmp_path):
    transport = FakeTransport({"both": ("out", "err", 2)})
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=transport
    )
    result = env.execute("both")
    assert result["output"] == "outerr"
    assert result["returncode"] == 2


def test_output_truncation_exact(tmp_path):
    limit = 100
    transport = FakeTransport({"big": ("x" * 500, "", 0)})
    env = AgentSandboxEnvironment(
        make_env(tmp_path, output_limit_bytes=limit),
        claims=FakeClaims(),
        transport=transport,
    )
    result = env.execute("big")
    assert result["output"].endswith(TRUNCATION_SUFFIX)
    assert len(result["output"]) == limit
    assert result["output"][: limit - len(TRUNCATION_SUFFIX)] == "x" * (limit - len(TRUNCATION_SUFFIX))


def test_under_limit_untouched(tmp_path):
    transport = FakeTransport({"small": ("tiny", "", 0)})
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=transport
    )
    assert env.execute("small")["output"] == "tiny"


def test_timeout_cancels_remote_before_124(tmp_path):
    class SlowTransport(FakeTransport):
        def run(self, command, timeout):
            time.sleep(1.2)
            raise RuntimeError("timed out")

    transport = SlowTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=transport
    )
    start = time.monotonic()
    result = env.execute("sleep 100", timeout=1)
    assert result["returncode"] == 124
    assert transport.cancelled == 1
    assert time.monotonic() - start < 5


def test_transport_failure_before_timeout_is_command_error(tmp_path):
    class FailingTransport(FakeTransport):
        def run(self, command, timeout):
            raise RuntimeError("connection reset")

    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FailingTransport()
    )
    with pytest.raises(SandboxCommandError):
        env.execute("any", timeout=30)


def test_stdin_unsupported(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    with pytest.raises(SandboxCommandError):
        env.execute("read x", stdin_data="data")


def test_unknown_kwargs_tolerated(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    assert env.execute("echo ok", some_future_key="x")["returncode"] == 0


def test_cwd_rules(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    with pytest.raises(CwdNotAllowedError):
        env.execute("pwd", cwd="relative/tmp")
    with pytest.raises(CwdNotAllowedError):
        env.execute("pwd", cwd="/etc")
    with pytest.raises(CwdNotAllowedError):
        env.execute("pwd", cwd="/workspace/../etc")


def test_cwd_within_workspace_used(tmp_path):
    transport = FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=transport
    )
    env.execute("pwd", cwd="/workspace/proj")
    assert transport.runs[0][0] == "cd /workspace/proj && pwd"
    env.execute("pwd")  # default /workspace
    assert transport.runs[1][0] == "cd /workspace && pwd"


def test_provider_style_creation_lazily_builds_transport(tmp_path, monkeypatch):
    """Provider-created envs pass no transport; the lazy init must attach."""
    import hermes_agent_sandbox.environment as env_mod

    class FakeRealTransport:
        def __init__(self, config, secret):
            self.secret = secret
            self.attached = []

        def attach(self, name, uid, pod_ip):
            self.attached.append((name, uid, pod_ip))

    monkeypatch.setattr(env_mod, "SandboxTransport", FakeRealTransport)
    (tmp_path / "tok").write_bytes(b"x" * 32)
    claims = FakeClaims()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), task_id="t-1", claims=claims  # no transport arg
    )
    assert env._transport is None
    env._ensure_ready()
    assert isinstance(env._transport, FakeRealTransport)
    assert env._transport.secret == b"x" * 32  # token loaded from file
    assert env._transport.attached == [("sbx-1", "uid-1", "10.0.0.5")]


def test_cleanup_deletes_claim_once_and_closes(tmp_path):
    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    env.execute("echo hi")  # creates claim
    env.cleanup()
    env.cleanup()  # idempotent
    assert claims.deleted == [claims.created[0]]
    assert transport.closed == 1
    # alias
    env2 = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    env2.execute("echo hi")
    env2.close()
    assert len(claims.deleted) == 2


def test_closed_environment_rejects_command(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    env.cleanup()
    with pytest.raises(SandboxCommandError):
        env.execute("echo hi")


def test_idle_recycle_recreates_claim(tmp_path, monkeypatch):
    clock = {"t": 1000.0}
    monkeypatch.setattr(
        "hermes_agent_sandbox.environment.time.monotonic", lambda: clock["t"]
    )
    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    env.execute("echo a")
    clock["t"] += 2000  # past the 1800s idle timeout
    env.execute("echo b")  # idle exceeded -> recycle + recreate
    assert len(claims.created) == 2
    assert len(claims.deleted) == 1
    assert len(transport.attached) == 2


def test_fetch_file_roundtrip(tmp_path):
    class ScriptedTransport(FakeTransport):
        def fetch_file(self, remote_path, timeout=300):
            if "/missing" in remote_path:
                raise SandboxCommandError(f"{remote_path!r} not found in the sandbox")
            return b"hello world\n" * 10

    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=ScriptedTransport()
    )
    dest = tmp_path / "out.bin"
    env.fetch_file("/workspace/f.bin", dest, max_bytes=100_000)
    assert dest.read_bytes() == b"hello world\n" * 10


def test_fetch_file_over_limit_rejected(tmp_path):
    class BigTransport(FakeTransport):
        def fetch_file(self, remote_path, timeout=300):
            return b"x" * 64

    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=BigTransport()
    )
    with pytest.raises(SandboxCommandError):
        env.fetch_file("/workspace/big", tmp_path / "o", max_bytes=10)


def test_fetch_file_missing_maps_to_command_error(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    with pytest.raises(SandboxCommandError):
        env.fetch_file("/workspace/missing", tmp_path / "o", max_bytes=1000)


def test_fetch_realpath(tmp_path):
    class ReadlinkTransport(FakeTransport):
        def run(self, command, timeout):
            if "readlink -f /workspace/x" in command:
                return ("/workspace/x\n", "", 0)
            return ("", "", 1)

    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=ReadlinkTransport()
    )
    assert env.fetch_realpath("/workspace/x") == "/workspace/x"
    assert env.fetch_realpath("/workspace/nope") is None


def test_temp_dir(tmp_path):
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=FakeClaims(), transport=FakeTransport()
    )
    assert env.get_temp_dir() == "/tmp"