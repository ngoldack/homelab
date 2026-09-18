"""Rule-table tests for the deterministic egress policy engine (plan Unit 3.2).

These pin the security-relevant contract of ``egress_guard.policy``: the
decision ORDER is the enforcement boundary, so every table row that a
mis-edit could silently widen is asserted here.

The engine is intentionally clock-injected and side-effect free, so every
test drives it with explicit ``now=`` timestamps and a synthetic policy; no
wall clock, no network, no cluster.
"""

from __future__ import annotations

import json

import pytest

from egress_guard.policy import (
    ALLOW,
    DENY,
    KILL,
    EgressEngine,
    Identity,
    PolicyError,
    ProfilePolicy,
    AllowEntry,
    load_policy,
    sessions_seen,
)


def _policy(**overrides):
    """A two-profile policy: an allowlisting profile and an empty one."""
    body = {
        "version": "test-1",
        "profiles": {
            "python": {
                "allow": [
                    {"host": "pypi.org", "ports": [443]},
                    {"host": ".pythonhosted.org", "ports": [443]},
                ]
            },
            "offline": {"allow": []},
        },
    }
    body.update(overrides)
    return body


@pytest.fixture()
def policy_file(tmp_path):
    def _write(body):
        path = tmp_path / "policy.json"
        path.write_text(json.dumps(body), encoding="utf-8")
        return str(path)

    return _write


@pytest.fixture()
def engine(policy_file):
    return EgressEngine(load_policy(policy_file(_policy())), strike_threshold=3, strike_window_s=60.0)


PY = Identity(session_hash="s" * 64, profile="python")
OFFLINE = Identity(session_hash="o" * 64, profile="offline")


# --- decision order: 1. unauthenticated ------------------------------------


def test_unauthenticated_is_denied_without_strike(engine):
    first = engine.evaluate(None, "pypi.org", 443, now=0)
    second = engine.evaluate(None, "pypi.org", 443, now=0)

    assert (first.kind, first.reason, first.status) == (DENY, "unauthenticated", 403)
    assert (second.kind, second.strikes) == (DENY, 0)


# --- decision order: 2. instant-kill destinations --------------------------


@pytest.mark.parametrize(
    "host",
    [
        "169.254.169.254",  # cloud metadata, named in the plan
        "10.1.2.3",  # RFC1918
        "192.168.1.1",  # RFC1918
        "127.0.0.1",  # loopback
        "100.64.0.1",  # CGNAT
        "::1",  # IPv6 loopback
        "fd00::1",  # IPv6 ULA
    ],
)
def test_builtin_kill_destinations(engine, host):
    decision = engine.evaluate(PY, host, 443, now=0)

    assert (decision.kind, decision.reason, decision.status) == (KILL, "kill-destination", 403)


def test_kill_wins_over_an_allowlist_entry(policy_file):
    """Even a policy that tries to allowlist the management plane loses."""
    policy = _policy(
        profiles={"python": {"allow": [{"host": "10.0.0.5", "ports": [443]}]}}
    )
    engine = EgressEngine(load_policy(policy_file(policy)))

    assert engine.evaluate(PY, "10.0.0.5", 443, now=0).kind == KILL


def test_policy_kill_cidrs_extend_the_builtin_baseline(policy_file):
    policy = _policy(kill_cidrs=["172.20.0.0/16"], kill_exact=["203.0.113.7"])
    engine = EgressEngine(load_policy(policy_file(policy)))

    assert engine.evaluate(PY, "172.20.5.5", 443, now=0).kind == KILL
    assert engine.evaluate(PY, "203.0.113.7", 443, now=0).kind == KILL
    # A hostname is never resolved here (DNS-rebinding defence is Cilium's).
    assert engine.evaluate(PY, "pypi.org", 443, now=0).kind == ALLOW


def test_hostname_that_resembles_an_ip_is_not_killed(engine):
    """Only IP literals take the kill path; a name is judged by the allowlist."""
    assert engine.evaluate(PY, "10.0.0.1.example.com", 443, now=0).kind == DENY


# --- decision order: 3. unknown profile ------------------------------------


def test_unknown_profile_is_denied(engine):
    stranger = Identity(session_hash="x" * 64, profile="ruby")

    decision = engine.evaluate(stranger, "pypi.org", 443, now=0)

    assert (decision.kind, decision.reason) == (DENY, "profile-unknown")


def test_unknown_profile_denial_does_not_strike(engine):
    stranger = Identity(session_hash="x" * 64, profile="ruby")

    for tick in range(5):
        assert engine.evaluate(stranger, "pypi.org", 443, now=tick).strikes == 0


# --- decision order: 4. allowlist -----------------------------------------


def test_allowlisted_host_is_allowed(engine):
    decision = engine.evaluate(PY, "pypi.org", 443, now=0)

    assert (decision.kind, decision.reason, decision.status) == (ALLOW, "allowed", 200)


def test_suffix_entry_matches_subdomain_and_bare_domain(policy_file):
    policy = _policy(profiles={"python": {"allow": [{"host": ".pythonhosted.org", "ports": [443]}]}})
    engine = EgressEngine(load_policy(policy_file(policy)))

    assert engine.evaluate(PY, "files.pythonhosted.org", 443, now=0).kind == ALLOW
    assert engine.evaluate(PY, "pythonhosted.org", 443, now=0).kind == ALLOW
    # Not a match: the suffix entry must not behave like a substring rule.
    assert engine.evaluate(PY, "evilpythonhosted.org", 443, now=0).kind == DENY


def test_host_matching_is_case_and_trailing_dot_insensitive(engine):
    assert engine.evaluate(PY, "PyPI.org.", 443, now=0).kind == ALLOW


def test_port_must_be_allowlisted(engine):
    decision = engine.evaluate(PY, "pypi.org", 22, now=0)

    assert (decision.kind, decision.reason) == (DENY, "not-allowlisted")


def test_empty_allowlist_profile_denies_everything(engine):
    assert engine.evaluate(OFFLINE, "pypi.org", 443, now=0).kind == DENY


# --- strikes and escalation ------------------------------------------------


def test_denials_accumulate_strikes_and_the_threshold_kills(engine):
    first = engine.evaluate(PY, "evil.example", 443, now=0)
    second = engine.evaluate(PY, "evil.example", 443, now=1)
    third = engine.evaluate(PY, "evil.example", 443, now=2)

    assert (first.kind, first.strikes) == (DENY, 1)
    assert (second.kind, second.strikes) == (DENY, 2)
    # The strike that reaches the threshold escalates the response itself.
    assert (third.kind, third.reason, third.strikes) == (KILL, "strikes", 3)


def test_strikes_expire_outside_the_window(engine):
    engine.evaluate(PY, "evil.example", 443, now=0)
    engine.evaluate(PY, "evil.example", 443, now=30)

    # A 60s window measured back from 91 discards both earlier strikes (0 and
    # 30 are < 31), so this denial starts a fresh count.
    decision = engine.evaluate(PY, "evil.example", 443, now=91)

    assert (decision.kind, decision.strikes) == (DENY, 1)


def test_strikes_are_per_session(engine):
    other = Identity(session_hash="p" * 64, profile="python")
    engine.evaluate(PY, "evil.example", 443, now=0)
    engine.evaluate(PY, "evil.example", 443, now=0)

    assert engine.evaluate(other, "evil.example", 443, now=0).strikes == 1


def test_allowed_request_does_not_strike(engine):
    engine.evaluate(PY, "evil.example", 443, now=0)

    allowed = engine.evaluate(PY, "pypi.org", 443, now=0)
    after = engine.evaluate(PY, "evil.example", 443, now=0)

    assert allowed.kind == ALLOW
    assert after.strikes == 2  # the allow neither added nor cleared a strike


def test_kill_destination_also_strikes(engine):
    """An instant-kill target is still counted, so the reaper sees escalation."""
    first = engine.evaluate(PY, "169.254.169.254", 443, now=0)
    second = engine.evaluate(PY, "169.254.169.254", 443, now=0)

    assert (first.kind, first.strikes) == (KILL, 1)
    assert second.strikes == 2


def test_session_table_is_bounded(engine):
    for index in range(engine.max_sessions + 50):
        engine.evaluate(Identity(session_hash=f"{index:064d}", profile="python"), "pypi.org", 443, now=index)

    assert sessions_seen(engine) <= engine.max_sessions


# --- decision order: 5. byte budget ---------------------------------------


def test_byte_budget_exhaustion_denies(policy_file):
    policy = _policy(
        profiles={"python": {"allow": [{"host": "pypi.org", "ports": [443]}], "budget_bytes": 1000}}
    )
    engine = EgressEngine(load_policy(policy_file(policy)))

    assert engine.evaluate(PY, "pypi.org", 443, byte_hint=600, now=0).kind == ALLOW
    assert engine.evaluate(PY, "pypi.org", 443, byte_hint=600, now=0).kind == DENY
    exhausted = engine.evaluate(PY, "pypi.org", 443, byte_hint=1, now=0)
    assert (exhausted.kind, exhausted.reason) == (DENY, "budget-exhausted")


def test_budget_without_hint_is_not_consumed(policy_file):
    policy = _policy(
        profiles={"python": {"allow": [{"host": "pypi.org", "ports": [443]}], "budget_bytes": 1000}}
    )
    engine = EgressEngine(load_policy(policy_file(policy)))

    for _ in range(10):
        assert engine.evaluate(PY, "pypi.org", 443, now=0).kind == ALLOW


# --- policy file validation ------------------------------------------------


def test_load_policy_reads_the_shipped_schema(policy_file):
    policy = load_policy(policy_file(_policy()))

    assert policy.version == "test-1"
    assert set(policy.profiles) == {"python", "offline"}
    # The builtin baseline is always armed, whatever the file says.
    assert policy.kill_reason("10.0.0.1") == "kill-destination"
    assert policy.kill_reason("pypi.org") is None


@pytest.mark.parametrize(
    "body",
    [
        {"profiles": {"a": {"allow": []}}},  # missing version
        {"version": "", "profiles": {"a": {"allow": []}}},  # empty version
        {"version": "1"},  # missing profiles
        {"version": "1", "profiles": {}},  # empty profiles
        {"version": "1", "profiles": {"a": {"allow": []}}, "extra": 1},  # unknown top key
        {"version": "1", "profiles": {"a": {"allow": [], "budget": 1}}},  # unknown profile key
        {"version": "1", "profiles": {"a": {"allow": [{"host": "x", "ports": []}]}}},  # empty ports
        {"version": "1", "profiles": {"a": {"allow": [{"host": "x", "ports": [0]}]}}},  # port range
        {"version": "1", "profiles": {"a": {"allow": [{"host": "x", "ports": [65536]}]}}},  # port range
        {"version": "1", "profiles": {"a": {"allow": [{"host": 5, "ports": [443]}]}}},  # host type
        {"version": "1", "profiles": {"a": {"allow": [{"host": "x..", "ports": [443]}]}}},  # malformed
        {"version": "1", "profiles": {"a": {"allow": [], "budget_bytes": 0}}},  # non-positive
        {"version": "1", "profiles": {"a": {"allow": [], "budget_bytes": "100"}}},  # non-int
        {"version": "1", "profiles": {"a": {"allow": []}}, "kill_cidrs": "10.0.0.0/8"},  # not a list
        {"version": "1", "profiles": {"a": {"allow": []}}, "kill_exact": ["nope"]},  # not an IP
    ],
)
def test_invalid_policies_are_rejected(policy_file, body):
    with pytest.raises(PolicyError):
        load_policy(policy_file(body))


def test_ipv4_mapped_ipv6_kill_address_is_caught(engine):
    assert engine.evaluate(PY, "::ffff:169.254.169.254", 443, now=0).kind == KILL


def test_allow_entry_normalizes_hosts(policy_file):
    policy = load_policy(policy_file({"version": "1", "profiles": {"a": {"allow": [{"host": " ExAmPle.COM ", "ports": [443]}]}}}))
    entry = policy.profiles["a"].allow[0]

    assert (entry.host, entry.matches("example.com", 443)) == ("example.com", True)


def test_defaults_for_engine_arguments(policy_file):
    """Threshold/window defaults are part of the deployed contract."""
    engine = EgressEngine(load_policy(policy_file(_policy())))

    assert (engine.strike_threshold, engine.strike_window_s) == (3, 60.0)


def test_profile_policy_allows_is_order_independent():
    profile = ProfilePolicy(
        name="p",
        allow=(
            AllowEntry(host="pypi.org", ports=frozenset({443})),
            AllowEntry(host=".pythonhosted.org", ports=frozenset({443})),
        ),
    )

    assert profile.allows("files.pythonhosted.org", 443)
    assert not profile.allows("files.pythonhosted.org", 80)
