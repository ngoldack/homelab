"""Reaper: consumes signed events, quarantines sessions, deletes claims
(plan Unit 3.3).

``POST /events`` accepts ONE event (the authorizer's schema, auth.py docstring)
with ``x-egress-signature: v1=<hex HMAC_SHA256(secret, raw body)>`` — verified
over the RAW received bytes (no re-serialization), then parsed. Replay guard:
event-id format, ±300 s window, seen-id LRU recorded only AFTER the event was
processed (auth.EventReplayGuard contract).

Per kill event: resolve the session's SandboxClaims by the
``workload.hermes.io/session-hash`` LABEL via the Kubernetes API
(k8sapi.K8sClient.list_claims), write the hermes-quarantine ConfigMap key
(namespace/key from env; the ledger is the admission gate), then delete every
resolved claim with a UID precondition (k8sapi.delete_claim). The event's
``source_ip`` is recorded in the ledger for forensics only — no pod-IP -> pod ->
claim lookup is performed (the landed authorizer carries no Kubernetes client,
and the source address is not a second identity factor). Metrics:
hermes_quarantine_total, hermes_egress_denied_total,
hermes_events_rejected_total (metrics.py).

Env (k8sapi.client_from_env + config plumbing):
  EGRESS_HMAC_SECRET (required) | EGRESS_LISTEN_PORT (8080)
  EGRESS_QUARANTINE_NAMESPACE (hermes-sandbox) | EGRESS_QUARANTINE_CONFIGMAP (hermes-quarantine)
  EGRESS_QUARANTINE_TTL_S (86400) | EGRESS_QUARANTINE_SWEEP_S (3600)
"""

from __future__ import annotations

import json
import logging
import threading
import time
from dataclasses import dataclass
from typing import Any, Optional

from . import auth, metrics as metrics_mod
from .http import GuardServer, HttpResult, JsonHandler, json_result
from .k8sapi import K8sApiError, K8sClient

LOG = logging.getLogger("egress_guard.reaper")

SESSION_HASH_LABEL = "workload.hermes.io/session-hash"

METRICS = metrics_mod.Metrics()
METRICS.declare("hermes_quarantine_total", "Sessions quarantined by the reaper.")
METRICS.declare("hermes_egress_denied_total", "Egress deny events consumed.")
METRICS.declare("hermes_events_rejected_total", "Events rejected (signature/replay/chain).")
METRICS.declare("hermes_quarantine_sweep_total", "Ledger TTL sweeps performed by the reaper.")
METRICS.declare("hermes_quarantine_swept_total", "Expired ledger entries dropped by the reaper.")


@dataclass(frozen=True)
class ReaperConfig:
    secret: bytes
    namespace: str
    configmap: str
    quarantine_ttl_s: int
    listen_port: int
    sweep_interval_s: int = 3600

def quarantine_entry(event: dict, now: int, default_ttl_s: int = 0) -> str:
    """The ConfigMap value for one quarantine: reason/strikes/TTL, one line.

    The event MAY carry its own ``ttl_s``; when it does not, the reaper's
    configured default applies, so a ledger entry always states a real expiry
    instead of an already-past one.
    """
    ttl_seconds = int(event.get("ttl_s") or default_ttl_s or 0)
    ttl = now + ttl_seconds
    return json.dumps(
        {
            "reason": event.get("reason") or "unknown",
            "strikes": int(event.get("strikes") or 0),
            "quarantined_at": now,
            "ttl_s": ttl,
            "source_ip": event.get("source_ip"),
            "target": "%s:%s" % (event.get("target_host"), event.get("target_port")),
        },
        sort_keys=True,
    )


class Reaper:
    def __init__(self, config: ReaperConfig, k8s: K8sClient, replay: auth.EventReplayGuard):
        self.config = config
        self.k8s = k8s
        self.replay = replay

    # -- events -----------------------------------------------------------
    def handle_event(self, body: bytes, signature: Optional[str], now: Optional[float] = None) -> HttpResult:
        moment = time.time() if now is None else now
        if not auth.verify_event(self.config.secret, body, signature):
            METRICS.inc("hermes_events_rejected_total")
            return json_result(401, {"error": "bad-signature"})
        try:
            event = json.loads(body)
        except json.JSONDecodeError as exc:
            METRICS.inc("hermes_events_rejected_total")
            return json_result(400, {"error": "bad-event", "detail": str(exc)})
        if not isinstance(event, dict):
            METRICS.inc("hermes_events_rejected_total")
            return json_result(400, {"error": "bad-event", "detail": "expected an object"})
        rejection = self.replay.check(str(event.get("event_id")), event.get("ts"), now=moment)
        if rejection is not None:
            METRICS.inc("hermes_events_rejected_total")
            return json_result(409, {"error": rejection})
        try:
            if event.get("kind") == "kill":
                self.quarantine(event, int(moment))
            else:
                METRICS.inc("hermes_egress_denied_total")
        except K8sApiError as exc:
            # Kubernetes failure: NOT recorded in the replay guard, so the
            # authorizer's retry still works (EventReplayGuard contract).
            LOG.error("event %s: kubernetes failure: %s", event.get("event_id"), exc)
            return json_result(503, {"error": "kubernetes-unavailable", "detail": str(exc)})
        self.replay.record(str(event.get("event_id")))
        return json_result(200, {"status": "processed", "event_id": event.get("event_id")})

    # -- quarantine -------------------------------------------------------
    def quarantine(self, event: dict, now: int) -> None:
        session_hash = str(event.get("session_hash") or "")
        if not auth.SESSION_HASH_RE.match(session_hash):
            raise K8sApiError(400, "event session_hash invalid", "POST", "/events")
        claims = self.k8s.list_claims(
            self.config.namespace, label_selector=f"{SESSION_HASH_LABEL}={session_hash}"
        )
        if not claims:
            # No live claim carries the hash: the session may have recycled
            # since the event was minted. Quarantine the hash anyway — the
            # ledger is the admission gate, the delete is the containment leg.
            LOG.warning(
                "session %s: no live claims found; quarantining the hash anyway",
                session_hash,
            )
        entry = quarantine_entry(event, now, self.config.quarantine_ttl_s)
        self._write_ledger(session_hash, entry)
        deleted = 0
        for claim in claims:
            name = str(claim.get("metadata", {}).get("name") or "")
            uid = str(claim.get("metadata", {}).get("uid") or "")
            if not name or not uid:
                continue
            try:
                if self.k8s.delete_claim(self.config.namespace, name, uid):
                    deleted += 1
            except K8sApiError as exc:
                # One claim's failure must not strand the others: the ledger
                # is already written (admission gate holds), so a later
                # event/sweep can retry the delete.
                LOG.error("claim %s delete failed: %s", name, exc)
        if deleted:
            LOG.info(
                "session %s quarantined: %d claim(s) deleted (%s)",
                session_hash, deleted, event.get("reason"),
            )
        METRICS.inc("hermes_quarantine_total")

    def _write_ledger(self, session_hash: str, entry: str) -> None:
        existing = self.k8s.get_configmap(self.config.namespace, self.config.configmap)
        if existing is None:
            raise K8sApiError(
                503, "hermes-quarantine ConfigMap missing (the reaper creates it at startup)", "GET", "/events"
            )
        data = dict(existing.get("data") or {})
        if session_hash not in data:
            data[session_hash] = entry
        resource_version = str((existing.get("metadata") or {}).get("resourceVersion") or "")
        try:
            self.k8s.replace_configmap(
                self.config.namespace, self.config.configmap, data, resource_version
            )
        except K8sApiError as exc:
            if exc.status != 409:
                raise
            # Concurrent writer: re-read once and merge (the ledger is
            # add-only per key, so a merge is always safe).
            existing = self.k8s.get_configmap(self.config.namespace, self.config.configmap)
            if existing is None:
                raise
            data = dict(existing.get("data") or {})
            if session_hash not in data:
                data[session_hash] = entry
            self.k8s.replace_configmap(
                self.config.namespace, self.config.configmap, data,
                str((existing.get("metadata") or {}).get("resourceVersion") or ""),
            )
    def _sweep_expired(self, now: Optional[int] = None) -> int:
        """Drop ledger entries whose ttl_s has passed. Returns the count removed.

        The ConfigMap key holds one session hash; the value is a JSON line
        (quarantine_entry) whose ``ttl_s`` is set at write time. A sweep
        re-reads the whole ledger and rewrites it with only live entries, so a
        concurrent writer between read and replace surfaces as a 409 and is
        merged by the same write-back logic _write_ledger uses.
        """
        moment = int(time.time()) if now is None else now
        existing = self.k8s.get_configmap(self.config.namespace, self.config.configmap)
        if existing is None:
            return 0
        data = dict(existing.get("data") or {})
        dropped: list[str] = []
        for key, value in list(data.items()):
            try:
                entry = json.loads(value)
                ttl_s = int(entry.get("ttl_s") or 0)
            except (ValueError, TypeError, AttributeError):
                # A malformed entry cannot expire by its own clock; leave it
                # alone (a corrupt ledger should not be silently deleted by
                # the sweeper).
                continue
            if ttl_s and ttl_s <= moment:
                dropped.append(key)
        if not dropped:
            return 0
        resource_version = str((existing.get("metadata") or {}).get("resourceVersion") or "")
        for key in dropped:
            data.pop(key, None)
        try:
            self.k8s.replace_configmap(
                self.config.namespace, self.config.configmap, data, resource_version
            )
        except K8sApiError as exc:
            if exc.status != 409:
                raise
            # Concurrent writer beat us: re-read and let the NEXT sweep finish
            # the job (an entry that was re-added with a fresh TTL keeps its
            # new clock, so a retry here would risk dropping a live session).
            LOG.warning("ledger sweep 409 (concurrent writer); next sweep retries")
            return 0
        METRICS.inc("hermes_quarantine_swept_total", amount=len(dropped))
        LOG.info("ledger sweep: dropped %d expired key(s): %s", len(dropped), ", ".join(sorted(dropped)))
        return len(dropped)

    def sweep_forever(self, interval_s: int) -> None:
        """Periodic TTL sweep — the daemon thread target (plan 2.E)."""
        while True:
            try:
                self._sweep_expired()
                METRICS.inc("hermes_quarantine_sweep_total")
            except K8sApiError as exc:
                # The ledger write-back can fail transiently; the next
                # interval retries. Never crash the sweep thread.
                LOG.error("ledger sweep failed: %s", exc)
            time.sleep(max(1, int(interval_s or 0)))

    # -- startup ----------------------------------------------------------
    def ensure_ledger(self) -> None:
        """The empty quarantine ConfigMap must EXIST at startup (Kyverno's
        context lookup consumes it; a missing CM is the reaper-down state)."""
        if self.k8s.get_configmap(self.config.namespace, self.config.configmap) is None:
            self.k8s.create_configmap(self.config.namespace, self.config.configmap, {})


def build_server(reaper: Reaper, *, port: int, bind: str = "0.0.0.0") -> GuardServer:
    class Handler(JsonHandler):
        def do_POST(self) -> None:  # noqa: N802 - http.server API
            if self.path.split("?")[0] != "/events":
                self.write_result(json_result(404, {"error": "not-found"}))
                return
            try:
                body = self.read_body()
            except ValueError as exc:
                self.write_result(json_result(400, {"error": "bad-request", "detail": str(exc)}))
                return
            self.write_result(reaper.handle_event(body, self.headers.get("x-egress-signature")))

        def do_GET(self) -> None:  # noqa: N802 - http.server API
            path = self.path.split("?")[0]
            if path == "/healthz":
                self.write_result(json_result(200, {"status": "ok", "role": "reaper"}))
                return
            if path == "/metrics":
                self.write_result(
                    HttpResult(200, METRICS.render().encode("utf-8"), content_type="text/plain; version=0.0.4")
                )
                return
            self.write_result(json_result(404, {"error": "not-found"}))

    return GuardServer((bind, port), Handler)


def main(argv: Optional[list[str]] = None) -> int:  # noqa: ARG001 - module argv contract
    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s"
    )
    from .config import env_int, env_required, env_str

    config = ReaperConfig(
        secret=env_required("EGRESS_HMAC_SECRET").encode("utf-8"),
        namespace=env_str("EGRESS_QUARANTINE_NAMESPACE", "hermes-sandbox") or "hermes-sandbox",
        configmap=env_str("EGRESS_QUARANTINE_CONFIGMAP", "hermes-quarantine") or "hermes-quarantine",
        quarantine_ttl_s=env_int("EGRESS_QUARANTINE_TTL_S", 86400),
        listen_port=env_int("EGRESS_LISTEN_PORT", 8080),
        sweep_interval_s=env_int("EGRESS_QUARANTINE_SWEEP_S", 3600),
    )
    reaper = Reaper(config, K8sClient(), auth.EventReplayGuard())
    try:
        reaper.ensure_ledger()
        LOG.info("hermes-quarantine ledger present (ns %s)", config.namespace)
    except K8sApiError as exc:
        # Startup proceeds: the ledger's absence surfaces on the first event
        # (503) and Kyverno's fail-closed policy reports the same condition.
        LOG.error("hermes-quarantine ledger create failed: %s", exc)
    sweep_thread = threading.Thread(
        target=reaper.sweep_forever,
        args=(config.sweep_interval_s,),
        name="ledger-ttl-sweep",
        daemon=True,
    )
    sweep_thread.start()
    LOG.info(
        "ledger TTL sweep thread started (every %ds)",
        config.sweep_interval_s,
    )
    server = build_server(reaper, port=config.listen_port)
    LOG.info("reaper listening on :%d (ledger %s/%s)", config.listen_port, config.namespace, config.configmap)
    server.serve_forever()
    return 0
