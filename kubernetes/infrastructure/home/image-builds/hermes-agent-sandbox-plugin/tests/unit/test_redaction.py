"""Secret-name detection, allowlist passthrough, strip enumeration."""

from __future__ import annotations

import pytest

from hermes_agent_sandbox.redaction import (
    ENV_ALLOWLIST,
    is_secret_name,
    sanitize_env,
    strip_env_names,
)


@pytest.mark.parametrize(
    "name",
    [
        "OPENROUTER_API_KEY",
        "GITHUB_TOKEN",
        "aws_secret_access_key",
        "AUTHORIZATION",
        "MYSQL_PASSWORD",
        "SERVICE_ACCOUNT_CREDENTIALS",
    ],
)
def test_secret_names_detected(name):
    assert is_secret_name(name)


@pytest.mark.parametrize("name", ["HOME", "PATH", "TZ", "LANG", "USER"])
def test_benign_names_not_secret(name):
    assert not is_secret_name(name)


def test_sanitize_keeps_only_allowlist(token_file):
    env = {
        "HOME": "/root",
        "PATH": "/usr/bin",
        "OPENROUTER_API_KEY": "sk-x",
        "RANDOM_APP_VAR": "v",
        "AGENT_SANDBOX_ROUTER_URL": "http://r",
    }
    clean = sanitize_env(env)
    assert clean == {"HOME": "/root", "PATH": "/usr/bin"}


def test_sanitize_none():
    assert sanitize_env(None) == {}


def test_secret_shaped_allowlist_name_still_dropped():
    # Even if someone adds a secret-shaped name to the allowlist, the regex
    # defense-in-depth layer must drop it.
    assert is_secret_name("TERM_TOKEN")
    assert "TERM_TOKEN" not in ENV_ALLOWLIST


def test_strip_env_names():
    names = strip_env_names({"X_API_KEY": "1", "HOME": "2", "TZ": "3"})
    assert names == frozenset({"X_API_KEY"})