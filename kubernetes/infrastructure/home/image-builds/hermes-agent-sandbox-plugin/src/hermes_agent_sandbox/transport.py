"""Sandbox transport and claim lifecycle for the agent_sandbox backend.

Two layers, both deliberately thin:

* :class:`KubeClaimsClient` — in-cluster Kubernetes REST over the SDK's
  ``K8sHelper`` (itself stdlib-client + kubernetes python bindings). The
  gateway creates/deletes/list/watch SandboxClaims directly because the Go
  Router is data-plane-only (it proxies to already-adopted sandboxes and never
  creates Sandboxes/Claims). The RBAC grant in hermes/rbac.yaml is derived
  from EXACTLY the callsites in :mod:`k8s_agent_sandbox.k8s_helper` that this
  class invokes.

* :class:`SandboxTransport` — the request path to an adopted sandbox through
  the Router. Exec/file requests go over the python SDK's HTTP contract
  (``POST /execute``, ``POST /upload``, ``GET /download/...``) against the
  Router at ``AGENT_SANDBOX_ROUTER_URL`` with ``Authorization: Bearer <v2>``
  plus the ``X-Sandbox-*`` identity headers the Router's proxy/authorizer
  require. v2 tokens are minted locally (the python SDK has no mint API) and
  always bind the sandbox UID so the router can resolve the warm-pool Pod.
"""

from __future__ import annotations

import logging
import posixpath
import re
import time
import uuid
from dataclasses import dataclass
from typing import Dict, List, Optional, Tuple

from .client import mint_scoped_token_v2
from .config import AgentSandboxConfig, CLAIM_SUFFIX_BYTES, MAX_CLAIM_NAME_LEN
from .errors import (
    SandboxCreateError,
    SandboxCreateTimeoutError,
    SandboxTransportError,
)

log = logging.getLogger(__name__)

# Router/controller constants mirrored from the python SDK constants module.
_CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
_CLAIM_API_VERSION = "v1beta1"
_CLAIM_PLURAL = "sandboxclaims"
_SANDBOX_API_GROUP = "agents.x-k8s.io"
_SANDBOX_API_VERSION = "v1beta1"
_SANDBOX_PLURAL = "sandboxes"

# The Router proxies to the sandbox's Filesystem & Runtime REST API. The
# vendored v1.0.2 sandboxd's default is 8080 (sources/router sandboxd-server.go
# rest-port flag), verified live via /proc/net/tcp in a deployed sandbox.
_ROUTER_PORT = 8080

# label key we stamp on owned claims so orphan reconciliation can identify the
# owning gateway without reading arbitrary annotations.
OWNER_LABEL = "agent-sandbox.hermes/owner"


def sanitize_claim_name(task_id: str, namespace: str = "", template: str = "") -> str:
    """Build a DNS-1123-safe claim name from a Hermes task id.

    The name must be lowercase, [a-z0-9-], at most 63 chars, and unique across
    sessions of the same task. We lowercase, fold [A-Z_/.] to '-', and append a
    random hex digest suffix; when the folded task id is too long we truncate
    it to fit the suffix. The namespace/template names are not part of the
    claim name (they live in the claim's spec), so nothing user-visible leaks
    through here.
    """
    slug = re.sub(r"[^a-z0-9-]", "-", (task_id or "").lower()).strip("-")
    slug = re.sub(r"-{2,}", "-", slug)
    suffix = uuid.uuid4().hex[: CLAIM_SUFFIX_BYTES * 2]
    # Reserve room for "-" + suffix.
    budget = MAX_CLAIM_NAME_LEN - len(suffix) - 1
    if len(slug) > budget:
        slug = slug[:budget].rstrip("-")
    name = f"{slug}-{suffix}" if slug else f"hermes-{suffix}"
    return name


def _parse_iso_z(value: str) -> Optional[float]:
    """Parse ``YYYY-MM-DDTHH:MM:SSZ`` shutdownTime back to epoch seconds."""
    try:
        from datetime import datetime, timezone

        return datetime.strptime(value, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    except (ValueError, TypeError):
        return None


@dataclass
class OwnedClaim:
    """One SandboxClaim owned by this gateway."""

    name: str
    namespace: str
    shutdown_time: float


class KubeClaimsClient:
    """Thin subscriber over the python SDK's K8sHelper for claim lifecycle."""

    def __init__(self, config: AgentSandboxConfig, k8s_helper=None):
        self.config = config
        self.namespace = config.namespace
        self.template = config.template
        # Lazy SDK import: nothing in this module is imported at module load
        # time, so the plugin can surface its availability without the SDK.
        if k8s_helper is None:
            from k8s_agent_sandbox.k8s_helper import K8sHelper

            k8s_helper = K8sHelper()
        self._helper = k8s_helper

    # ---- creation (callsite: K8sHelper.create_sandbox_claim) ----
    def create_claim(self, name: str, pod_labels: Optional[Dict[str, str]] = None) -> Dict:
        """Create a SandboxClaim for this gateway against the warm pool."""
        claims = self._helper.create_sandbox_claim(
            name=name,
            warmpool=self.template,  # caller passes warmpool name==template ref
            namespace=self.namespace,
            labels={
                OWNER_LABEL: self.config.resolve_gateway_id(),
                **(pod_labels or {}),
            },
            lifecycle={
                # shutdownTime = now + max lifetime; shutdownPolicy=Delete so the
                # controller reclaims the adopted sandbox at expiry. The CRD
                # (sandbox-with-extensions.yaml spec.lifecycle) accepts both.
                "shutdownTime": self._shutdown_time_iso(),
                "shutdownPolicy": "Delete",
            },
        )
        log.info("Created SandboxClaim %s in %s (warmpool %s)", name, self.namespace, self.template)
        return claims

    def _shutdown_time_iso(self) -> str:
        from datetime import datetime, timezone, timedelta

        return (
            datetime.now(timezone.utc) + timedelta(seconds=self.config.max_lifetime_seconds)
        ).strftime("%Y-%m-%dT%H:%M:%SZ")

    # ---- readiness (callsite: K8sHelper.wait_for_claim_ready) ----
    def wait_ready(self, claim_name: str, timeout: int) -> str:
        """Return the adopted sandbox name once the claim is bound and Ready."""
        try:
            return self._helper.wait_for_claim_ready(
                claim_name, self.namespace, timeout
            )
        except TimeoutError as exc:
            raise SandboxCreateTimeoutError(str(exc)) from exc
        except Exception as exc:
            if "sandbox claim" in str(exc).lower():
                raise SandboxCreateError(str(exc)) from exc
            raise

    # ---- UID resolution (callsite: K8sHelper.get_sandbox) ----
    def get_sandbox_uid(self, sandbox_name: str) -> str:
        """Resolve the Sandbox CR UID; the router keys its cache and the
        authorization target on it.
        """
        try:
            sandbox = self._helper.get_sandbox(sandbox_name, self.namespace)
        except Exception as exc:
            raise SandboxTransportError(f"could not read sandbox {sandbox_name!r}: {exc}") from exc
        if not sandbox:
            raise SandboxTransportError(f"sandbox {sandbox_name!r} not found")
        uid = (sandbox.get("metadata") or {}).get("uid")
        if not uid:
            raise SandboxTransportError(f"sandbox {sandbox_name!r} has no metadata.uid")
        return uid

    def get_sandbox_ip(self, sandbox_name: str) -> str:
        """Resolve the Sandbox CR's pod IP for direct gRPC exec.

        The gRPC ProcessService (sandboxd v1.0.2) is reachable only at the
        pod IP on the runtime gRPC port; the CRD exposes it under
        ``status.podIPs`` (string array in the pinned CRD) or ``podIP``.
        """
        try:
            sandbox = self._helper.get_sandbox(sandbox_name, self.namespace)
        except Exception as exc:
            raise SandboxTransportError(f"could not read sandbox {sandbox_name!r}: {exc}") from exc
        if not sandbox:
            raise SandboxTransportError(f"sandbox {sandbox_name!r} not found")
        status = sandbox.get("status") or {}
        for entry in status.get("podIPs") or []:
            ip = entry.get("ip") if isinstance(entry, dict) else entry
            if ip:
                return ip
        ip = status.get("podIP")
        if ip:
            return ip
        raise SandboxTransportError(f"sandbox {sandbox_name!r} has no status.podIP")

    # ---- teardown (callsite: K8sHelper.delete_sandbox_claim) ----
    def delete_claim(self, claim_name: str) -> None:
        try:
            self._helper.delete_sandbox_claim(claim_name, self.namespace)
            log.info("Deleted SandboxClaim %s", claim_name)
        except Exception as exc:  # noqa: BLE001 - best-effort cleanup
            log.warning("delete of claim %s failed: %s", claim_name, exc)

    # ---- orphan reconcile (callsite: K8sHelper.list_sandbox_claims + get_sandbox_claim) ----
    def list_owned_claims(self, gateway_id: str) -> List[OwnedClaim]:
        """List SandboxClaims tagged as owned by *gateway_id* (label selector
        keeps the read tiny and namespaced to this gateway).
        """
        owned = []
        try:
            names = self._helper.list_sandbox_claims(
                self.namespace, label_selector=f"{OWNER_LABEL}={gateway_id}"
            )
        except Exception as exc:  # noqa: BLE001
            log.warning("could not list owned claims: %s", exc)
            return owned
        for name in names:
            claim = self._helper.get_sandbox_claim(name, self.namespace)
            if not claim:
                continue
            shutdown = (claim.get("spec") or {}).get("lifecycle", {}).get("shutdownTime")
            owned.append(
                OwnedClaim(
                    name=name,
                    namespace=self.namespace,
                    shutdown_time=_parse_iso_z(shutdown) if shutdown else 0.0,
                )
            )
        return owned

    def reconcile_orphans(self, gateway_id: str, now: Optional[float] = None) -> List[str]:
        """Delete owned claims whose shutdownTime has passed; return deleted names.

        Runs at provider/startup to clean up claims a prior gateway instance
        left behind (a gateway restart mid-task must eventually reclaim its
        sandboxes instead of letting them accumulate).
        """
        now = time.time() if now is None else now
        deleted: List[str] = []
        for claim in self.list_owned_claims(gateway_id):
            if claim.shutdown_time and claim.shutdown_time <= now:
                log.info("orphan reconcile deletes claim %s (past shutdownTime)", claim.name)
                self.delete_claim(claim.name)
                deleted.append(claim.name)
        return deleted


class SandboxTarget:
    """The signed authorization target for one sandbox request."""

    def __init__(self, namespace: str, name: str, uid: str, port: int):
        self.namespace = namespace
        self.name = name
        self.uid = uid
        self.port = port

    def headers(self, method: str, path: str) -> Dict[str, str]:
        return {
            "Authorization": "Bearer "
            + mint_scoped_token_v2(
                self._token,
                self._kid,
                namespace=self.namespace,
                sandbox_name=self.name,
                sandbox_uid=self.uid,
                port=self.port,
                method=method,
                path=path,
                ttl_seconds=self._ttl,
            ),
            "X-Sandbox-ID": self.name,
            "X-Sandbox-Namespace": self.namespace,
            "X-Sandbox-Port": str(self.port),
            "X-Sandbox-Uid": self.uid,
        }

    # Filled in by SandboxTransport.attach (kept out of __init__ so the class
    # stays a plain value object).
    _token: bytes = b""
    _kid: str = ""
    _ttl: int = 60


class SandboxTransport:
    """Executes into an adopted sandbox: gRPC ProcessService for commands,
    authenticated Router REST for file transfer (v2 scoped tokens).

    The pinned stack is behind the plugin's exec model: sandboxd v1.0.2 has
    no HTTP exec route (only gRPC on podIP:9090, REST /v1/files on 8080) and
    the Go router is an HTTP-only proxy. Commands therefore go direct gRPC to
    the adopted pod — user-approved fallback, and Cilium admits it only from
    the hermes namespace on the runtime gRPC port. Files keep the intended
    scoped-token path: GET /v1/files through the Router, per-request headers.
    """

    def __init__(self, config: AgentSandboxConfig, secret: bytes, kid: str = "hermes-1"):
        self.config = config
        self._secret = secret
        self._kid = kid
        self._target: Optional[SandboxTarget] = None
        self._connector = SdkCommandConnector(config)

    def attach(self, sandbox_name: str, sandbox_uid: str, pod_ip: str) -> None:
        target = SandboxTarget(self.config.namespace, sandbox_name, sandbox_uid, _ROUTER_PORT)
        target._token = self._secret
        target._kid = self._kid
        target._ttl = 60
        self._target = target
        if self._connector is None:
            # close() (recycle/teardown) nils the connector; re-attach after
            # an idle recycle must rebuild it or every command after the
            # first recycle crashes with AttributeError.
            self._connector = SdkCommandConnector(self.config)
        self._connector.attach(sandbox_name, pod_ip)

    # ---- exec (direct gRPC to the adopted sandbox's ProcessService) ----
    def run(self, command: str, timeout: int) -> Tuple[str, str, int]:
        """Run *command* with a per-request timeout; returns (stdout, stderr, exit_code)."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            return self._connector.run_command(command, timeout)
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"command failed at transport: {exc}") from exc

    # ---- files (authenticated Router REST, mirroring SDK Filesystem.read) ----
    def fetch_file(self, remote_path: str, timeout: int = 300) -> bytes:
        """Fetch *remote_path* bytes through the Router (v2 scoped token)."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            return self._connector.fetch_file(self._target, remote_path, timeout)
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"file fetch failed at transport: {exc}") from exc

    def close(self) -> None:
        if self._connector is not None:
            self._connector.close()
            self._connector = None
        self._target = None

    def cancel(self) -> None:
        """Best-effort cancellation: drop the connection. The remote process is
        fully killed when the claim is torn down (shutdownPolicy Delete)."""
        self.close()


class SdkCommandConnector:
    """Command execution via sandboxd's gRPC ProcessService; file transfer via
    the Router REST API with per-request v2 scoped-token headers.

    gRPC stubs are imported lazily (they need grpcio, declared as a plugin
    dependency); the plaintext channel only ever dials the adopted pod IP on
    the runtime gRPC port, never the Kubernetes API.
    """

    _GRPC_PORT = 9090

    def __init__(self, config: AgentSandboxConfig):
        self.config = config
        self._sandbox_name: Optional[str] = None
        self._pod_ip: Optional[str] = None
        self._channel = None
        self._stub = None

    def attach(self, sandbox_name: str, pod_ip: str) -> None:
        if pod_ip != self._pod_ip:
            self._drop_grpc()
        self._sandbox_name = sandbox_name
        self._pod_ip = pod_ip

    def _ensure_grpc(self):
        if self._stub is None:
            import grpc
            from k8s_agent_sandbox.commands._process_stubs import process_pb2_grpc

            if not self._pod_ip:
                raise SandboxTransportError("transport not attached to a sandbox")
            self._channel = grpc.insecure_channel(f"{self._pod_ip}:{self._GRPC_PORT}")
            self._stub = process_pb2_grpc.ProcessServiceStub(self._channel)
        return self._stub

    def run_command(self, command: str, timeout: int) -> Tuple[str, str, int]:
        from k8s_agent_sandbox.commands._process_stubs import process_pb2

        stub = self._ensure_grpc()
        response = stub.Execute(
            process_pb2.ExecuteRequest(
                config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command])
            ),
            timeout=timeout,
        )
        return (
            response.stdout.decode("utf-8", errors="replace"),
            response.stderr.decode("utf-8", errors="replace"),
            int(response.exit_code),
        )

    def fetch_file(self, target: SandboxTarget, remote_path: str, timeout: int) -> bytes:
        import urllib.error
        import urllib.parse
        import urllib.request

        # Slashes stay literal: the router's claim comparison runs against
        # the REQUEST path as Go re-derives it, and '%2F' sequences do not
        # survive that round trip (403 on every request). Only genuinely
        # unsafe characters are escaped.
        escaped = urllib.parse.quote(remote_path, safe="/")
        url = f"{self.config.router_url.rstrip('/')}/v1/files{escaped}"
        req = urllib.request.Request(
            url, headers=target.headers("GET", f"/v1/files{escaped}"), method="GET"
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.read()
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                raise SandboxTransportError(f"{remote_path!r} not found in the sandbox")
            raise SandboxTransportError(f"file fetch returned HTTP {exc.code}")
        except urllib.error.URLError as exc:
            raise SandboxTransportError(f"file fetch failed: {exc}")

    def _drop_grpc(self) -> None:
        if self._channel is not None:
            try:
                self._channel.close()
            except Exception:  # noqa: BLE001
                pass
            self._channel = None
            self._stub = None

    def close(self) -> None:
        self._drop_grpc()
        self._sandbox_name = None
        self._pod_ip = None


def _monotonic_now() -> float:
    return time.monotonic()