"""HMAC identity and event signing (plan Units 3.2/3.5).

TOKEN BYTE LAYOUT — authoritative, Hermes mints it, the authorizer verifies it
-----------------------------------------------------------------------------
The gateway injects ``HTTP_PROXY``/``HTTPS_PROXY`` with the session token as
proxy userinfo::

    http://<session-hash>:<token>@hermes-egress.hermes-egress.svc.cluster.local:3128

Envoy turns that into a ``Proxy-Authorization: Basic base64(<user>:<pass>)``
header which it forwards verbatim to ext_authz, so the authorizer sees exactly:

    session_hash = the Basic username
    token        = the Basic password

and the password is::

    "v1." <expiry_unix> "." <profile> "." <mac>
    mac = b64url_nopad(HMAC_SHA256(secret,
              b"egress-token-v1|" + session_hash + b"|" + expiry_unix + b"|" + profile))

with ``expiry_unix`` written in ASCII decimal. ``session_hash`` must match
``[A-Za-z0-9._-]{1,128}`` and ``profile`` ``[a-z0-9-]{1,32}`` (the fixed
vocabulary: core, offline, python, go, node, web).

Why the profile is inside the MAC (and not an ``x-egress-profile`` header): the
sandbox controls its own HTTP headers, so anything but the verified token is
untrusted, and the authorizer has no Kubernetes API access to look the claim's
label up. Riding the MAC keeps the authorizer stateless about identity.

Accepted window: ``now - EGRESS_TOKEN_SKEW_S`` (default 60s) through
``now + EGRESS_TOKEN_MAX_TTL_S`` (default 86400s). Tokens are not single-use:
every proxied request re-presents the same proxy credentials, so replay
protection applies to *events*, not to the session token.

EVENTS — authorizer -> reaper
-----------------------------
Body (compact, ``sort_keys=True`` JSON, serialized once by the sender)::

    {"event_id": "<uuid4 hex>", "ts": <unix int>, "kind": "deny"|"kill",
     "session_hash": "<hash>"|null, "profile": ..., "target_host": ...,
     "target_port": ..., "reason": ..., "strikes": <int>, "source_ip": ...}

Header ``x-egress-signature: v1=<hex HMAC_SHA256(secret, raw body bytes)>``.
The reaper verifies over the raw bytes it received (no JSON re-serialization,
no canonicalization ambiguity) and only then parses.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import re
import time
from collections import OrderedDict
from dataclasses import dataclass
from typing import Optional

SESSION_HASH_RE = re.compile(r"^[A-Za-z0-9._-]{1,128}$")
PROFILE_RE = re.compile(r"^[a-z0-9-]{1,32}$")
EVENT_ID_RE = re.compile(r"^[A-Za-z0-9_-]{8,64}$")

TOKEN_PREFIX = "v1"
TOKEN_MAC_DOMAIN = b"egress-token-v1"
SIGNATURE_PREFIX = "v1="

#: Reasons the authorizer reports in ``x-egress-reason`` / events for
#: pre-authentication failures. None of them counts a strike.
TOKEN_MISSING = "token-missing"
TOKEN_INVALID = "token-invalid"
TOKEN_EXPIRED = "token-expired"
TOKEN_NOT_YET_VALID = "token-not-yet-valid"


@dataclass(frozen=True)
class TokenCheck:
    ok: bool
    reason: str
    session_hash: str = ""
    profile: str = ""
    expiry: int = 0


def b64url_nopad(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def b64url_decode(text: str) -> bytes:
    padding = "=" * (-len(text) % 4)
    return base64.urlsafe_b64decode(text + padding)


def token_mac_input(session_hash: str, expiry: int, profile: str) -> bytes:
    return b"|".join(
        [
            TOKEN_MAC_DOMAIN,
            session_hash.encode("utf-8"),
            str(expiry).encode("ascii"),
            profile.encode("utf-8"),
        ]
    )


def token_mac(secret: bytes, session_hash: str, expiry: int, profile: str) -> str:
    digest = hmac.new(secret, token_mac_input(session_hash, expiry, profile), hashlib.sha256)
    return b64url_nopad(digest.digest())


def mint_token(secret: bytes, session_hash: str, profile: str, expiry: int) -> str:
    """Mint a proxy password. Both the authorizer's verifier and the gateway's
    minter must agree byte-for-byte with the layout above."""
    if not SESSION_HASH_RE.match(session_hash):
        raise ValueError(f"invalid session hash: {session_hash!r}")
    if not PROFILE_RE.match(profile):
        raise ValueError(f"invalid profile: {profile!r}")
    expiry = int(expiry)
    mac = token_mac(secret, session_hash, expiry, profile)
    return f"{TOKEN_PREFIX}.{expiry}.{profile}.{mac}"


def verify_token(
    secret: bytes,
    session_hash: str,
    password: str,
    *,
    now: float,
    skew_s: float = 60.0,
    max_ttl_s: float = 86400.0,
) -> TokenCheck:
    if not SESSION_HASH_RE.match(session_hash or ""):
        return TokenCheck(False, TOKEN_INVALID)
    parts = (password or "").split(".")
    if len(parts) != 4 or parts[0] != TOKEN_PREFIX:
        return TokenCheck(False, TOKEN_INVALID)
    _, raw_expiry, profile, mac = parts
    if not raw_expiry.isdigit() or not PROFILE_RE.match(profile) or not mac:
        return TokenCheck(False, TOKEN_INVALID)
    expiry = int(raw_expiry)
    expected = token_mac(secret, session_hash, expiry, profile)
    if not hmac.compare_digest(expected, mac):
        return TokenCheck(False, TOKEN_INVALID)
    if expiry < now - skew_s:
        return TokenCheck(False, TOKEN_EXPIRED)
    if expiry > now + max_ttl_s:
        return TokenCheck(False, TOKEN_NOT_YET_VALID)
    return TokenCheck(True, "ok", session_hash=session_hash, profile=profile, expiry=expiry)


def sign_event(secret: bytes, body: bytes) -> str:
    digest = hmac.new(secret, body, hashlib.sha256).hexdigest()
    return SIGNATURE_PREFIX + digest


def verify_event(secret: bytes, body: bytes, signature: Optional[str]) -> bool:
    if not signature or not signature.startswith(SIGNATURE_PREFIX):
        return False
    expected = hmac.new(secret, body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature[len(SIGNATURE_PREFIX) :])


class EventReplayGuard:
    """Timestamp window + seen-id LRU.

    ``check`` only reports; the caller calls :meth:`record` *after* the event
    was processed successfully, so a transient Kubernetes failure does not burn
    the event id and a retry still works.
    """

    def __init__(self, *, max_events: int = 4096, skew_s: float = 300.0) -> None:
        self.max_events = max_events
        self.skew_s = skew_s
        self._seen: "OrderedDict[str, None]" = OrderedDict()

    def check(self, event_id: str, ts: object, *, now: float) -> Optional[str]:
        if not isinstance(event_id, str) or not EVENT_ID_RE.match(event_id):
            return "malformed-event-id"
        if isinstance(ts, bool) or not isinstance(ts, int):
            return "malformed-timestamp"
        if abs(now - ts) > self.skew_s:
            return "stale-timestamp"
        if event_id in self._seen:
            return "replay"
        return None

    def record(self, event_id: str) -> None:
        self._seen[event_id] = None
        self._seen.move_to_end(event_id)
        while len(self._seen) > self.max_events:
            self._seen.popitem(last=False)


def now_seconds() -> float:  # pragma: no cover - trivial
    return time.time()
