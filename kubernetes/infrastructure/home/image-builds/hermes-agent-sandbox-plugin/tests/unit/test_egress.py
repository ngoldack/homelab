"""Egress identity + proxy env (Phase 3, Unit 3.5).

Covers the byte-level token contract with the guard (auth.py is the
authoritative layout), the stable session identity stamped on claims, and the
admission-denial -> permanent quarantine error mapping.
"""

from __future__ import annotations

import base64
import threading
import time

import pytest

from hermes_agent_sandbox import egress
from hermes_agent_sandbox.config import AgentSandboxConfig
from hermes_agent_sandbox.environment import (
    AgentSandboxEnvironment,
    _is_quarantine_denial,
)
from hermes_agent_sandbox.errors import (
    SandboxCreateError,
    SessionQuarantinedError,
)


def make_config(tmp_path, **overrides):
    cfg = AgentSandboxConfig(
        router_url="http://router.svc.cluster.local:8080",
        token_file=str(tmp_path / "router-token"),
        **overrides,
    )
    return cfg


# ---------------- token byte contract ----------------

def test_token_layout_matches_guard_contract():
    """v1.<expiry>.<profile>.<mac> with the exact HMAC input auth.py defines."""
    secret = b"0123456789abcdef0123456789abcdef"
    token = egress.mint_token(secret, "abc123", "go", 1700000000)
    prefix, expiry, profile, mac = token.split(".")
    assert prefix == "v1"
    assert expiry == "1700000000"  # ASCII decimal
    assert profile == "go"
    # Byte-for-byte HMAC over egress-token-v1|<hash>|<expiry>|<profile>.
    import hashlib
    import hmac as hmac_mod

    mac_input = b"|".join(
        [b"egress-token-v1", b"abc123", b"1700000000", b"go"]
    )
    expected = (
        base64.urlsafe_b64encode(
            hmac_mod.new(secret, mac_input, hashlib.sha256).digest()
        )
        .rstrip(b"=")
        .decode("ascii")
    )
    assert mac == expected


def test_mint_token_rejects_bad_session_hash_and_profile():
    secret = b"s" * 32
    with pytest.raises(ValueError):
        egress.mint_token(secret, "bad hash!", "go", 1700000000)
    with pytest.raises(ValueError):
        egress.mint_token(secret, "abc", "BAD_PROFILE", 1700000000)


def test_proxy_env_embeds_token_and_url(tmp_path):
    cfg = make_config(tmp_path)
    env = egress.proxy_env(cfg, "abc123", "go", b"k" * 32, now=1000.0)
    url = env["HTTP_PROXY"]
    # <session-hash>:<token>@<proxy host>:<port> — the guest's HTTP stack
    # turns the userinfo into Proxy-Authorization on the CONNECT.
    assert url.startswith("http://abc123:v1.")
    assert f"@{egress.PROXY_HOST}:{egress.PROXY_PORT}" in url
    for key in ("HTTPS_PROXY", "http_proxy", "https_proxy"):
        assert env[key] == url
    # The guard's own REST/gRPC peers never route through the tunnel.
    assert ".svc.cluster.local" in env["NO_PROXY"]


def test_proxy_token_expiry_is_now_plus_ttl(tmp_path):
    cfg = make_config(
        tmp_path, egress_token_ttl_seconds=3600, max_lifetime_seconds=1800
    )
    secret = b"k" * 32
    env = egress.proxy_env(cfg, "abc123", "go", secret, now=1000.0)
    password = env["HTTP_PROXY"].split(":v1.", 1)[1].split("@", 1)[0]
    expiry = int(password.split(".", 1)[0])
    assert expiry == 1000 + 3600


# ---------------- config knobs ----------------

def test_egress_secret_missing_knob_disables_identity(tmp_path):
    cfg = make_config(tmp_path)
    assert cfg.load_egress_secret() is None


def test_egress_secret_configured_file_loads(tmp_path):
    secret_file = tmp_path / "hmac"
    secret_file.write_bytes(b"raw-secret-bytes\n")
    cfg = make_config(
        tmp_path, egress_hmac_secret_file=str(secret_file)
    )
    assert cfg.load_egress_secret() == b"raw-secret-bytes"


def test_egress_secret_empty_file_is_loud(tmp_path):
    secret_file = tmp_path / "hmac"
    secret_file.write_bytes(b"   ")
    cfg = make_config(
        tmp_path, egress_hmac_secret_file=str(secret_file)
    )
    from hermes_agent_sandbox.errors import ConfigError

    with pytest.raises(ConfigError):
        cfg.load_egress_secret()


def test_egress_secret_unreadable_file_is_loud(tmp_path):
    cfg = make_config(
        tmp_path, egress_hmac_secret_file=str(tmp_path / "missing")
    )
    from hermes_agent_sandbox.errors import ConfigError

    with pytest.raises(ConfigError):
        cfg.load_egress_secret()


def test_token_ttl_below_claim_lifetime_rejected(tmp_path):
    # An expiry inside the claim lifetime answers 403 token-expired
    # mid-session — self-inflicted, so it is a config error at construction.
    from hermes_agent_sandbox.errors import ConfigError

    with pytest.raises(ConfigError) as excinfo:
        make_config(tmp_path, egress_token_ttl_seconds=3600)
    assert "max_lifetime_seconds" in str(excinfo.value)


def test_token_ttl_above_guard_ceiling_rejected(tmp_path):
    from hermes_agent_sandbox.errors import ConfigError

    with pytest.raises(ConfigError) as excinfo:
        make_config(
            tmp_path,
            egress_token_ttl_seconds=86400 * 7,
            max_lifetime_seconds=86400 * 7,
        )
    assert "86400" in str(excinfo.value)


def test_unknown_egress_profile_rejected(tmp_path):
    from hermes_agent_sandbox.errors import ConfigError

    with pytest.raises(ConfigError) as excinfo:
        make_config(tmp_path, egress_profile="cobol")
    assert "egress_profile" in str(excinfo.value)


# ---------------- session identity on claims ----------------

def test_session_hash_is_stable_and_labeled(tmp_path):
    """One environment keeps ONE hash across claim recycles; it is stamped
    as the workload.hermes.io/session-hash label on every create_claim."""
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    env.execute("echo one")
    first = env._session_hash
    assert first
    assert len(first) == 32
    assert claims.created_labels == {egress.SESSION_HASH_LABEL: first}
    # Recycle: the hash survives (one session == one identity).
    env._fresh_claim()
    assert env._session_hash == first
    assert claims.created_labels == {egress.SESSION_HASH_LABEL: first}


def test_session_hash_is_stable_concurrently(tmp_path):
    """Two racing executes mint ONE hash (the lock serializes claim create)."""
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    threads = [
        threading.Thread(target=env.execute, args=("echo hi",)) for _ in range(2)
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(10)
    assert len(claims.created) == 1
    assert env._session_hash


def test_no_secret_submits_no_proxy_env(tmp_path):
    """An unset HMAC knob submits NO env (Cilium still confines the guest)."""
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    env.execute("echo hi")
    assert getattr(transport, "env", None) is None


def test_secret_injects_proxy_env_per_command(tmp_path):
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    secret_file = tmp_path / "hmac"
    secret_file.write_bytes(b"raw-secret")
    cfg = make_env(tmp_path, egress_hmac_secret_file=str(secret_file))
    claims, transport = FakeClaims(), FakeTransport()
    env = AgentSandboxEnvironment(cfg, claims=claims, transport=transport)
    env.execute("echo hi")
    env_dict = getattr(transport, "env", None)
    assert env_dict is not None
    url = env_dict["HTTP_PROXY"]
    assert url.startswith(f"http://{env._session_hash}:v1.")
    assert f"@{egress.PROXY_HOST}:{egress.PROXY_PORT}" in url
    # The token rides the env; the secret itself never crosses the exec
    # channel.
    assert all("raw-secret" != value for value in env_dict.values())


# ---------------- quarantine mapping ----------------

def test_quarantine_denial_maps_to_permanent_error(tmp_path):
    """A Kyverno hermes-session-quarantine denial becomes the PERMANENT
    SessionQuarantinedError, never a transient create error."""
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    class QuarantinedClaims(FakeClaims):
        def create_claim(self, name, pod_labels=None):
            raise SandboxCreateError(
                "admission webhook denied: Policy hermes-session-quarantine: "
                "session is quarantined by the hermes-egress reaper "
                "(ConfigMap hermes-quarantine/hermes-sandbox)"
            )

    claims, transport = QuarantinedClaims(), FakeTransport()
    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=claims, transport=transport
    )
    with pytest.raises(SessionQuarantinedError) as excinfo:
        env.execute("echo hi")
    assert env._session_hash in str(excinfo.value)
    assert "hermes-quarantine" in str(excinfo.value)


def test_unrelated_create_failure_stays_transient(tmp_path):
    """A capacity/transport create failure keeps its RETRYABLE type: the
    quarantine mapping must not swallow unrelated admission failures."""
    from test_environment import FakeClaims, FakeTransport, make_env  # noqa: F401 - suite helper

    class BrokenClaims(FakeClaims):
        def create_claim(self, name, pod_labels=None):
            raise SandboxCreateError("warm pool hermes-go is at capacity")

    env = AgentSandboxEnvironment(
        make_env(tmp_path), claims=BrokenClaims(), transport=FakeTransport()
    )
    with pytest.raises(SandboxCreateError) as excinfo:
        env.execute("echo hi")
    assert not isinstance(excinfo.value, SessionQuarantinedError)
    # No session hash leaked into the message (not a quarantine text).
    assert "hermes-quarantine" not in str(excinfo.value)


def test_marker_matcher_is_conjunction_not_any():
    assert _is_quarantine_denial(
        SandboxCreateError(
            "denied by Policy hermes-session-quarantine (hermes-quarantine)"
        )
    )
    # Only one marker: not a quarantine denial.
    assert not _is_quarantine_denial(
        SandboxCreateError("Policy hermes-session-quarantine matched")
    )
    assert not _is_quarantine_denial(
        SandboxCreateError("quota exceeded in hermes-quarantine-ns")
    )
