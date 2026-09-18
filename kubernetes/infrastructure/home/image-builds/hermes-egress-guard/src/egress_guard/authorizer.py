"""Authorizer: the Envoy ext_authz decision point (plan Unit 3.2).

``POST /check`` accepts the AuthorizationRequest JSON Envoy's HTTP ext_authz
service sends::

    {"attributes": {
       "source":  {"address": {"socketAddress": {"address": "10.42.1.9"}}},
       "request": {"http": {"method": "CONNECT", "host": "pypi.org:443",
                            "path": "/",
                            "headers": {"proxy-authorization": "Basic ..."}}}}}

and a flat shape for direct callers (the e2e script, curl, tests)::

    {"method": "CONNECT", "target": "pypi.org:443",
     "proxy_authorization": "Basic ...", "source_ip": "10.42.1.9", "bytes": 4096}

Decision protocol (fixed contract): ``200`` allow, ``403`` deny (counts a
strike), ``403`` + ``x-egress-kill: 1`` quarantine now. Every decision carries
``x-egress-decision``/``x-egress-reason``/``x-egress-strikes``/
``x-egress-policy-version``, plus the verified ``x-egress-session`` /
``x-egress-profile`` when authenticated.

Every deny and kill decision is reported to the reaper as an HMAC-signed event;
the authorizer makes no other network call (no DNS, no Kubernetes API).
``GET /healthz`` exists for probes.
"""

from __future__ import annotations

import base64
import json
import logging
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from typing import Any, Callable, Mapping, Optional

from . import auth
from .config import env_float, env_int, env_required, env_str
from .http import GuardServer, HttpResult, JsonHandler, json_result
from .policy import ALLOW, DENY, KILL, Decision, EgressEngine, Identity, Policy, load_policy

LOG = logging.getLogger("egress_guard.authorizer")

DEFAULT_REAPER_URL = "http://sandbox-reaper.hermes-egress.svc.cluster.local:8080/events"


@dataclass(frozen=True)
class CheckRequest:
    method: str
    host: str
    port: int
    headers: Mapping[str, str]
    source_ip: Optional[str]
    byte_hint: int


def _lower_headers(raw: Any) -> dict[str, str]:
    headers: dict[str, str] = {}
    if not isinstance(raw, dict):
        return headers
    for key, value in raw.items():
        if isinstance(value, list):
            value = value[0] if value else ""
        headers[str(key).lower()] = str(value)
    return headers


def _socket_address(block: Any) -> Optional[str]:
    if not isinstance(block, dict):
        return None
    address = block.get("address")
    if not isinstance(address, dict):
        return None
    socket = address.get("socketAddress")
    if not isinstance(socket, dict):
        return None
    value = socket.get("address")
    return str(value) if value else None


def split_host_port(value: str, default_port: int) -> tuple[Optional[str], int]:
    """Split ``host[:port]`` / ``[v6][:port]`` without resolving anything."""
    target = (value or "").strip()
    if not target:
        return None, default_port
    if target.startswith("["):
        host, _, rest = target[1:].partition("]")
        rest = rest.lstrip(":")
        if rest and rest.isdigit():
            return host or None, int(rest)
        return host or None, default_port
    if target.count(":") == 1:
        host, _, raw_port = target.partition(":")
        if raw_port.isdigit():
            return host or None, int(raw_port)
        return host or None, default_port
    return target, default_port


def parse_check_request(payload: Any) -> CheckRequest:
    if not isinstance(payload, dict):
        raise ValueError("check payload must be a JSON object")
    attributes = payload.get("attributes")
    if isinstance(attributes, dict):
        http = (attributes.get("request") or {}).get("http") or {}
        if not isinstance(http, dict):
            raise ValueError("attributes.request.http must be an object")
        method = str(http.get("method") or "")
        headers = _lower_headers(http.get("headers"))
        target = str(http.get("host") or headers.get(":authority") or "")
        path = str(http.get("path") or "")
        source_ip = _socket_address(attributes.get("source"))
        raw_bytes: Any = headers.get("x-egress-bytes")
    else:
        method = str(payload.get("method") or "CONNECT")
        headers = _lower_headers(payload.get("headers"))
        if payload.get("proxy_authorization"):
            headers.setdefault("proxy-authorization", str(payload["proxy_authorization"]))
        target = str(payload.get("target") or payload.get("host") or "")
        path = str(payload.get("path") or "")
        source_ip = payload.get("source_ip")
        raw_bytes = payload.get("bytes")

    default_port = 443 if method.upper() == "CONNECT" else 80
    host, port = split_host_port(target, default_port)
    if host is None and path:
        # Non-CONNECT proxy requests carry an absolute-form target in :path.
        parsed = urllib.parse.urlsplit(path if "//" in path else f"//{path}")
        host, port = split_host_port(parsed.netloc, default_port)
    if not host:
        raise ValueError("check payload has no target host")
    return CheckRequest(
        method=method,
        host=host,
        port=port,
        headers=headers,
        source_ip=str(source_ip) if source_ip else None,
        byte_hint=_byte_hint(raw_bytes),
    )


def _byte_hint(raw: Any) -> int:
    """Optional advisory byte count for the per-session budget (see README:
    real byte accounting needs an Envoy-side reporter, the guard consumes the
    hint when one is supplied)."""
    if raw is None:
        return 0
    try:
        value = int(str(raw).strip())
    except (TypeError, ValueError):
        return 0
    return value if value > 0 else 0


def basic_credentials(headers: Mapping[str, str]) -> tuple[Optional[tuple[str, str]], Optional[str]]:
    """Return ((session_hash, password), None) or (None, failure_reason)."""
    raw = headers.get("proxy-authorization") or headers.get("authorization") or ""
    if not raw:
        return None, auth.TOKEN_MISSING
    scheme, _, encoded = raw.partition(" ")
    if scheme.lower() != "basic" or not encoded.strip():
        return None, auth.TOKEN_INVALID
    try:
        decoded = base64.b64decode(encoded.strip(), validate=True).decode("utf-8")
    except (ValueError, UnicodeDecodeError):
        return None, auth.TOKEN_INVALID
    session_hash, separator, password = decoded.partition(":")
    if not separator or not session_hash or not password:
        return None, auth.TOKEN_INVALID
    return (session_hash, password), None


class EventEmitter:
    """Signed event POST to the reaper. Never raises, never blocks a decision
    for longer than ``timeout_s * attempts``."""

    def __init__(
        self,
        url: str,
        secret: bytes,
        *,
        timeout_s: float = 1.0,
        attempts: int = 2,
        transport: Optional[Callable[[str, str, dict, bytes], tuple[int, bytes]]] = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.url = url
        self.secret = secret
        self.timeout_s = timeout_s
        self.attempts = max(1, attempts)
        self._sleep = sleep
        self._transport = transport or self._default_transport

    def _default_transport(self, method: str, url: str, headers: dict, body: bytes):
        request = urllib.request.Request(url, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=self.timeout_s) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as exc:
            return exc.code, exc.read()

    def emit(self, event: dict) -> bool:
        body = json.dumps(event, sort_keys=True, separators=(",", ":")).encode("utf-8")
        headers = {
            "Content-Type": "application/json",
            "x-egress-signature": auth.sign_event(self.secret, body),
        }
        for attempt in range(1, self.attempts + 1):
            try:
                status, _ = self._transport("POST", self.url, headers, body)
            except Exception as exc:  # noqa: BLE001 - transport failure must never break /check
                LOG.warning(
                    "event %s: reaper unreachable (attempt %d/%d): %s",
                    event.get("event_id"),
                    attempt,
                    self.attempts,
                    exc,
                )
                if attempt < self.attempts:
                    self._sleep(0.2)
                continue
            if 200 <= status < 300:
                return True
            if status >= 500 and attempt < self.attempts:
                LOG.warning("event %s: reaper HTTP %d, retrying", event.get("event_id"), status)
                self._sleep(0.2)
                continue
            LOG.warning("event %s: reaper rejected with HTTP %d", event.get("event_id"), status)
            return False
        return False


class Authorizer:
    def __init__(
        self,
        *,
        engine: EgressEngine,
        emitter: EventEmitter,
        secret: bytes,
        token_skew_s: float = 60.0,
        token_max_ttl_s: float = 86400.0,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.engine = engine
        self.emitter = emitter
        self.secret = secret
        self.token_skew_s = token_skew_s
        self.token_max_ttl_s = token_max_ttl_s
        self._clock = clock

    def check(self, payload: Any, now: Optional[float] = None) -> HttpResult:
        moment = self._clock() if now is None else now
        try:
            request = parse_check_request(payload)
        except ValueError as exc:
            return json_result(400, {"error": "bad-request", "detail": str(exc)})

        credentials, failure = basic_credentials(request.headers)
        if credentials is None:
            token = auth.TokenCheck(False, failure or auth.TOKEN_INVALID)
        else:
            token = auth.verify_token(
                self.secret,
                credentials[0],
                credentials[1],
                now=moment,
                skew_s=self.token_skew_s,
                max_ttl_s=self.token_max_ttl_s,
            )
        identity = (
            Identity(token.session_hash, token.profile) if token.ok else None
        )
        if identity is None:
            decision = Decision(DENY, token.reason, 0, 403)
        else:
            decision = self.engine.evaluate(
                identity,
                request.host,
                request.port,
                byte_hint=request.byte_hint,
                now=moment,
            )

        headers = {
            "x-egress-decision": decision.kind,
            "x-egress-reason": decision.reason,
            "x-egress-strikes": str(decision.strikes),
            "x-egress-policy-version": self.engine.policy.version,
        }
        if identity is not None:
            headers["x-egress-session"] = identity.session_hash
            headers["x-egress-profile"] = identity.profile
        if decision.kind == KILL:
            headers["x-egress-kill"] = "1"
        if decision.kind != ALLOW:
            self.emit_event(identity, request, decision, moment)
        body = {
            "decision": decision.kind,
            "reason": decision.reason,
            "strikes": decision.strikes,
            "target": f"{request.host}:{request.port}",
        }
        return json_result(decision.status, body, headers)

    def emit_event(
        self,
        identity: Optional[Identity],
        request: CheckRequest,
        decision: Decision,
        moment: float,
    ) -> None:
        event = {
            "event_id": uuid.uuid4().hex,
            "ts": int(moment),
            "kind": "kill" if decision.kind == KILL else "deny",
            "session_hash": identity.session_hash if identity else None,
            "profile": identity.profile if identity else None,
            "target_host": request.host,
            "target_port": request.port,
            "reason": decision.reason,
            "strikes": decision.strikes,
            "source_ip": request.source_ip,
        }
        self.emitter.emit(event)


def build_server(authorizer: Authorizer, *, port: int, bind: str = "0.0.0.0") -> GuardServer:
    policy_version = authorizer.engine.policy.version

    class Handler(JsonHandler):
        def do_POST(self) -> None:  # noqa: N802 - http.server API
            if self.path.split("?")[0] != "/check":
                self.write_result(json_result(404, {"error": "not-found"}))
                return
            try:
                payload = self.read_json()
            except ValueError as exc:
                self.write_result(json_result(400, {"error": "bad-request", "detail": str(exc)}))
                return
            self.write_result(authorizer.check(payload))

        def do_GET(self) -> None:  # noqa: N802 - http.server API
            if self.path.split("?")[0] != "/healthz":
                self.write_result(json_result(404, {"error": "not-found"}))
                return
            self.write_result(
                json_result(
                    200,
                    {
                        "status": "ok",
                        "role": "authorizer",
                        "policy_version": policy_version,
                        "profiles": sorted(authorizer.engine.policy.profiles),
                    },
                )
            )

    return GuardServer((bind, port), Handler)


def main(argv: Optional[list[str]] = None) -> int:
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )
    secret = env_required("EGRESS_HMAC_SECRET").encode("utf-8")
    policy_path = env_str("EGRESS_POLICY_FILE", "/etc/egress/policy.json") or ""
    try:
        policy: Policy = load_policy(policy_path)
    except (OSError, ValueError) as exc:
        raise SystemExit(f"cannot load egress policy from {policy_path}: {exc}") from None
    engine = EgressEngine(
        policy,
        strike_threshold=env_int("EGRESS_STRIKE_THRESHOLD", 3),
        strike_window_s=env_float("EGRESS_STRIKE_WINDOW_S", 60.0),
        max_sessions=env_int("EGRESS_MAX_SESSIONS", 4096),
    )
    emitter = EventEmitter(
        env_str("EGRESS_REAPER_URL", DEFAULT_REAPER_URL) or DEFAULT_REAPER_URL, secret
    )
    authorizer = Authorizer(
        engine=engine,
        emitter=emitter,
        secret=secret,
        token_skew_s=env_float("EGRESS_TOKEN_SKEW_S", 60.0),
        token_max_ttl_s=env_float("EGRESS_TOKEN_MAX_TTL_S", 86400.0),
    )
    port = env_int("EGRESS_LISTEN_PORT", 8080)
    server = build_server(authorizer, port=port)
    LOG.info(
        "authorizer listening on :%d (policy %s, profiles %s, reaper %s)",
        port,
        policy.version,
        sorted(policy.profiles),
        emitter.url,
    )
    server.serve_forever()
    return 0
