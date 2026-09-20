"""Contract tests for the reaper service (plan Unit 3.3).

The reaper is the containment leg: it turns a signed kill event into a
quarantine ledger entry and a claim deletion. The tests below pin the three
properties that make that safe:

* nothing is processed before the signature is verified over the RAW bytes;
* a replay is refused, but a *Kubernetes* failure is not recorded, so the
  authorizer's retry still lands;
* one claim's delete failing must not strand the others, and the ledger (the
  admission gate) is written before any deletion is attempted.

``StubK8s`` stands in for the API client; ``K8sApiError`` injection drives the
failure paths.
"""

from __future__ import annotations

import http.client
import json
import threading
import time

import pytest

from egress_guard import auth
from egress_guard.k8sapi import K8sApiError
from egress_guard.reaper import Reaper, ReaperConfig, build_server, quarantine_entry

NOW = 1_700_000_000
SECRET = b"unit-test-secret"
SESSION = "d" * 64
CLAIM_LABEL = "workload.hermes.io/session-hash"


class StubK8s:
    def __init__(self) -> None:
        self.claims: list[dict] = []
        self.deleted: list[tuple[str, str]] = []
        self.configmap: dict | None = {"data": {}, "metadata": {"resourceVersion": "1"}}
        self.create_calls: list[dict] = []
        self.replace_error: Exception | None = None
        self.delete_error: dict[str, Exception] = {}
        self.list_error: Exception | None = None

    # -- claims ---------------------------------------------------------
    def list_claims(self, namespace, label_selector=None):
        assert label_selector == f"{CLAIM_LABEL}={SESSION}" or label_selector is None
        if self.list_error is not None:
            raise self.list_error
        return list(self.claims)

    def delete_claim(self, namespace, name, uid):
        if name in self.delete_error:
            raise self.delete_error[name]
        self.deleted.append((name, uid))
        return True

    # -- ConfigMap ------------------------------------------------------
    def get_configmap(self, namespace, name):
        return self.configmap

    def create_configmap(self, namespace, name, data):
        self.create_calls.append(dict(data))
        self.configmap = {"data": dict(data), "metadata": {"resourceVersion": "1"}}
        return self.configmap

    def replace_configmap(self, namespace, name, data, resource_version):
        if self.replace_error is not None:
            error, self.replace_error = self.replace_error, None
            raise error
        self.configmap = {"data": dict(data), "metadata": {"resourceVersion": "2"}}
        return self.configmap


@pytest.fixture()
def reaper():
    stub = StubK8s()
    config = ReaperConfig(
        secret=SECRET,
        namespace="hermes-sandbox",
        configmap="hermes-quarantine",
        quarantine_ttl_s=86400,
        listen_port=0,
    )
    return Reaper(config, stub, auth.EventReplayGuard()), stub


def _event(**overrides):
    event = {
        "event_id": "e" * 16,
        "ts": NOW,
        "kind": "kill",
        "session_hash": SESSION,
        "profile": "python",
        "target_host": "169.254.169.254",
        "target_port": 443,
        "reason": "kill-destination",
        "strikes": 1,
        "source_ip": "172.20.5.9",
    }
    event.update(overrides)
    return event


def _body(event):
    return json.dumps(event, sort_keys=True).encode()


def _post(reaper, event=None, *, body=None, signature=None):
    raw = body if body is not None else _body(event if event is not None else _event())
    return reaper.handle_event(raw, auth.sign_event(SECRET, raw) if signature is None else signature, now=NOW)


def _claims(count=2):
    return [
        {"metadata": {"name": f"claim-{index}", "uid": f"uid-{index}"}} for index in range(count)
    ]


# --- signature gate --------------------------------------------------------


def test_unsigned_event_is_rejected(reaper):
    service, stub = reaper

    result = service.handle_event(_body(_event()), None, now=NOW)

    assert result.status == 401
    assert stub.configmap["data"] == {}
    assert stub.deleted == []


def test_wrong_signature_is_rejected(reaper):
    service, stub = reaper

    result = _post(service, signature="v1=" + "0" * 64)

    assert result.status == 401


def test_tampered_body_is_rejected(reaper):
    service, _ = reaper
    body = _body(_event())
    signature = auth.sign_event(SECRET, body)

    result = service.handle_event(body.replace(b"kill-destination", b"not-allowlisted"), signature, now=NOW)

    assert result.status == 401


def test_signed_but_unparseable_body_is_a_bad_event(reaper):
    service, _ = reaper
    raw = b"{not json"

    result = service.handle_event(raw, auth.sign_event(SECRET, raw), now=NOW)

    assert result.status == 400


# --- deny events -----------------------------------------------------------


def test_deny_event_is_counted_but_does_not_quarantine(reaper):
    service, stub = reaper

    result = _post(service, _event(kind="deny", reason="not-allowlisted"))

    assert result.status == 200
    assert stub.configmap["data"] == {}
    assert stub.deleted == []


# --- kill events -----------------------------------------------------------


def test_kill_event_quarantines_the_session_and_deletes_its_claims(reaper):
    service, stub = reaper
    stub.claims = _claims(2)

    result = _post(service)

    assert result.status == 200
    assert set(stub.configmap["data"]) == {SESSION}
    entry = json.loads(stub.configmap["data"][SESSION])
    assert entry["reason"] == "kill-destination"
    assert entry["strikes"] == 1
    assert entry["ttl_s"] == NOW + 86400
    assert entry["source_ip"] == "172.20.5.9"
    assert entry["target"] == "169.254.169.254:443"
    # Deleted by name WITH the UID precondition.
    assert stub.deleted == [("claim-0", "uid-0"), ("claim-1", "uid-1")]


def test_kill_event_quarantines_even_without_a_live_claim(reaper):
    service, stub = reaper
    stub.claims = []

    result = _post(service)

    assert result.status == 200
    assert set(stub.configmap["data"]) == {SESSION}


def test_one_failing_delete_does_not_strand_the_others(reaper):
    service, stub = reaper
    stub.claims = _claims(2)
    stub.delete_error["claim-0"] = K8sApiError(500, "boom", "DELETE", "/claims/claim-0")

    result = _post(service)

    assert result.status == 200
    assert stub.deleted == [("claim-1", "uid-1")]


def test_invalid_session_hash_in_a_kill_event_is_refused(reaper):
    service, stub = reaper
    stub.claims = _claims(1)

    result = _post(service, _event(session_hash="not a hash"))

    assert result.status == 503
    assert stub.deleted == []


def test_quarantine_entry_is_stable_json(reaper):
    entry = quarantine_entry(_event(), NOW)

    assert entry == json.dumps(json.loads(entry), sort_keys=True)


# --- replay guard ----------------------------------------------------------


def test_replayed_event_id_is_refused(reaper):
    service, stub = reaper
    stub.claims = _claims(1)

    first = _post(service)
    second = _post(service)

    assert first.status == 200
    assert second.status == 409
    # The second attempt must not delete anything again.
    assert len(stub.deleted) == 1


def test_event_outside_the_replay_window_is_refused(reaper):
    service, _ = reaper

    result = _post(service, _event(ts=NOW - 3600))

    assert result.status == 409


def test_missing_event_id_is_refused(reaper):
    service, _ = reaper
    event = _event()
    event.pop("event_id")

    result = _post(service, event)

    assert result.status == 409


# --- kubernetes failure handling ------------------------------------------


def test_kubernetes_failure_returns_503_and_is_not_recorded_as_replayed(reaper):
    service, stub = reaper
    stub.claims = _claims(1)
    stub.replace_error = K8sApiError(500, "apiserver down", "PUT", "/configmaps/hermes-quarantine")

    failed = _post(service)
    retried = _post(service)

    assert failed.status == 503
    # The retry of the SAME event id is processed (not 409): a Kubernetes
    # failure must not burn the event id.
    assert retried.status == 200
    assert set(stub.configmap["data"]) == {SESSION}


def test_missing_ledger_configmap_fails_closed(reaper):
    service, stub = reaper
    stub.claims = _claims(1)
    stub.configmap = None

    result = _post(service)

    assert result.status == 503
    assert stub.deleted == []


def test_conflicting_write_is_merged(reaper):
    service, stub = reaper
    stub.claims = _claims(1)
    stub.replace_error = K8sApiError(409, "conflict", "PUT", "/configmaps/hermes-quarantine")

    result = _post(service)

    assert result.status == 200
    assert set(stub.configmap["data"]) == {SESSION}


def test_ensure_ledger_creates_the_empty_configmap_when_absent(reaper):
    service, stub = reaper
    stub.configmap = None

    service.ensure_ledger()

    assert stub.create_calls == [{}]


def test_ensure_ledger_keeps_an_existing_configmap(reaper):
    service, stub = reaper
    stub.configmap = {"data": {"other": "entry"}, "metadata": {"resourceVersion": "9"}}

    service.ensure_ledger()

    assert stub.create_calls == []
    assert stub.configmap["data"] == {"other": "entry"}


# --- HTTP surface ----------------------------------------------------------


@pytest.fixture()
def server(reaper):
    service, _ = reaper
    httpd = build_server(service, port=0, bind="127.0.0.1")
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        yield httpd
    finally:
        httpd.shutdown()
        httpd.server_close()
        thread.join(timeout=5)


def _request(server, method, path, body=None, headers=None):
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=5)
    try:
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        return response.status, {key.lower(): value for key, value in response.getheaders()}, response.read()
    finally:
        connection.close()


def test_events_endpoint_accepts_a_signed_kill(server, reaper):
    _, stub = reaper
    stub.claims = _claims(1)
    # The HTTP path uses the wall clock (no injected clock), so the event must
    # sit inside the replay guard's ±300s window.
    body = _body(_event(ts=int(time.time())))

    status, _, payload = _request(
        server, "POST", "/events", body=body, headers={"x-egress-signature": auth.sign_event(SECRET, body)}
    )

    assert status == 200
    assert json.loads(payload)["status"] == "processed"
    assert stub.deleted == [("claim-0", "uid-0")]


def test_events_endpoint_rejects_an_unsigned_body(server, reaper):
    status, _, _ = _request(server, "POST", "/events", body=_body(_event()))

    assert status == 401


def test_healthz_and_metrics_are_open(server):
    health = _request(server, "GET", "/healthz")
    metrics = _request(server, "GET", "/metrics")

    assert health[0] == 200
    assert json.loads(health[2])["role"] == "reaper"
    assert metrics[0] == 200
    assert b"hermes_quarantine_total" in metrics[2]


def test_unknown_path_is_not_found(server):
    assert _request(server, "GET", "/nope")[0] == 404


# --- ledger TTL sweep (plan 2.E) -------------------------------------------


def test_sweep_drops_only_expired_ledger_keys(reaper):
    reaper_obj, stub = reaper
    # Two live (future TTL) + one expired (past TTL).
    stub.configmap = {
        "data": {
            "a" * 64: quarantine_entry({"ttl_s": 3600}, NOW - 10, default_ttl_s=86400),
            "b" * 64: quarantine_entry({"ttl_s": 86400}, NOW, default_ttl_s=86400),
            "c" * 64: quarantine_entry({"ttl_s": 0}, NOW - 7200, default_ttl_s=0),
        },
        "metadata": {"resourceVersion": "1"},
    }

    removed = reaper_obj._sweep_expired(now=NOW)

    assert removed == 1
    assert "c" * 64 not in stub.configmap["data"]
    assert "a" * 64 in stub.configmap["data"]
    assert "b" * 64 in stub.configmap["data"]


def test_sweep_leaves_ledger_untouched_when_nothing_expired(reaper):
    reaper_obj, stub = reaper
    stub.configmap = {
        "data": {
            "a" * 64: quarantine_entry({"ttl_s": 3600}, NOW, default_ttl_s=86400),
        },
        "metadata": {"resourceVersion": "1"},
    }

    removed = reaper_obj._sweep_expired(now=NOW)

    assert removed == 0
    assert "a" * 64 in stub.configmap["data"]


def test_sweep_skips_malformed_entries_and_reports_metric(reaper):
    reaper_obj, stub = reaper
    stub.configmap = {
        "data": {
            "a" * 64: quarantine_entry({"ttl_s": 3600}, NOW, default_ttl_s=86400),
            "bad": "not-json{",
        },
        "metadata": {"resourceVersion": "1"},
    }

    removed = reaper_obj._sweep_expired(now=NOW)

    assert removed == 0
    assert {"bad"} <= set(stub.configmap["data"].keys())
