"""Ed25519 v2 scoped-token minting for the sandbox-router.

Mirrors sandbox-router/authz/scopedtoken_v2.go byte-for-byte so the router's
verifier (scopedtoken.go verifyV2, and MintScopedTokenV2 in scopedtoken_v2.go)
accepts exactly what we produce.

Wire format::

    <version> . <kid> . <b64url(json_claims)> . <b64url(ed25519_signature)>

with::

    version   = "v2"
    claims    = JSON object with keys in the order {ns, name, uid, port,
                 method, path, exp}
    signing   = b"agent-sandbox/scoped-token/v2." + kid + b"." + enc_payload
    signature = Ed25519(private_seed, signing_input)

The signing context prefix, the key-ID protection (the kid is part of the
signed input), and the exact claim set are all verified router-side: any drift
here makes every request 401.
"""

from __future__ import annotations

import base64
import json
import time
from typing import Any, Dict, Optional

from .errors import ConfigError

# Consts mirrored from sandbox-router/authz/scopedtoken_v2.go:19-22.
SCOPED_TOKEN_VERSION = "v2"
SIGNING_CONTEXT = "agent-sandbox/scoped-token/v2."


def _b64url(data: bytes) -> str:
    """Unpadded base64url (RFC 4648 §5), matching Go's RawURLEncoding."""
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _upper_percent_encoding(path: str) -> str:
    """Canonicalize escapes exactly like the Go verifier (authorizer.go).

    NormalizeAuthorizationTarget keeps the path byte-verbatim: Go's
    ``url.URL{Path: PathUnescape(raw), RawPath: raw}.EscapedPath()`` returns
    RawPath unchanged (it is a valid escaping of Path by construction), so
    '/' and '%' are never introduced or re-encoded. The only transform is
    ``uppercasePercentEncoding``: hex digits inside existing %XX sequences
    are upper-cased, and a '%' without two hex digits is the same
    PathUnescape error the verifier rejects.
    """
    _HEX = "0123456789abcdefABCDEF"
    out: list[str] = []
    i = 0
    while i < len(path):
        ch = path[i]
        if ch == "%":
            hi = path[i + 1] if i + 1 < len(path) else ""
            lo = path[i + 2] if i + 2 < len(path) else ""
            if len(hi) != 1 or len(lo) != 1 or hi not in _HEX or lo not in _HEX:
                raise ConfigError(
                    f"invalid percent-encoding in scoped-token path {path!r}"
                )
            out.append("%" + hi.upper() + lo.upper())
            i += 3
        else:
            out.append(ch)
            i += 1
    return "".join(out)


def _normalize_path(path: str) -> str:
    """Match the verifier: empty -> "/", must start with "/", escape-canonicalized."""
    if not path:
        path = "/"
    if not path.startswith("/"):
        raise ConfigError(f"scoped-token path must be absolute, got {path!r}")
    return _upper_percent_encoding(path)


def build_claims(
    namespace: str,
    sandbox_name: str,
    sandbox_uid: str,
    port: int,
    method: str,
    path: str,
    ttl_seconds: int,
    now: Optional[float] = None,
) -> Dict[str, Any]:
    """Build the v2 claims dict in the verifier's exact field order.

    Port and path are validated to the same constraints NormalizeAuthorizationTarget
    enforces (port 1..65535, absolute path, method upper-cased).
    """
    if not sandbox_uid:
        raise ConfigError("scoped-token requires a sandbox UID")
    method = method.strip().upper()
    if not method or any(c not in "!#$%&'*+-.^_`|~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ" for c in method):
        raise ConfigError(f"invalid HTTP method {method!r} for scoped token")
    if not (1 <= port <= 65535):
        raise ConfigError(f"port must be between 1 and 65535, got {port}")
    exp = int(now if now is not None else time.time()) + int(ttl_seconds)
    return {
        "ns": namespace,
        "name": sandbox_name,
        "uid": sandbox_uid,
        "port": port,
        "method": method,
        "path": _normalize_path(path),
        "exp": exp,
    }


def scoped_token_v2_signing_input(key_id: str, encoded_payload: str) -> bytes:
    """Reconstruct the exact bytes the verifier signs/verifies over."""
    return (SIGNING_CONTEXT + key_id + "." + encoded_payload).encode("ascii")


def mint_scoped_token_v2(
    private_seed: bytes,
    key_id: str,
    *,
    namespace: str,
    sandbox_name: str,
    sandbox_uid: str,
    port: int,
    method: str,
    path: str,
    ttl_seconds: int,
    now: Optional[float] = None,
) -> str:
    """Mint a v2 scoped token signing over the exact router byte layout."""
    from cryptography.hazmat.primitives import serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519

    if not _valid_key_id(key_id):
        raise ConfigError(f"invalid scoped-token key id {key_id!r}")
    claims = build_claims(
        namespace, sandbox_name, sandbox_uid, port, method, path, ttl_seconds, now=now
    )
    payload = json.dumps(claims, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    enc_payload = _b64url(payload)
    signing_input = scoped_token_v2_signing_input(key_id, enc_payload)
    private_key = ed25519.Ed25519PrivateKey.from_private_bytes(private_seed)
    signature = private_key.sign(signing_input)
    return ".".join(
        [SCOPED_TOKEN_VERSION, key_id, enc_payload, _b64url(signature)]
    )


def _valid_key_id(key_id: str) -> bool:
    """Reject invalid key ids (scopedtoken_v2.go validScopedTokenKeyID)."""
    if not key_id or not key_id.isascii() or len(key_id) > 128:
        return False
    return all(c.isalnum() or c in "-_" for c in key_id)