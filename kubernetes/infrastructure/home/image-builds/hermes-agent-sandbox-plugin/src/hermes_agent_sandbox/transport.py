"""Sandbox transport and claim lifecycle for the agent_sandbox backend.

Two layers, both deliberately thin:

* :class:`KubeClaimsClient` — in-cluster Kubernetes REST over the SDK's
  ``K8sHelper`` (itself stdlib-client + kubernetes python bindings). The
  gateway creates/deletes/list/watch SandboxClaims directly because the Go
  Router is data-plane-only (it proxies to already-adopted sandboxes and never
  creates Sandboxes/Claims). The RBAC grant in hermes/rbac.yaml is derived
  from EXACTLY the callsites in :mod:`k8s_agent_sandbox.k8s_helper` that this
  class invokes.

* :class:`SandboxTransport` — the request path to an adopted sandbox. Commands,
  stdin and background processes go over the python SDK's gRPC ProcessService
  (``Execute`` unary, ``Start`` server-streaming + ``WriteStdin`` /
  ``SendSignal``); file bytes go over the Router's authenticated REST
  FilesystemService (``GET``/``PUT``/``DELETE /v1/files``) with Ed25519 v2
  scoped tokens. Commands dial the POD IP directly (the SDK ships no HTTP exec
  route and the Go Router does not proxy gRPC); files keep the Router path so
  every transfer is scoped-token authorized.
"""

from __future__ import annotations

import logging
import posixpath
import re
import threading
import time
import uuid
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Dict, Iterator, List, NoReturn, Optional, Tuple

from .client import mint_scoped_token_v2
from .config import AgentSandboxConfig, CLAIM_SUFFIX_BYTES, MAX_CLAIM_NAME_LEN
from .errors import (
    AgentSandboxError,
    SandboxCommandError,
    SandboxCreateError,
    SandboxCreateTimeoutError,
    SandboxProcessError,
    SandboxTimeoutError,
    SandboxTransportError,
    SandboxUnsupportedError,
)

if TYPE_CHECKING:  # pragma: no cover - typing only, grpc is imported lazily
    import grpc
    from typing import Any

log = logging.getLogger(__name__)

# Router/controller constants mirrored from the python SDK constants module.
_CLAIM_API_GROUP = "extensions.agents.x-k8s.io"
_CLAIM_API_VERSION = "v1beta1"
_CLAIM_PLURAL = "sandboxclaims"
_SANDBOX_API_GROUP = "agents.x-k8s.io"
_SANDBOX_API_VERSION = "v1beta1"
_SANDBOX_PLURAL = "sandboxes"

# Authorization-target port for Router-minted scoped tokens: 8080 is the
# adopted sandbox's REST port (sandboxd's Filesystem/Runtime API), NOT the
# Router's own service port — the Router verifies each scoped token against the
# upstream sandbox's port, so "fixing" this to the Router's address would break
# every file request with 403. Verified live via /proc/net/tcp in a deployed
# sandbox (vendored v1.0.2 sandboxd default, sources/router sandboxd-server.go).
_SANDBOX_REST_PORT = 8080

# File transfers (REST) get their own default budget: a scoped-token PUT/GET of a
# multi-megabyte artifact legitimately takes longer than a command, and the
# session queue — not this timeout — is what bounds concurrency.
FILE_TRANSFER_TIMEOUT_SECONDS = 300

# stdin frames are chunked because one WriteStdin = one unary gRPC message:
# 64 KiB keeps each frame far below the 4 MiB default message ceiling while
# keeping the frame count (and therefore RPC count) sane for a 4 MiB payload.
STDIN_CHUNK_BYTES = 64 * 1024

# Ceiling for waiting on a single stream event (e.g. the InitEvent of a
# background process) before treating the stream as wedged.
STREAM_EVENT_TIMEOUT_SLACK_SECONDS = 1.0

# label key we stamp on owned claims so orphan reconciliation can identify the
# owning gateway without reading arbitrary annotations.
OWNER_LABEL = "agent-sandbox.hermes/owner"

# A failed claim delete is retried once after this delay; a name that still
# fails is recorded in _UNRECONCILED_CLAIMS for reconcile_orphans to retry.
_DELETE_RETRY_DELAY_SECONDS = 0.2

# Claims whose deletion failed in this process. The warm pool has ONE sandbox
# slot, so a claim that survives teardown starves every later session until its
# shutdownTime (~4 h) unless something removes it: reconcile_orphans retries
# these names unconditionally, independent of their lifecycle timestamps.
_UNRECONCILED_CLAIMS: List[str] = []


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


def _is_not_found(exc: BaseException) -> bool:
    """True for a 404-equivalent delete failure: the claim is already gone."""
    status = getattr(exc, "status", None)
    if status is None:
        status = getattr(exc, "status_code", None)
    if status is None:
        code = getattr(exc, "code", None)
        status = code() if callable(code) else code
    try:
        if int(status) == 404:
            return True
    except (TypeError, ValueError):
        pass
    text = str(exc).lower()
    return "404" in text or "not found" in text or "not_found" in text


def _forget_unreconciled(claim_name: str) -> None:
    try:
        _UNRECONCILED_CLAIMS.remove(claim_name)
    except ValueError:
        pass


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
            # Classified by type, not by message text: every non-timeout
            # failure to bind is still a claim-creation failure.
            raise SandboxCreateError(
                f"waiting for sandbox claim {claim_name!r} failed: "
                f"{type(exc).__name__}: {exc}"
            ) from exc

    # ---- diagnostics (callsite: K8sHelper.get_sandbox_claim) ----
    def get_claim(self, claim_name: str) -> Optional[Dict]:
        """Read one SandboxClaim (used to explain a create timeout)."""
        try:
            return self._helper.get_sandbox_claim(claim_name, self.namespace)
        except Exception as exc:
            raise SandboxTransportError(
                f"could not read claim {claim_name!r}: {exc}"
            ) from exc

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
    def delete_claim(self, claim_name: str) -> bool:
        """Delete a claim; True when it is gone (deleted or already 404).

        A delete failure is retried once and, if it still fails, recorded in
        :data:`_UNRECONCILED_CLAIMS` (and logged at ERROR) so the caller can
        keep the claim name and :meth:`reconcile_orphans` can retry it later —
        a surviving claim holds the warm pool's only sandbox slot.
        """
        for attempt in (1, 2):
            try:
                self._helper.delete_sandbox_claim(claim_name, self.namespace)
            except Exception as exc:  # noqa: BLE001 - cleanup must not raise
                if _is_not_found(exc):
                    _forget_unreconciled(claim_name)
                    log.info("SandboxClaim %s already gone", claim_name)
                    return True
                if attempt == 1:
                    log.warning(
                        "delete of claim %s failed (%s); retrying", claim_name, exc
                    )
                    time.sleep(_DELETE_RETRY_DELAY_SECONDS)
                    continue
                log.error(
                    "delete of claim %s failed after retry: %s — recording it for "
                    "orphan reconcile",
                    claim_name,
                    exc,
                )
                if claim_name not in _UNRECONCILED_CLAIMS:
                    _UNRECONCILED_CLAIMS.append(claim_name)
                return False
            _forget_unreconciled(claim_name)
            log.info("Deleted SandboxClaim %s", claim_name)
            return True
        return False

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
        sandboxes instead of letting them accumulate). Names whose deletion
        already failed in this process are retried unconditionally — their
        shutdownTime is not authoritative because the claim may be stuck.
        """
        now = time.time() if now is None else now
        deleted: List[str] = []
        for claim in self.list_owned_claims(gateway_id):
            if claim.shutdown_time and claim.shutdown_time <= now:
                log.info("orphan reconcile deletes claim %s (past shutdownTime)", claim.name)
                if self.delete_claim(claim.name):
                    deleted.append(claim.name)
        for name in list(_UNRECONCILED_CLAIMS):
            log.info("orphan reconcile retries unreconciled claim %s", name)
            if self.delete_claim(name):
                deleted.append(name)
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
        target = SandboxTarget(self.config.namespace, sandbox_name, sandbox_uid, _SANDBOX_REST_PORT)
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
        except SandboxCommandError:
            # Already classified (including SandboxTimeoutError): re-raise so
            # the environment can tell a real remote deadline from any other
            # transport failure instead of seeing one opaque wrapper.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"command failed at transport: {exc}") from exc

    # ---- files (authenticated Router REST, mirroring SDK Filesystem.read) ----
    def fetch_file(self, remote_path: str, timeout: int = 300) -> bytes:
        """Fetch *remote_path* bytes through the Router (v2 scoped token)."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            return self._connector.fetch_file(self._target, remote_path, timeout)
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"file fetch failed at transport: {exc}") from exc

    def put_file(
        self, remote_path: str, data: bytes, timeout: Optional[int] = None
    ) -> None:
        """Write *data* to *remote_path* through the Router (PUT, v2 scoped token).

        The scoped token binds method+path, so the PUT must be signed for the
        exact path the Router will re-derive (no query string: the Router's
        authorization target is the upstream path, query excluded).
        """
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            self._connector.put_file(
                self._target, remote_path, data, self._file_timeout(timeout)
            )
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"file write failed at transport: {exc}") from exc

    def delete_file(
        self, remote_path: str, recursive: bool = False, timeout: Optional[int] = None
    ) -> None:
        """Delete *remote_path* through the Router (DELETE, v2 scoped token)."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            self._connector.delete_file(
                self._target, remote_path, recursive, self._file_timeout(timeout)
            )
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"file delete failed at transport: {exc}") from exc

    def _file_timeout(self, timeout: Optional[int]) -> int:
        """File transfers get their own generous budget (large PUT/GET bodies)."""
        return int(timeout) if timeout else FILE_TRANSFER_TIMEOUT_SECONDS

    # ---- process service: stdin + streaming (capability layer) ----
    def run_with_stdin(
        self, command: str, payload: bytes, timeout: int
    ) -> Tuple[str, str, int]:
        """Run *command* feeding *payload* to its stdin; returns (out, err, code)."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            return self._connector.run_with_stdin(command, payload, timeout)
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"stdin command failed at transport: {exc}") from exc

    def start_process(self, command: str, start_timeout: int) -> "StartedProcess":
        """Open a long-lived ``Start`` stream; returns the live process handle."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            return self._connector.start_process(command, start_timeout)
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"process start failed at transport: {exc}") from exc

    def signal_process(self, process_id: int, signal_name: str, timeout: int = 30) -> None:
        """Deliver INT/TERM/KILL to the remote process group."""
        if self._target is None or self._connector is None:
            raise SandboxTransportError("transport not attached to a sandbox")
        try:
            self._connector.signal_process(process_id, signal_name, timeout)
        except AgentSandboxError:
            # Already classified (path confinement, unsupported RPC, process
            # lifecycle, command taxonomy): never re-wrap as a transport error.
            raise
        except Exception as exc:  # noqa: BLE001
            raise SandboxTransportError(f"process signal failed at transport: {exc}") from exc

    def close(self) -> None:
        if self._connector is not None:
            self._connector.close()
            self._connector = None
        self._target = None

    def cancel(self) -> None:
        """Best-effort cancellation: drop the connection. The remote process is
        fully killed when the claim is torn down (shutdownPolicy Delete)."""
        self.close()


def _translate_rpc_error(exc: "grpc.RpcError", timeout: int) -> NoReturn:
    """Map a gRPC RpcError onto the command-error taxonomy.

    DEADLINE_EXCEEDED is the only status that proves the command outran its
    deadline; every other status (UNAVAILABLE, INTERNAL, ...) is reported as a
    command failure carrying the status and details instead of a raw traceback.
    UNIMPLEMENTED is separated out because it means the pinned sandboxd build
    lacks the RPC entirely (not that the request failed), which is the only
    condition the stdin auto-fallback is allowed to react to.
    """
    code = exc.code()
    name = getattr(code, "name", "")
    if name == "DEADLINE_EXCEEDED":
        raise SandboxTimeoutError(f"command exceeded {timeout}s") from exc
    if name == "UNIMPLEMENTED":
        raise SandboxUnsupportedError(
            f"sandboxd does not implement this RPC ({code}: {exc.details()})"
        ) from exc
    raise SandboxCommandError(f"command failed: {code} ({exc.details()})") from exc


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
        import grpc
        from k8s_agent_sandbox.commands._process_stubs import process_pb2

        stub = self._ensure_grpc()
        try:
            response = stub.Execute(
                process_pb2.ExecuteRequest(
                    config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command])
                ),
                timeout=timeout,
            )
        except grpc.RpcError as exc:
            _translate_rpc_error(exc, timeout)
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
            if exc.code == 403:
                # sandboxd's pathutil refused the resolved target (path escapes
                # /workspace). Typed so callers can distinguish confinement from
                # an infrastructure failure; see SandboxPathError.
                from .errors import SandboxPathError

                raise SandboxPathError(
                    f"{remote_path!r} was refused by the sandbox filesystem (HTTP 403: "
                    "resolves outside the sandbox root)"
                ) from exc
            raise SandboxTransportError(f"file fetch returned HTTP {exc.code}")
        except urllib.error.URLError as exc:
            raise SandboxTransportError(f"file fetch failed: {exc}")

    def put_file(
        self, target: SandboxTarget, remote_path: str, data: bytes, timeout: int
    ) -> None:
        """PUT raw bytes to sandboxd's FilesystemService through the Router.

        Content-Type is pinned to application/octet-stream so sandboxd's
        `requestFileBody` takes the raw-body branch: it switches to multipart
        parsing when the header says multipart/form-data, and urllib's default
        (application/x-www-form-urlencoded) would otherwise be sent for a bytes
        body. 204 is the only success status sandboxd emits for PUT.
        """
        import urllib.error
        import urllib.parse
        import urllib.request

        escaped = urllib.parse.quote(remote_path, safe="/")
        path = f"/v1/files{escaped}"
        headers = dict(target.headers("PUT", path))
        headers["Content-Type"] = "application/octet-stream"
        req = urllib.request.Request(
            f"{self.config.router_url.rstrip('/')}{path}",
            data=data,
            headers=headers,
            method="PUT",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                if resp.status not in (200, 204):
                    raise SandboxTransportError(
                        f"file write to {remote_path!r} returned HTTP {resp.status}"
                    )
        except urllib.error.HTTPError as exc:
            if exc.code == 403:
                # sandboxd's pathutil refused the resolved target: the path
                # escapes /workspace (symlink-aware). Map it to the plugin's own
                # path error so callers see a typed confinement failure.
                from .errors import SandboxPathError

                raise SandboxPathError(
                    f"{remote_path!r} was refused by the sandbox filesystem (HTTP 403: "
                    "resolves outside the sandbox root)"
                ) from exc
            raise SandboxTransportError(f"file write returned HTTP {exc.code}")
        except urllib.error.URLError as exc:
            raise SandboxTransportError(f"file write failed: {exc}")

    def delete_file(
        self,
        target: SandboxTarget,
        remote_path: str,
        recursive: bool,
        timeout: int,
    ) -> None:
        """DELETE a file (or directory) from sandboxd through the Router."""
        import urllib.error
        import urllib.parse
        import urllib.request

        escaped = urllib.parse.quote(remote_path, safe="/")
        path = f"/v1/files{escaped}" + ("?recursive=true" if recursive else "")
        # The scoped token binds the PATH, not the query (router README:
        # "Query parameters and request bodies are not signed"), so the token is
        # minted for the bare path while the URL carries the recursive flag.
        signed_path = f"/v1/files{escaped}"
        req = urllib.request.Request(
            f"{self.config.router_url.rstrip('/')}{path}",
            headers=target.headers("DELETE", signed_path),
            method="DELETE",
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                if resp.status not in (200, 204):
                    raise SandboxTransportError(
                        f"delete of {remote_path!r} returned HTTP {resp.status}"
                    )
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                raise SandboxTransportError(f"{remote_path!r} not found in the sandbox")
            raise SandboxTransportError(f"file delete returned HTTP {exc.code}")
        except urllib.error.URLError as exc:
            raise SandboxTransportError(f"file delete failed: {exc}")

    def run_with_stdin(
        self, command: str, payload: bytes, timeout: int
    ) -> Tuple[str, str, int]:
        """Execute *command* with *payload* on stdin via Start + WriteStdin.

        Why the streaming RPC instead of the unary Execute: sandboxd's Execute
        has no stdin field at all (process.proto ExecuteRequest = {config}), and
        WriteStdin addresses a process_id that only ``Start`` creates
        (packages/sandboxd/pkg/server/process.go: Execute runs the command and
        buffers output, Start registers the process and creates the stdin pipe).
        """
        import grpc
        from google.protobuf import empty_pb2
        from k8s_agent_sandbox.commands._process_stubs import process_pb2

        stub = self._ensure_grpc()
        try:
            return drive_start_with_stdin(
                stub, process_pb2, empty_pb2, command, payload, timeout
            )
        except grpc.RpcError as exc:
            _translate_rpc_error(exc, timeout)

    def start_process(self, command: str, start_timeout: int) -> "StartedProcess":
        """Open a background ``Start`` stream and wait for its InitEvent."""
        import grpc
        from k8s_agent_sandbox.commands._process_stubs import process_pb2

        stub = self._ensure_grpc()
        request = process_pb2.StartRequest(
            config=process_pb2.ProcessConfig(command=["/bin/sh", "-c", command])
        )
        try:
            # No deadline: a background process is long-lived by definition. The
            # InitEvent wait below is what bounds a wedged start.
            stream = stub.Start(request)
        except grpc.RpcError as exc:
            _translate_rpc_error(exc, start_timeout)
        events = iter_start_events(stream)
        try:
            kind, value = next_stream_event(events, start_timeout)
        except SandboxTimeoutError as exc:
            _cancel_stream(stream)
            raise SandboxProcessError(
                f"background process did not start within {start_timeout}s: {exc}"
            ) from exc
        except grpc.RpcError as exc:
            _translate_rpc_error(exc, start_timeout)
        if kind != START_EVENT_INIT:
            _cancel_stream(stream)
            raise SandboxProcessError(
                f"background process stream started with a {kind!r} event, "
                "expected an InitEvent"
            )
        return StartedProcess(int(value), events, stream)

    def signal_process(self, process_id: int, signal_name: str, timeout: int) -> None:
        """Deliver a signal to the remote process group (INT/TERM/KILL)."""
        import grpc
        from k8s_agent_sandbox.commands._process_stubs import process_pb2

        key = (signal_name or "").strip().upper()
        if key.startswith("SIG"):
            key = key[3:]
        constant = _SIGNAL_CONSTANTS.get(key)
        if constant is None:
            raise SandboxCommandError(
                f"unsupported signal {signal_name!r}; use INT, TERM or KILL"
            )
        stub = self._ensure_grpc()
        try:
            stub.SendSignal(
                process_pb2.SendSignalRequest(
                    process_id=process_id, signal=getattr(process_pb2, constant)
                ),
                timeout=timeout,
            )
        except grpc.RpcError as exc:
            code = getattr(exc.code(), "name", "")
            if code == "NOT_FOUND":
                # The process exited between the registry read and the signal:
                # a normal race for short-lived background commands.
                raise SandboxProcessError(
                    f"process {process_id} is no longer running"
                ) from exc
            _translate_rpc_error(exc, timeout)

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


# ---------------- ProcessService.Start stream helpers ----------------
# Event kind tags yielded by :func:`iter_start_events`. They mirror the
# `StartResponse` oneof field names in sandboxd's process.proto (`init`,
# `stdout`, `stderr`, `exit`), verified against the generated stubs shipped by
# k8s-agent-sandbox 1.0.2 (process_pb2.StartResponse.oneofs_by_name == ["event"]),
# so a wire change shows up here as an explicit mismatch instead of a silent drop.
START_EVENT_INIT = "init"
START_EVENT_STDOUT = "stdout"
START_EVENT_STDERR = "stderr"
START_EVENT_EXIT = "exit"

# Signal name -> module-level protobuf constant (process_pb2.SIGNAL_SIGTERM, ...).
_SIGNAL_CONSTANTS = {
    "INT": "SIGNAL_SIGINT",
    "TERM": "SIGNAL_SIGTERM",
    "KILL": "SIGNAL_SIGKILL",
}


def iter_start_events(stream: Any) -> Iterator[Tuple[str, Any]]:
    """Decode a ``ProcessService.Start`` stream into ``(kind, value)`` tuples.

    Yields ``(init, process_id)``, ``(stdout, bytes)``, ``(stderr, bytes)`` and
    finally ``(exit, exit_code)``, then stops. Iteration raises the underlying
    ``grpc.RpcError`` if the stream fails (including DEADLINE_EXCEEDED).
    """
    for event in stream:
        kind = event.WhichOneof("event")
        if kind == START_EVENT_INIT:
            yield START_EVENT_INIT, int(event.init.process_id)
        elif kind == START_EVENT_STDOUT:
            yield START_EVENT_STDOUT, bytes(event.stdout)
        elif kind == START_EVENT_STDERR:
            yield START_EVENT_STDERR, bytes(event.stderr)
        elif kind == START_EVENT_EXIT:
            yield START_EVENT_EXIT, int(event.exit.exit_code)
            return
        else:
            log.debug("ignoring unknown StartResponse event %r", kind)


def next_stream_event(
    events: Iterator[Tuple[str, Any]], timeout: float
) -> Tuple[str, Any]:
    """Read one stream event with a deadline.

    gRPC python has no per-``next()`` timeout, so the read runs on a throwaway
    daemon thread and the caller stops waiting after *timeout*. On timeout that
    thread is still blocked in ``next()``; the caller cancels the stream, which
    unblocks it with an RpcError the worker discards — so nothing leaks past the
    cancel.
    """
    box: Dict[str, Any] = {}

    def worker() -> None:
        try:
            box["event"] = next(events)
        except StopIteration:
            box["empty"] = True
        except BaseException as exc:  # noqa: BLE001 - handed back to the caller
            box["error"] = exc

    thread = threading.Thread(target=worker, name="agent-sandbox-stream", daemon=True)
    thread.start()
    if not thread.join(timeout + STREAM_EVENT_TIMEOUT_SLACK_SECONDS):
        raise SandboxTimeoutError(f"no stream event within {timeout}s")
    if "error" in box:
        raise box["error"]
    if box.get("empty"):
        raise SandboxProcessError("the process stream ended before any event")
    return box["event"]


def _cancel_stream(stream: Any) -> None:
    """Best-effort stream cancel; never masks the original failure."""
    try:
        stream.cancel()
    except Exception as exc:  # noqa: BLE001 - cancel is advisory
        log.debug("stream cancel failed: %s", exc)


class StartedProcess:
    """A live sandboxd process started with the streaming ``Start`` RPC.

    ``events()`` returns the still-open event iterator with the InitEvent
    already consumed by the transport; a consumer draining it sees stdout/stderr
    chunks and finally the exit event. Draining is mandatory: sandboxd sends
    stream events synchronously, so a client that stops reading eventually
    stalls the remote process on a full pipe.
    """

    def __init__(self, process_id: int, events: Iterator[Tuple[str, Any]], stream: Any):
        self.process_id = process_id
        self._events = events
        self._stream = stream

    def events(self) -> Iterator[Tuple[str, Any]]:
        return self._events

    def cancel(self) -> None:
        """Cancel the RPC; sandboxd kills the child when the stream context ends."""
        _cancel_stream(self._stream)


def drive_start_with_stdin(
    stub: Any,
    pb2: Any,
    empty: Any,
    command: str,
    payload: bytes,
    timeout: int,
    *,
    chunk_bytes: int = STDIN_CHUNK_BYTES,
    clock: Any = time.monotonic,
) -> Tuple[str, str, int]:
    """Run *command* with *payload* on stdin; return (stdout, stderr, exit_code).

    stdin is written from a helper thread: the server only drains the child's
    stdin pipe while it streams stdout/stderr, so a synchronous writer would
    deadlock against a process that writes output before consuming all input
    (``cat`` with a large payload is exactly that shape).

    A stdin write failure is NOT fatal on its own — the process may have exited
    before reading (``true``, a command that ignores stdin) and the command's
    observable result is still the exit event. Write errors are recorded and
    surfaced only when the stream never produced an exit code.
    """
    request = pb2.StartRequest(
        config=pb2.ProcessConfig(command=["/bin/sh", "-c", command])
    )
    stream = stub.Start(request, timeout=timeout)
    deadline = clock() + timeout
    stdout = bytearray()
    stderr = bytearray()
    exit_code: Optional[int] = None
    write_errors: List[BaseException] = []
    writer: Optional[threading.Thread] = None
    try:
        for event in stream:
            kind = event.WhichOneof("event")
            if kind == START_EVENT_INIT:
                writer = threading.Thread(
                    target=_write_stdin_payload,
                    args=(
                        stub,
                        pb2,
                        empty,
                        int(event.init.process_id),
                        payload,
                        deadline,
                        chunk_bytes,
                        write_errors,
                        clock,
                    ),
                    name="agent-sandbox-stdin",
                    daemon=True,
                )
                writer.start()
            elif kind == START_EVENT_STDOUT:
                stdout += event.stdout
            elif kind == START_EVENT_STDERR:
                stderr += event.stderr
            elif kind == START_EVENT_EXIT:
                exit_code = int(event.exit.exit_code)
                break
    finally:
        if writer is not None:
            # Bounded join: a writer still blocked on a full pipe for a process
            # that already exited must not delay the result.
            writer.join(timeout=STREAM_EVENT_TIMEOUT_SLACK_SECONDS)
    if exit_code is None:
        detail = f"; first stdin write error: {write_errors[0]}" if write_errors else ""
        raise SandboxCommandError(
            f"stdin command stream ended without an exit event{detail}"
        )
    if write_errors:
        log.debug("stdin write errors (process likely exited early): %s", write_errors[0])
    return (
        stdout.decode("utf-8", errors="replace"),
        stderr.decode("utf-8", errors="replace"),
        exit_code,
    )


def _write_stdin_payload(
    stub: Any,
    pb2: Any,
    empty: Any,
    process_id: int,
    payload: bytes,
    deadline: float,
    chunk_bytes: int,
    errors: List[BaseException],
    clock: Any,
) -> None:
    """Write *payload* in chunked WriteStdin frames, then one EOF frame."""
    try:
        for offset in range(0, len(payload), chunk_bytes):
            _write_stdin_frame(
                stub,
                pb2,
                empty,
                process_id,
                payload[offset : offset + chunk_bytes],
                deadline,
                clock,
            )
        # EOF closes the pipe: without it a `cat`-style reader blocks until the
        # command deadline instead of returning its output.
        _write_stdin_frame(stub, pb2, empty, process_id, None, deadline, clock)
    except BaseException as exc:  # noqa: BLE001 - reported back through `errors`
        errors.append(exc)


def _write_stdin_frame(
    stub: Any,
    pb2: Any,
    empty: Any,
    process_id: int,
    chunk: Optional[bytes],
    deadline: float,
    clock: Any,
) -> None:
    """Send one stdin frame (or the EOF marker) with the remaining budget."""
    remaining = max(1.0, deadline - clock())
    if chunk is None:
        request = pb2.WriteStdinRequest(process_id=process_id, eof=empty.Empty())
    else:
        request = pb2.WriteStdinRequest(process_id=process_id, input=chunk)
    stub.WriteStdin(request, timeout=remaining)