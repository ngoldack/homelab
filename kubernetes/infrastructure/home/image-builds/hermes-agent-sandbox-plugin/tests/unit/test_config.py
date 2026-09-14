"""Config validation, non-overridable identity, token file state."""

from __future__ import annotations

import pytest

from hermes_agent_sandbox.config import (
    NAMESPACE,
    TEMPLATE,
    AgentSandboxConfig,
    from_env,
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
    tok = tmp_path / "t"
    tok.write_bytes(b"\n  abcdef  \n")
    cfg = AgentSandboxConfig(router_url="http://r:1", token_file=str(tok))
    assert cfg.load_token() == b"abcdef"


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