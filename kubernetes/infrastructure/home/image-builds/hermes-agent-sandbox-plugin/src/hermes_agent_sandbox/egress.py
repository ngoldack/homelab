"""Egress identity + proxy plumbing (plan Unit 3.5).

The hermes-egress guard's contracts are implemented by
``image-builds/hermes-egress-guard/src/egress_guard/`` — auth.py's token byte
layout is authoritative and must agree BYTE-FOR-BYTE with what this module
mints:

    token = "v1." <expiry_unix> "." <profile> "." <mac>
    mac   = b64url_nopad(HMAC_SHA256(secret,
                b"egress-token-v1|" + session_hash + b"|" + expiry_unix + b"|" + profile))

with expiry_unix in ASCII decimal; session_hash matches
``[A-Za-z0-9._-]{1,128}`` and profile ``[a-z0-9-]{1,32}``.

Identity is the session hash, minted ONCE per environment (one Hermes session
== one sandbox) and stamped as the ``workload.hermes.io/session-hash`` label on
the claim (which propagates to its sandbox and pods via the controller). The
proxy env is injected into the sandbox's command environment over the exec
channel: the guest's HTTP(S)_PROXY point at the guard with the token as proxy
userinfo, so the sandbox never sees the HMAC secret itself.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import secrets
import re
import time
from typing import Dict, Optional

from .config import AgentSandboxConfig

# The label key: workload.hermes.io/session-hash — the key the Kyverno
# hermes-session-quarantine policy and the reaper's RBAC-backed lookup consume
# (see hermes-egress/README.md). Sits beside the profile label key the
# capability layer already uses (config.PROFILE_LABEL).
SESSION_HASH_LABEL = "workload.hermes.io/session-hash"

# Same regex the guard's auth.py enforces; a value that fails it would be
# rejected by the authorizer as token-invalid, so it never leaves the gateway.
_SESSION_HASH_RE = re.compile(r"^[A-Za-z0-9._-]{1,128}$")
_PROFILE_RE = re.compile(r"^[a-z0-9-]{1,32}$")

TOKEN_PREFIX = "v1"
TOKEN_MAC_DOMAIN = b"egress-token-v1"

# The proxy address: hermes-egress:3128 (the fixed contract). FQDN — the
# guest resolves it with cluster DNS (its only other egress) and hands the
# CONNECT to the proxy pod IP via the Service VIP.
PROXY_HOST = "hermes-egress.hermes-egress.svc.cluster.local"
PROXY_PORT = 3128


def mint_session_hash() -> str:
    """A fresh session hash: 16 hex chars, stable for the environment's life.

    16 hex chars = 64 bits of entropy: collision risk for live sessions is
    negligible, and the value stays short enough to be a readable label and
    Basic-auth username (the guard's regex caps it at 128 chars).
    """
    return secrets.token_hex(16)


def mint_token(secret: bytes, session_hash: str, profile: str, expiry: int) -> str:
    """Mint the proxy password — byte-for-byte auth.py:mint_token.

    The guard verifies with the same layout; a drift here means every proxied
    request answers 403 token-invalid (fail-closed, loud in the metrics), so
    the two implementations are kept intentionally tiny and mirrored.
    """
    if not _SESSION_HASH_RE.match(session_hash):
        raise ValueError(f"invalid session hash: {session_hash!r}")
    if not _PROFILE_RE.match(profile):
        raise ValueError(f"invalid profile: {profile!r}")
    expiry = int(expiry)
    mac_input = b"|".join(
        [
            TOKEN_MAC_DOMAIN,
            session_hash.encode("utf-8"),
            str(expiry).encode("ascii"),
            profile.encode("utf-8"),
        ]
    )
    digest = hmac.new(secret, mac_input, hashlib.sha256)
    mac = base64.urlsafe_b64encode(digest.digest()).rstrip(b"=").decode("ascii")
    return f"{TOKEN_PREFIX}.{expiry}.{profile}.{mac}"


def proxy_env(
    config: AgentSandboxConfig,
    session_hash: str,
    profile: str,
    secret: bytes,
    *,
    now: Optional[float] = None,
) -> Dict[str, str]:
    """The sandbox env dict: proxy URL with token as proxy userinfo.

    The URL embeds <session-hash>:<token>; the guest's HTTP stack turns the
    userinfo into Proxy-Authorization on the CONNECT, which is exactly what
    the guard's authorizer verifies. The secret itself never crosses the exec
    channel — only the derived token does.
    """
    moment = time.time() if now is None else now
    expiry = int(moment) + int(config.egress_token_ttl_seconds)
    token = mint_token(secret, session_hash, profile, expiry)
    url = f"http://{session_hash}:{token}@{PROXY_HOST}:{PROXY_PORT}"
    env = {
        "http_proxy": url,
        "https_proxy": url,
        "HTTP_PROXY": url,
        "HTTPS_PROXY": url,
        # The guard's gRPC/REST transport peers (the sandbox's own Router
        # and sandboxd channels) are pod-IP direct and never route through
        # the proxy; NO_PROXY keeps them out of the tunnel. The .svc.cluster.local
        # suffix covers every in-cluster Service; the pod IP covers the direct
        # gRPC exec peer.
        "no_proxy": ".svc.cluster.local,localhost,127.0.0.1,::1",
        "NO_PROXY": ".svc.cluster.local,localhost,127.0.0.1,::1",
        # Corroboration channel: the authorizer cross-checks the Basic
        # username against the source address's claim label (identity contract
        # in hermes-egress/README.md). Advisory only — the guard never trusts
        # a sandbox-supplied identity.
        "x-egress-session": session_hash,
        "X_EGRESS_SESSION": session_hash,
    }
    return env


def quarantine_error_text(session_hash: str) -> str:
    """The permanent-quarantine error text Hermes surfaces to the user."""
    return (
        f"hermes-egress: session {session_hash} is QUARANTINED by the egress "
        f"reaper (egress violations escalated to a kill event). The sandbox "
        f"is permanently quarantined for this session; clearing requires the "
        f"documented hermes-quarantine key deletion "
        f"(kubernetes/infrastructure/home/hermes-egress/README.md, "
        f"'Clearing a quarantine'). No new sandbox can be created for this "
        f"session hash."
    )
