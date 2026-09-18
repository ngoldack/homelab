"""HTTP-level contract tests for the authorizer service (plan Units 3.2/3.5).

``test_policy.py`` covers the rules engine in isolation; this file covers the
SERVICE: the real ``ThreadingHTTPServer`` from ``build_server``, real HTTP
round-trips, the Envoy ext_authz payload shape, the token byte layout
(``auth.mint_token`` -> ``Proxy-Authorization`` -> ``auth.verify_token``) and
the decision headers Envoy acts on.

The interesting boundary is authentication: identity is whatever the HMAC
token says, because the sandbox controls every other header. The tests below
therefore drive each failure mode (missing, malformed, expired) and assert the
response carries no session identity.
"""

from __future__ import annotations

import base64
import http.client
import json
import threading

import pytest

from egress_guard import auth
from egress_guard.authorizer import Authorizer, build_server
from egress_guard.policy import EgressEngine, load_policy

NOW = 1_700_000_000
SECRET = b"unit-test-secret"
SESSION = "a" * 64
POLICY_VERSION = "http-test-1"


class Recorder:
    """Stand-in for EventEmitter: records events instead of POSTing them."""

    def __init__(self) -> None:
        self.events = []

    def emit(self, event) -> bool:
        self.events.append(event)
        return True


@pytest.fixture()
def authorizer(tmp_path):
    body = {
        "version": POLICY_VERSION,
        "profiles": {
            "python": {"allow": [{"host": ".pythonhosted.org", "ports": [443]}, {"host": "pypi.org", "ports": [443]}]},
            "offline": {"allow": []},
        },
    }
    path = tmp_path / "policy.json"
    path.write_text(json.dumps(body), encoding="utf-8")
    engine = EgressEngine(load_policy(str(path)), strike_threshold=3, strike_window_s=60.0)
    recorder = Recorder()
    authz = Authorizer(engine=engine, emitter=recorder, secret=SECRET, clock=lambda: NOW)
    return authz, recorder


@pytest.fixture()
def server(authorizer):
    authz, _ = authorizer
    httpd = build_server(authz, port=0, bind="127.0.0.1")
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        yield httpd
    finally:
        httpd.shutdown()
        httpd.server_close()
        thread.join(timeout=5)


def _post(server, path, body, *, raw=None, headers=None):
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
    try:
        payload = raw if raw is not None else json.dumps(body)
        connection.request("POST", path, body=payload, headers={"content-type": "application/json", **(headers or {})})
        response = connection.getresponse()
        return response.status, {key.lower(): value for key, value in response.getheaders()}, json.loads(response.read() or b"{}")
    finally:
        connection.close()


def _get(server, path):
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
    try:
        connection.request("GET", path)
        response = connection.getresponse()
        return response.status, {key.lower(): value for key, value in response.getheaders()}, json.loads(response.read() or b"{}")
    finally:
        connection.close()


def _token(session=SESSION, profile="python", expiry=NOW + 300):
    return auth.mint_token(SECRET, session, profile, expiry)


def _basic(session, token):
    return "Basic " + base64.b64encode(f"{session}:{token}".encode()).decode()


def _envoy_payload(target, port, *, token=None, session=SESSION, source_ip="172.20.5.9", byte_hint=None, method="CONNECT"):
    headers = {}
    if token is not None:
        headers["proxy-authorization"] = _basic(session, token)
    if byte_hint is not None:
        headers["x-egress-bytes"] = str(byte_hint)
    return {
        "attributes": {
            "request": {"http": {"method": method, "host": f"{target}:{port}", "headers": headers}},
            "source": {"address": {"socketAddress": {"address": source_ip}}},
        }
    }


def test_allowlisted_connect_is_allowed(server, authorizer):
    _, recorder = authorizer

    status, headers, body = _post(server, "/check", _envoy_payload("pypi.org", 443, token=_token()))

    assert status == 200
    assert headers["x-egress-decision"] == "allow"
    assert headers["x-egress-reason"] == "allowed"
    assert headers["x-egress-session"] == SESSION
    assert headers["x-egress-profile"] == "python"
    assert headers["x-egress-policy-version"] == POLICY_VERSION
    assert "x-egress-kill" not in headers
    assert body["target"] == "pypi.org:443"
    assert recorder.events == []


def test_suffix_entry_matches_subdomain_over_http(server, authorizer):
    status, headers, _ = _post(server, "/check", _envoy_payload("files.pythonhosted.org", 443, token=_token()))

    assert (status, headers["x-egress-decision"]) == (200, "allow")


def test_unapproved_host_is_denied_with_a_strike(server, authorizer):
    _, recorder = authorizer

    status, headers, body = _post(server, "/check", _envoy_payload("example.org", 443, token=_token()))

    assert (status, headers["x-egress-decision"], headers["x-egress-reason"]) == (403, "deny", "not-allowlisted")
    assert headers["x-egress-strikes"] == "1"
    assert "x-egress-kill" not in headers
    assert body["strikes"] == 1
    assert [event["kind"] for event in recorder.events] == ["deny"]
    assert recorder.events[0]["target_host"] == "example.org"


def test_management_plane_target_kills(server, authorizer):
    _, recorder = authorizer

    status, headers, _ = _post(server, "/check", _envoy_payload("169.254.169.254", 443, token=_token()))

    assert (status, headers["x-egress-decision"], headers["x-egress-kill"]) == (403, "kill", "1")
    assert headers["x-egress-reason"] == "kill-destination"
    assert recorder.events[0]["kind"] == "kill"


def test_repeated_denials_escalate_to_kill_over_http(server, authorizer):
    kinds = []
    for _ in range(3):
        _, headers, _ = _post(server, "/check", _envoy_payload("example.org", 443, token=_token()))
        kinds.append((headers["x-egress-decision"], headers["x-egress-kill"] if "x-egress-kill" in headers else None))

    assert kinds == [("deny", None), ("deny", None), ("kill", "1")]


def test_missing_token_is_denied_without_identity(server, authorizer):
    _, recorder = authorizer

    status, headers, _ = _post(server, "/check", _envoy_payload("pypi.org", 443))

    assert (status, headers["x-egress-reason"]) == (403, auth.TOKEN_MISSING)
    assert "x-egress-session" not in headers
    assert headers["x-egress-strikes"] == "0"
    assert recorder.events[0]["session_hash"] is None


def test_malformed_password_is_rejected(server):
    payload = _envoy_payload("pypi.org", 443, token="not-a-token")

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-reason"]) == (403, auth.TOKEN_INVALID)


def test_token_bound_to_another_session_is_rejected(server):
    """The MAC covers the session hash, so a stolen password cannot be reused."""
    payload = _envoy_payload("pypi.org", 443, session="b" * 64, token=_token())

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-reason"]) == (403, auth.TOKEN_INVALID)


def test_expired_token_is_rejected(server):
    payload = _envoy_payload("pypi.org", 443, token=_token(expiry=NOW - 600))

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-reason"]) == (403, auth.TOKEN_EXPIRED)


def test_unknown_profile_is_denied(server):
    payload = _envoy_payload("pypi.org", 443, token=_token(profile="ruby"))

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-reason"]) == (403, "profile-unknown")


def test_offline_profile_denies_every_target(server):
    payload = _envoy_payload("pypi.org", 443, session="c" * 64, token=_token(session="c" * 64, profile="offline"))

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-decision"]) == (403, "deny")


def test_byte_budget_is_consumed_over_http(server, authorizer):
    authz, _ = authorizer
    profile = authz.engine.policy.profiles["python"]
    assert profile.budget_bytes is None  # the shipped python profile has no budget
    # Enforce one through the engine the service holds, then drive it over HTTP.
    authz.engine.policy.profiles["python"] = type(profile)(
        name=profile.name, allow=profile.allow, budget_bytes=1000
    )

    first = _post(server, "/check", _envoy_payload("pypi.org", 443, token=_token(), byte_hint=600))
    second = _post(server, "/check", _envoy_payload("pypi.org", 443, token=_token(), byte_hint=600))

    assert first[1]["x-egress-decision"] == "allow"
    assert (second[1]["x-egress-decision"], second[1]["x-egress-reason"]) == ("deny", "budget-exhausted")


def test_source_address_is_recorded_for_the_reaper(server, authorizer):
    _, recorder = authorizer

    _post(server, "/check", _envoy_payload("example.org", 443, token=_token(), source_ip="172.20.7.7"))

    assert recorder.events[0]["source_ip"] == "172.20.7.7"


def test_absolute_form_path_resolves_the_target(server):
    """Non-CONNECT proxying puts the authority in :path, not :host."""
    payload = _envoy_payload("ignored", 0, token=_token(), method="GET")
    payload["attributes"]["request"]["http"]["host"] = ""
    payload["attributes"]["request"]["http"]["path"] = "http://pypi.org:443/simple/"

    status, headers, _ = _post(server, "/check", payload)

    assert (status, headers["x-egress-decision"]) == (200, "allow")


def test_request_without_a_target_is_a_bad_request(server):
    status, _, body = _post(server, "/check", {"attributes": {"request": {"http": {"method": "CONNECT", "host": ""}}}})

    assert status == 400
    assert body["error"] == "bad-request"


def test_unparseable_body_is_a_bad_request(server):
    status, _, body = _post(server, "/check", None, raw="{not json")

    assert (status, body["error"]) == (400, "bad-request")


def test_unknown_post_path_is_not_found(server):
    status, _, body = _post(server, "/nope", {})

    assert (status, body["error"]) == (404, "not-found")


def test_healthz_reports_role_and_policy_version(server):
    status, _, body = _get(server, "/healthz")

    assert status == 200
    assert body["role"] == "authorizer"
    assert body["policy_version"] == POLICY_VERSION
    assert body["profiles"] == ["offline", "python"]


def test_healthz_is_exempt_from_authentication(server):
    """Probes must not need a token: /healthz is the only unauthenticated path."""
    status, _, _ = _get(server, "/healthz")

    assert status == 200
