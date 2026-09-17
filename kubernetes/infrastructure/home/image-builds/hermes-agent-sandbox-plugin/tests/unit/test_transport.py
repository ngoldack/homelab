"""Claim-name sanitization, shutdown-time parsing, orphan reconciliation."""

from __future__ import annotations

import re

import pytest

from hermes_agent_sandbox.transport import (
    OWNER_LABEL,
    KubeClaimsClient,
    _parse_iso_z,
    sanitize_claim_name,
)

DNS1123 = re.compile(r"^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$")


@pytest.fixture(autouse=True)
def _clean_unreconciled():
    """Unreconciled-claim bookkeeping is process-global; keep tests isolated."""
    import hermes_agent_sandbox.transport as transport

    transport._UNRECONCILED_CLAIMS.clear()
    yield
    transport._UNRECONCILED_CLAIMS.clear()


class FakeApiError(Exception):
    """HTTP-status-carrying stand-in for kubernetes ApiException."""

    def __init__(self, message: str, status: int | None = None):
        super().__init__(message)
        self.status = status


class FakeRpcCode:
    def __init__(self, name: str):
        self.name = name

    def __str__(self) -> str:
        return f"StatusCode.{self.name}"


class FakeRpcError(Exception):
    """Minimal grpc.RpcError shape: code()/details()."""

    def __init__(self, code_name: str, details: str = "boom"):
        super().__init__(f"{code_name}: {details}")
        self._code = FakeRpcCode(code_name)
        self._details = details

    def code(self):
        return self._code

    def details(self) -> str:
        return self._details


class FakeHelper:
    """In-memory stand-in for k8s_agent_sandbox.k8s_helper.K8sHelper."""

    def __init__(self, claims):
        self._claims = {
            k: (v, "") if not isinstance(v, tuple) else v for k, v in claims.items()
        }
        self.created = []
        self.deleted = []

    def create_sandbox_claim(self, **kwargs):
        self.created.append(kwargs)
        return {}

    def wait_for_claim_ready(self, name, namespace, timeout):
        return "sbx-1"

    def get_sandbox(self, name, namespace):
        return {"metadata": {"uid": "uid-1"}}

    def list_sandbox_claims(self, namespace, label_selector=""):
        # Server-side label filtering (the real SDK does this via the API).
        selector = f"{OWNER_LABEL}="
        if label_selector.startswith(selector):
            owner = label_selector[len(selector):]
            return [n for n, (_, o) in self._claims.items() if o == owner]
        return [n for n, (c, _) in self._claims.items() if c is not None]

    def get_sandbox_claim(self, name, namespace):
        claim, _ = self._claims.get(name, (None, ""))
        return claim

    def delete_sandbox_claim(self, name, namespace):
        self.deleted.append(name)
        self._claims.pop(name, None)


def _claim(name, shutdown, owner="gw-1"):
    return (
        {
            "metadata": {"name": name},
            "spec": {"lifecycle": {"shutdownTime": shutdown}},
        },
        owner,
    )


def test_claim_name_shape_and_folding():
    name = sanitize_claim_name("My_Task/2026")
    assert DNS1123.match(name)
    assert name.startswith("my-task-2026-")


def test_claim_name_max_length():
    name = sanitize_claim_name("x" * 120)
    assert len(name) == 63
    assert DNS1123.match(name)


def test_claim_name_unique_per_call():
    assert sanitize_claim_name("task") != sanitize_claim_name("task")


def test_claim_name_empty_falls_back():
    assert sanitize_claim_name("").startswith("hermes-")
    assert sanitize_claim_name(None).startswith("hermes-")


def test_parse_iso_z():
    assert abs(_parse_iso_z("2026-01-01T00:00:00Z") - 1767225600.0) < 2
    assert _parse_iso_z("garbage") is None
    assert _parse_iso_z(None) is None


def test_create_claim_pins_warmpool_and_owner(config, token_file):
    helper = FakeHelper({})
    client = KubeClaimsClient(config, k8s_helper=helper)
    client.create_claim("c-1")
    kwargs = helper.created[0]
    assert kwargs["warmpool"] == "hermes-go"
    assert kwargs["namespace"] == "hermes-sandbox"
    assert kwargs["labels"][OWNER_LABEL] == config.resolve_gateway_id()
    assert kwargs["lifecycle"]["shutdownPolicy"] == "Delete"
    assert kwargs["lifecycle"]["shutdownTime"].endswith("Z")


def test_wait_ready_timeout_maps_to_sandbox_error(config, token_file):
    class SlowHelper(FakeHelper):
        def wait_for_claim_ready(self, name, namespace, timeout):
            raise TimeoutError("claim never became ready")

    client = KubeClaimsClient(config, k8s_helper=SlowHelper({}))
    from hermes_agent_sandbox.errors import SandboxCreateTimeoutError

    try:
        client.wait_ready("c-1", 1)
        raise AssertionError("expected SandboxCreateTimeoutError")
    except SandboxCreateTimeoutError:
        pass


def test_reconcile_deletes_only_expired(config, token_file):
    helper = FakeHelper(
        {
            "old": _claim("old", "2020-01-01T00:00:00Z"),
            "fresh": _claim("fresh", "2999-01-01T00:00:00Z"),
        }
    )
    client = KubeClaimsClient(config, k8s_helper=helper)
    deleted = client.reconcile_orphans("gw-1", now=1_700_000_000)
    assert deleted == ["old"]
    assert helper.deleted == ["old"]


def test_reconcile_missing_claim_skipped(config, token_file):
    helper = FakeHelper({"gone": None})
    client = KubeClaimsClient(config, k8s_helper=helper)
    assert client.reconcile_orphans("gw-1", now=1_700_000_000) == []


def test_reconcile_filters_other_owners(config, token_file):
    helper = FakeHelper({"m": _claim("m", "2020-01-01T00:00:00Z")})
    client = KubeClaimsClient(config, k8s_helper=helper)
    # Label filtering is delegated to the helper selector; ours simulates the
    # same by only listing this gateway's claims.
    assert client.reconcile_orphans("other-gw", now=1_700_000_000) == []


def test_wait_ready_non_timeout_error_is_named(config, token_file):
    class BoomHelper(FakeHelper):
        def wait_for_claim_ready(self, name, namespace, timeout):
            raise ValueError("apiserver returned garbage")

    from hermes_agent_sandbox.errors import SandboxCreateError

    client = KubeClaimsClient(config, k8s_helper=BoomHelper({}))
    try:
        client.wait_ready("c-1", 1)
        raise AssertionError("expected SandboxCreateError")
    except SandboxCreateError as exc:
        assert "ValueError" in str(exc)
        assert "apiserver returned garbage" in str(exc)


def test_delete_claim_retries_once_then_records_failure(
    config, token_file, monkeypatch
):
    import hermes_agent_sandbox.transport as transport

    attempts = []

    class AlwaysFailHelper(FakeHelper):
        def delete_sandbox_claim(self, name, namespace):
            attempts.append(name)
            raise RuntimeError("apiserver flaked")

    monkeypatch.setattr(transport.time, "sleep", lambda _seconds: None)
    client = KubeClaimsClient(config, k8s_helper=AlwaysFailHelper({}))
    assert client.delete_claim("c-1") is False
    assert attempts == ["c-1", "c-1"]  # exactly one retry
    assert transport._UNRECONCILED_CLAIMS == ["c-1"]
    # A second failure keeps one entry (no duplicates).
    assert client.delete_claim("c-1") is False
    assert transport._UNRECONCILED_CLAIMS == ["c-1"]


def test_delete_claim_404_is_success(config, token_file):
    import hermes_agent_sandbox.transport as transport

    class GoneHelper(FakeHelper):
        def delete_sandbox_claim(self, name, namespace):
            raise FakeApiError("sandboxclaims gone", status=404)

    client = KubeClaimsClient(config, k8s_helper=GoneHelper({}))
    assert client.delete_claim("c-1") is True
    assert transport._UNRECONCILED_CLAIMS == []


def test_reconcile_retries_unreconciled_names(config, token_file, monkeypatch):
    import hermes_agent_sandbox.transport as transport

    # Future shutdownTime: only the unreconciled list can justify deleting it.
    helper = FakeHelper({"stuck": _claim("stuck", "2999-01-01T00:00:00Z")})
    client = KubeClaimsClient(config, k8s_helper=helper)
    monkeypatch.setattr(transport.time, "sleep", lambda _seconds: None)
    transport._UNRECONCILED_CLAIMS.append("stuck")
    assert client.reconcile_orphans("gw-1", now=1_700_000_000) == ["stuck"]
    assert helper.deleted == ["stuck"]
    assert transport._UNRECONCILED_CLAIMS == []


def test_rpc_deadline_maps_to_timeout_error():
    from hermes_agent_sandbox.errors import SandboxTimeoutError
    from hermes_agent_sandbox.transport import _translate_rpc_error

    try:
        _translate_rpc_error(FakeRpcError("DEADLINE_EXCEEDED"), 7)
        raise AssertionError("expected SandboxTimeoutError")
    except SandboxTimeoutError as exc:
        assert str(exc) == "command exceeded 7s"
        assert "Traceback" not in str(exc)


def test_rpc_non_deadline_maps_to_command_error():
    from hermes_agent_sandbox.errors import SandboxCommandError, SandboxTimeoutError
    from hermes_agent_sandbox.transport import _translate_rpc_error

    try:
        _translate_rpc_error(FakeRpcError("UNAVAILABLE", details="connection refused"), 7)
        raise AssertionError("expected SandboxCommandError")
    except SandboxCommandError as exc:
        assert not isinstance(exc, SandboxTimeoutError)
        assert "UNAVAILABLE" in str(exc)
        assert "connection refused" in str(exc)


def test_transport_run_does_not_wrap_classified_command_errors(config, token_file):
    from hermes_agent_sandbox.errors import SandboxTimeoutError
    from hermes_agent_sandbox.transport import SandboxTransport

    class TimeoutConnector:
        def run_command(self, command, timeout):
            raise SandboxTimeoutError(f"command exceeded {timeout}s")

    transport = SandboxTransport(config, secret=b"x" * 32)
    transport.attach("sbx-1", "uid-1", "10.0.0.5")
    transport._connector = TimeoutConnector()
    try:
        transport.run("true", 3)
        raise AssertionError("expected SandboxTimeoutError to pass through")
    except SandboxTimeoutError:
        pass