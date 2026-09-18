"""Minimal stdlib Kubernetes API client for the reaper.

No client library is installed, so this speaks the REST API directly and only
the verbs the reaper needs. RBAC the reaper's ServiceAccount requires (Role in
``hermes-sandbox``, bound to the ``hermes-egress`` ServiceAccount):

  * ``extensions.agents.x-k8s.io/v1beta1`` ``sandboxclaims``  get, list, delete
  * ``agents.x-k8s.io/v1beta1``            ``sandboxes``      get
  * core                                   ``configmaps``     get, create, update
    (restrict with ``resourceNames: [hermes-quarantine]``)

NOTE (actual grant, hermes-egress/rbac.yaml): only the sandboxclaims verbs and
the restricted configmap verbs are granted. ``reaper.py`` resolves a session's
claims by the ``workload.hermes.io/session-hash`` label and never calls
``get_sandbox``, so the sandboxes (and pods) verbs are deliberately NOT in the
Role — a grant with no caller is standing privilege, not headroom.

CRD group/version facts (from the vendored upstream manifests in
``kubernetes/infrastructure/home/agent-sandbox/upstream/``): SandboxClaim lives
in ``extensions.agents.x-k8s.io/v1beta1`` and Sandbox in
``agents.x-k8s.io/v1beta1``; both are namespaced.
"""

from __future__ import annotations

import json
import ssl
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable, Optional, Tuple

Transport = Callable[[str, str, dict, Optional[bytes]], Tuple[int, bytes]]


class K8sApiError(Exception):
    def __init__(self, status: int, body: str, method: str = "", path: str = "") -> None:
        super().__init__(f"{method} {path}: HTTP {status}: {body[:400]}")
        self.status = status
        self.body = body
        self.method = method
        self.path = path


def _api_base(group_version: str) -> str:
    group, _, version = group_version.partition("/")
    return f"/apis/{urllib.parse.quote(group, safe='')}/{urllib.parse.quote(version, safe='')}"


class K8sClient:
    def __init__(
        self,
        *,
        base_url: str = "https://kubernetes.default.svc",
        token_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/token",
        ca_path: str = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
        token: Optional[str] = None,
        claim_api: str = "extensions.agents.x-k8s.io/v1beta1",
        sandbox_api: str = "agents.x-k8s.io/v1beta1",
        timeout_s: float = 10.0,
        transport: Optional[Transport] = None,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token_path = token_path
        self.ca_path = ca_path
        self._token = token
        self.claim_base = _api_base(claim_api)
        self.sandbox_base = _api_base(sandbox_api)
        self.timeout_s = timeout_s
        self._transport = transport or self._default_transport

    # -- transport ---------------------------------------------------------
    def _default_transport(self, method: str, url: str, headers: dict, body: Optional[bytes]):
        context = None
        if self.ca_path:
            context = ssl.create_default_context(cafile=self.ca_path)
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout_s, context=context) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read()

    def _bearer(self) -> str:
        if self._token is not None:
            return self._token
        # Read per request: projected ServiceAccount tokens rotate.
        try:
            with open(self.token_path, "r", encoding="utf-8") as handle:
                return handle.read().strip()
        except OSError as exc:
            raise K8sApiError(0, f"cannot read service account token: {exc}") from None

    def _raw(self, method: str, path: str, body: Optional[bytes] = None) -> Tuple[int, bytes]:
        url = self.base_url + path
        headers = {"Authorization": f"Bearer {self._bearer()}", "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        return self._transport(method, url, headers, body)

    def _json(self, method: str, path: str, body: Optional[bytes] = None) -> dict:
        status, raw = self._raw(method, path, body)
        if status >= 400:
            raise K8sApiError(status, raw.decode("utf-8", "replace"), method, path)
        if not raw:
            return {}
        try:
            payload = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise K8sApiError(status, f"invalid JSON from API server: {exc}", method, path) from None
        if not isinstance(payload, dict):
            raise K8sApiError(status, "expected a JSON object from API server", method, path)
        return payload

    # -- resources ---------------------------------------------------------
    def list_claims(self, namespace: str, label_selector: str = "") -> list[dict]:
        ns = urllib.parse.quote(namespace, safe="")
        path = f"{self.claim_base}/namespaces/{ns}/sandboxclaims"
        if label_selector:
            path += "?" + urllib.parse.urlencode({"labelSelector": label_selector})
        payload = self._json("GET", path)
        items = payload.get("items") or []
        return [item for item in items if isinstance(item, dict)]

    def get_sandbox(self, namespace: str, name: str) -> dict:
        ns = urllib.parse.quote(namespace, safe="")
        sandbox = urllib.parse.quote(name, safe="")
        return self._json("GET", f"{self.sandbox_base}/namespaces/{ns}/sandboxes/{sandbox}")

    def delete_claim(self, namespace: str, name: str, uid: str) -> bool:
        """Delete one SandboxClaim, refusing if its UID changed (the UID
        precondition prevents deleting a claim that was recycled meanwhile).
        Returns False when the claim is already gone."""
        ns = urllib.parse.quote(namespace, safe="")
        claim = urllib.parse.quote(name, safe="")
        body = json.dumps(
            {"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": {"uid": uid}}
        ).encode("utf-8")
        status, raw = self._raw(
            "DELETE", f"{self.claim_base}/namespaces/{ns}/sandboxclaims/{claim}", body
        )
        if status == 404:
            return False
        if status >= 400:
            raise K8sApiError(status, raw.decode("utf-8", "replace"), "DELETE", claim)
        return True

    def get_configmap(self, namespace: str, name: str) -> Optional[dict]:
        ns = urllib.parse.quote(namespace, safe="")
        cm = urllib.parse.quote(name, safe="")
        status, raw = self._raw("GET", f"/api/v1/namespaces/{ns}/configmaps/{cm}")
        if status == 404:
            return None
        if status >= 400:
            raise K8sApiError(status, raw.decode("utf-8", "replace"), "GET", cm)
        try:
            return json.loads(raw)
        except json.JSONDecodeError as exc:
            raise K8sApiError(status, f"invalid ConfigMap JSON: {exc}", "GET", cm) from None

    def create_configmap(self, namespace: str, name: str, data: dict) -> dict:
        ns = urllib.parse.quote(namespace, safe="")
        body = json.dumps(
            {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {"name": name, "namespace": namespace},
                "data": data,
            }
        ).encode("utf-8")
        return self._json("POST", f"/api/v1/namespaces/{ns}/configmaps", body)

    def replace_configmap(self, namespace: str, name: str, data: dict, resource_version: str) -> dict:
        ns = urllib.parse.quote(namespace, safe="")
        cm = urllib.parse.quote(name, safe="")
        body = json.dumps(
            {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {
                    "name": name,
                    "namespace": namespace,
                    "resourceVersion": resource_version,
                },
                "data": data,
            }
        ).encode("utf-8")
        return self._json("PUT", f"/api/v1/namespaces/{ns}/configmaps/{cm}", body)


def client_from_env() -> K8sClient:  # pragma: no cover - exercised in the cluster
    from .config import env_str

    return K8sClient(
        base_url=env_str("EGRESS_K8S_API", "https://kubernetes.default.svc") or "",
        claim_api=env_str("EGRESS_CLAIM_API", "extensions.agents.x-k8s.io/v1beta1") or "",
        sandbox_api=env_str("EGRESS_SANDBOX_API", "agents.x-k8s.io/v1beta1") or "",
    )


_UNUSED: Any = None  # keeps `Any` imported for readers of the transport alias
