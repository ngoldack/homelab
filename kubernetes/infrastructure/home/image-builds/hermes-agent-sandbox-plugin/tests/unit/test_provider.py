"""Provider classification, availability, creation, doctor rows."""

from __future__ import annotations

from types import SimpleNamespace

from hermes_agent_sandbox.environment import AgentSandboxEnvironment
from hermes_agent_sandbox.provider import (
    BACKEND_NAME,
    SANDBOX_ENV_FACTS,
    AgentSandboxProvider,
    register,
)


def test_module_register_registers_backend_and_prompt_section():
    ctx = SimpleNamespace(
        registered=[], sections={}, register_system_prompt_section=None
    )

    def _reg_provider(p):
        ctx.registered.append(p)

    def _reg_section(section_id, content, **kwargs):
        ctx.sections[section_id] = (content, kwargs)

    ctx.register_terminal_environment_provider = _reg_provider
    ctx.register_system_prompt_section = _reg_section
    register(ctx)
    assert len(ctx.registered) == 1
    assert isinstance(ctx.registered[0], AgentSandboxProvider)
    assert "agent_sandbox_env" in ctx.sections
    content, kwargs = ctx.sections["agent_sandbox_env"]
    assert content == SANDBOX_ENV_FACTS
    assert kwargs.get("position") == "after_memory"


def test_isolation_classification():
    p = AgentSandboxProvider()
    assert p.name == "agent_sandbox"
    assert BACKEND_NAME == "agent_sandbox"
    assert p.is_remote is True
    assert p.is_container is True
    assert p.session_isolated_when_nonpersistent is True
    assert p.skip_container_guards is True
    assert p.display_name  # non-empty


def test_unavailable_without_router_env(monkeypatch):
    monkeypatch.delenv("AGENT_SANDBOX_ROUTER_URL", raising=False)
    monkeypatch.delenv("AGENT_SANDBOX_ROUTER_TOKEN_FILE", raising=False)
    p = AgentSandboxProvider()
    assert not p.is_available()
    assert not p.check_requirements({})


def test_available_with_env(env_with_router):
    p = AgentSandboxProvider()
    assert p.is_available()
    assert p.check_requirements({})
    rows = p.doctor_checks()
    assert len(rows) == 4
    assert rows[0][0] is True  # config present
    assert rows[0][1] == "agent_sandbox config"
    token_row = [r for r in rows if r[1] == "agent_sandbox token file (32 B Ed25519 seed)"]
    assert token_row and token_row[0][0] is True


def test_doctor_token_size_row_fails_for_44_byte_file(tmp_path, monkeypatch):
    tok = tmp_path / "tok"
    tok.write_bytes(b"A" * 44)  # base64 of a 32-byte seed, e.g. via stringData
    monkeypatch.setenv("AGENT_SANDBOX_ROUTER_URL", "http://router:8080")
    monkeypatch.setenv("AGENT_SANDBOX_ROUTER_TOKEN_FILE", str(tok))
    p = AgentSandboxProvider()
    assert not p.is_available()
    rows = p.doctor_checks()
    token_row = [r for r in rows if r[1] == "agent_sandbox token file (32 B Ed25519 seed)"]
    assert token_row and token_row[0][0] is False
    assert "32" in token_row[0][2]


def test_doctor_reachability_row_uses_probe_result(env_with_router, monkeypatch):
    monkeypatch.setattr(
        AgentSandboxProvider,
        "_probe_router",
        lambda self, cfg: (False, "connection refused"),
    )
    rows = AgentSandboxProvider().doctor_checks()
    reach = [r for r in rows if r[1] == "agent_sandbox router reachability"]
    assert reach and reach[0][0] is False
    assert reach[0][2] == "connection refused"
    auth = [r for r in rows if r[1] == "agent_sandbox router auth"]
    assert auth and auth[0][0] is False


def test_strip_env_keys_covers_live_credentials(monkeypatch, env_with_router):
    monkeypatch.setenv("X_OPENROUTER_API_KEY", "sk-live")
    monkeypatch.setenv("HOME", "/home/x")
    keys = AgentSandboxProvider().strip_env_keys
    assert "X_OPENROUTER_API_KEY" in keys
    assert "HOME" not in keys


def test_create_environment_scopes_task(env_with_router):
    p = AgentSandboxProvider()
    env = p.create_environment(
        cwd="/workspace/proj",
        timeout=99,
        task_id="task-42",
        image="ignored",
        container_config={"x": 1},
        future_kwarg=True,
    )
    assert isinstance(env, AgentSandboxEnvironment)
    assert env.task_id == "task-42"
    assert env.cwd == "/workspace/proj"
    assert env.timeout == 99


def test_env_description_informs_about_sandbox():
    desc = AgentSandboxProvider().env_description()
    assert desc == SANDBOX_ENV_FACTS
    assert "Kata" in desc
    assert "/workspace" in desc
    assert "Go 1.26.5" in desc
    assert "DNS only" in desc  # no external egress — honest about it
    assert "PyPI" not in desc or "no" in desc  # no download promises


def test_register_wires_provider(env_with_router):
    ctx = SimpleNamespace(registered=[])
    ctx.register_terminal_environment_provider = ctx.registered.append
    p = AgentSandboxProvider()
    p.register(ctx)
    assert ctx.registered == [p]


def test_doctor_reports_missing_config_first(monkeypatch):
    monkeypatch.delenv("AGENT_SANDBOX_ROUTER_URL", raising=False)
    rows = AgentSandboxProvider().doctor_checks()
    assert len(rows) == 1
    assert rows[0] == (
        False,
        "agent_sandbox config",
        "AGENT_SANDBOX_ROUTER_URL/TOKEN_FILE missing",
    )


def _register_ctx():
    ctx = SimpleNamespace(registered=[], sections={})
    ctx.register_terminal_environment_provider = ctx.registered.append
    ctx.register_system_prompt_section = (
        lambda section_id, content, **kwargs: ctx.sections.__setitem__(
            section_id, (content, kwargs)
        )
    )
    return ctx


def test_register_runs_orphan_sweep_once(env_with_router, monkeypatch):
    import hermes_agent_sandbox.provider as provider

    calls = []

    class FakeClaimsClient:
        def __init__(self, cfg):
            self.cfg = cfg

        def reconcile_orphans(self, gateway_id):
            calls.append(gateway_id)
            return []

    monkeypatch.setattr(provider, "KubeClaimsClient", FakeClaimsClient)
    monkeypatch.setattr(provider, "_reconciled", False)
    monkeypatch.setenv("AGENT_SANDBOX_GATEWAY_ID", "hermes")
    ctx = _register_ctx()
    register(ctx)
    assert len(ctx.registered) == 1
    second = _register_ctx()
    register(second)  # registers again, but the sweep is once per process
    assert calls == ["hermes"]
    assert len(second.registered) == 1


def test_register_survives_sweep_failure(env_with_router, monkeypatch):
    import hermes_agent_sandbox.provider as provider

    class ExplodingClaimsClient:
        def __init__(self, cfg):
            self.cfg = cfg

        def reconcile_orphans(self, gateway_id):
            raise RuntimeError("apiserver unreachable")

    monkeypatch.setattr(provider, "KubeClaimsClient", ExplodingClaimsClient)
    monkeypatch.setattr(provider, "_reconciled", False)
    ctx = _register_ctx()
    register(ctx)  # must not raise
    assert len(ctx.registered) == 1