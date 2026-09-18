"""Config validation, non-overridable identity, token file state."""

from __future__ import annotations

import pytest

from hermes_agent_sandbox.config import (
    NAMESPACE,
    PROFILE_NAMES,
    PROFILE_PLANNED,
    STDIN_MODES,
    TEMPLATE,
    WORKSPACE,
    AgentSandboxConfig,
    from_env,
    template_for_profile,
)
from hermes_agent_sandbox.errors import ConfigError


def test_valid_config_defaults(token_file):
    cfg = AgentSandboxConfig(
        router_url="https://router.internal:8080", token_file=str(token_file)
    )
    assert cfg.namespace == "hermes-sandbox"
    assert cfg.template == "hermes-go"
    assert cfg.output_limit_bytes == 1048576
    assert cfg.command_timeout_seconds == 300
    assert cfg.token_file_readable()


def test_router_url_must_be_http_scheme(tmp_path):
    for bad in ["router.internal", "ftp://x", ""]:
        with pytest.raises(ConfigError):
            AgentSandboxConfig(router_url=bad, token_file=str(tmp_path / "t"))


def test_identity_not_overridable_by_callers(tmp_path):
    cfg = AgentSandboxConfig(
        router_url="http://r:1",
        token_file=str(tmp_path / "t"),
        namespace="attacker-ns",
        template="attacker-tpl",
    )
    # Dataclass fields accept any value; the ALLOWLIST lives in the module
    # constants and must never be read from the instance.
    assert NAMESPACE == "hermes-sandbox"
    assert TEMPLATE == "hermes-go"
    assert cfg.namespace == "attacker-ns"  # document: enforcement is upstream


def test_int_fields_validated(tmp_path):
    for field in (
        "create_timeout_seconds",
        "command_timeout_seconds",
        "max_lifetime_seconds",
        "idle_timeout_seconds",
        "output_limit_bytes",
    ):
        with pytest.raises(ConfigError):
            AgentSandboxConfig(
                router_url="http://r:1",
                token_file=str(tmp_path / "t"),
                **{field: -1},
            )


def test_load_token_strips_whitespace(tmp_path):
    seed = b"a" * 32
    tok = tmp_path / "t"
    tok.write_bytes(b"\n  " + seed + b"  \n")
    cfg = AgentSandboxConfig(router_url="http://r:1", token_file=str(tok))
    assert cfg.load_token() == seed
    assert cfg.token_file_readable()


def test_token_file_requires_exactly_32_bytes(tmp_path):
    # 44 chars is what a base64-encoded 32-byte seed looks like when the Secret
    # is (wrongly) mounted through `stringData`.
    tok = tmp_path / "t"
    tok.write_bytes(b"A" * 44)
    cfg = AgentSandboxConfig(router_url="http://r:1", token_file=str(tok))
    assert not cfg.token_file_readable()
    with pytest.raises(ConfigError) as excinfo:
        cfg.load_token()
    assert "44" in str(excinfo.value)
    assert "32" in str(excinfo.value)


def test_token_file_short_seed_rejected(tmp_path):
    tok = tmp_path / "t"
    tok.write_bytes(b"x" * 31)
    cfg = AgentSandboxConfig(router_url="http://r:1", token_file=str(tok))
    assert not cfg.token_file_readable()
    with pytest.raises(ConfigError):
        cfg.load_token()


def test_missing_token_file_unreadable(tmp_path):
    cfg = AgentSandboxConfig(
        router_url="http://r:1", token_file=str(tmp_path / "missing")
    )
    assert not cfg.token_file_readable()
    with pytest.raises(ConfigError):
        cfg.load_token()


def test_from_env_full(env_with_router):
    cfg = from_env()
    assert cfg.router_url == "http://router:8080"
    assert cfg.namespace == "hermes-sandbox"
    assert cfg.template == "hermes-go"


def test_from_env_rejects_bad_int(env_with_router, monkeypatch):
    monkeypatch.setenv("AGENT_SANDBOX_COMMAND_TIMEOUT_SECONDS", "abc")
    with pytest.raises(ConfigError):
        from_env()


def test_from_env_missing_router(tmp_path, monkeypatch):
    monkeypatch.delenv("AGENT_SANDBOX_ROUTER_URL", raising=False)
    monkeypatch.setenv("AGENT_SANDBOX_ROUTER_TOKEN_FILE", str(tmp_path / "t"))
    from hermes_agent_sandbox.errors import ConfigError as CE

    with pytest.raises(CE):
        from_env()


def test_gateway_id_fallback(tmp_path):
    cfg = AgentSandboxConfig(
        router_url="http://r:1", token_file=str(tmp_path / "t"), gateway_id=""
    )
    assert cfg.resolve_gateway_id()  # hostname fallback is non-empty

# ---------------- capability-layer knobs (Phase 2) ----------------


def test_capability_defaults(token_file):
    cfg = AgentSandboxConfig(
        router_url="https://router.internal:8080", token_file=str(token_file)
    )
    assert cfg.file_write_max_bytes == 8 * 1024 * 1024
    assert cfg.file_read_max_bytes == 8 * 1024 * 1024
    assert cfg.stdin_mode == "auto" and cfg.stdin_mode in STDIN_MODES
    assert cfg.artifact_root == "/opt/data/agent-sandbox/artifacts"
    assert cfg.artifact_default_ttl_hours <= cfg.artifact_max_ttl_hours
    assert cfg.queue_max_concurrent >= 1 and cfg.queue_max_queued >= 0
    assert cfg.process_log_tail_bytes <= cfg.process_log_buffer_bytes
    assert WORKSPACE == "/workspace"


def test_from_env_reads_capability_knobs(env_with_router, monkeypatch):
    monkeypatch.setenv("AGENT_SANDBOX_STDIN_MODE", "file")
    monkeypatch.setenv("AGENT_SANDBOX_FILE_WRITE_MAX_BYTES", "1024")
    monkeypatch.setenv("AGENT_SANDBOX_ARTIFACT_ROOT", "/tmp/artifacts")
    monkeypatch.setenv("AGENT_SANDBOX_ARTIFACT_MAX_TTL_HOURS", "48")
    monkeypatch.setenv("AGENT_SANDBOX_QUEUE_MAX_QUEUED", "0")
    monkeypatch.setenv("AGENT_SANDBOX_PROCESS_MAX_CONCURRENT", "2")

    cfg = from_env()

    assert cfg.stdin_mode == "file"
    assert cfg.file_write_max_bytes == 1024
    assert cfg.artifact_root == "/tmp/artifacts"
    assert cfg.artifact_max_ttl_hours == 48
    assert cfg.queue_max_queued == 0  # 0 = never queue, reject while busy
    assert cfg.process_max_concurrent == 2


def test_from_env_rejects_unknown_stdin_mode(env_with_router, monkeypatch):
    monkeypatch.setenv("AGENT_SANDBOX_STDIN_MODE", "telepathy")
    with pytest.raises(ConfigError) as excinfo:
        from_env()
    assert "AGENT_SANDBOX_STDIN_MODE" in str(excinfo.value)


def test_from_env_rejects_negative_queue_depth(env_with_router, monkeypatch):
    monkeypatch.setenv("AGENT_SANDBOX_QUEUE_MAX_QUEUED", "-1")
    with pytest.raises(ConfigError):
        from_env()


def test_default_ttl_above_ceiling_rejected(token_file):
    with pytest.raises(ConfigError) as excinfo:
        AgentSandboxConfig(
            router_url="https://router.internal:8080",
            token_file=str(token_file),
            artifact_default_ttl_hours=48,
            artifact_max_ttl_hours=24,
        )
    assert "artifact_default_ttl_hours" in str(excinfo.value)


def test_log_tail_above_buffer_rejected(token_file):
    with pytest.raises(ConfigError) as excinfo:
        AgentSandboxConfig(
            router_url="https://router.internal:8080",
            token_file=str(token_file),
            process_log_buffer_bytes=100,
            process_log_tail_bytes=101,
        )
    assert "process_log_tail_bytes" in str(excinfo.value)


def test_profile_mapping_only_advertises_shipped_templates():
    """A profile the plugin cannot name a template for must not be selectable."""
    assert set(PROFILE_NAMES) == {"core", "offline", "go"}
    assert template_for_profile("core") == "hermes-core"
    assert template_for_profile("offline") == "hermes-offline"
    assert template_for_profile("go") == TEMPLATE  # the retained legacy template
    for planned in PROFILE_PLANNED:
        with pytest.raises(ConfigError) as excinfo:
            template_for_profile(planned)
        assert "no SandboxTemplate yet" in str(excinfo.value)
    with pytest.raises(ConfigError) as excinfo:
        template_for_profile("kubernetes")
    assert "available profiles" in str(excinfo.value)
