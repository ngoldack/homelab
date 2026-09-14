"""Scoped-token v2 minting: wire format, byte-exact signing input, claims."""

from __future__ import annotations

import base64
import json
import time

import pytest
from cryptography.hazmat.primitives.asymmetric import ed25519

from hermes_agent_sandbox.client import (
    SIGNING_CONTEXT,
    SCOPED_TOKEN_VERSION,
    _upper_percent_encoding,
    mint_scoped_token_v2,
    scoped_token_v2_signing_input,
)
from hermes_agent_sandbox.errors import ConfigError

SEED = bytes(range(32))


def test_wire_format_and_signature():
    tok = mint_scoped_token_v2(
        SEED,
        "hermes-1",
        namespace="hermes-sandbox",
        sandbox_name="sbx-abc",
        sandbox_uid="uid-9",
        port=8888,
        method="post",
        path="/execute",
        ttl_seconds=60,
        now=1_700_000_000,
    )
    version, kid, enc_payload, enc_sig = tok.split(".")
    assert version == SCOPED_TOKEN_VERSION
    assert kid == "hermes-1"
    payload = json.loads(base64.urlsafe_b64decode(enc_payload + "=="))
    assert payload == {
        "ns": "hermes-sandbox",
        "name": "sbx-abc",
        "uid": "uid-9",
        "port": 8888,
        "method": "POST",  # upper-cased
        "path": "/execute",  # verbatim; only existing %XX hex is upper-cased
        "exp": 1_700_000_060,
    }
    # Byte-exact signing input must match the verifier layout.
    signing_input = scoped_token_v2_signing_input(kid, enc_payload)
    assert signing_input == (
        f"agent-sandbox/scoped-token/v2.{kid}.{enc_payload}".encode("ascii")
    )
    assert SIGNING_CONTEXT == "agent-sandbox/scoped-token/v2."
    priv = ed25519.Ed25519PrivateKey.from_private_bytes(SEED)
    pub = priv.public_key()
    pub.verify(base64.urlsafe_b64decode(enc_sig + "=="), signing_input)  # no raise


def test_path_normalization_uppercases_escapes():
    # Verifier parity: bytes verbatim, hex inside existing %XX upper-cased.
    assert _upper_percent_encoding("/a%2fb") == "/a%2Fb"
    assert _upper_percent_encoding("/execute") == "/execute"
    assert _upper_percent_encoding("/x%7eY") == "/x%7EY"
    assert _upper_percent_encoding("/fü") == "/fü"  # non-ASCII kept verbatim


def test_empty_path_becomes_root():
    tok = mint_scoped_token_v2(
        SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
        port=1, method="GET", path="", ttl_seconds=1,
    )
    payload = json.loads(
        base64.urlsafe_b64decode(tok.split(".")[2] + "==")
    )
    assert payload["path"] == "/"  # empty -> "/"


def test_relative_path_rejected():
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
            port=1, method="GET", path="nope", ttl_seconds=1,
        )


@pytest.mark.parametrize("bad_path", ["/a%zz", "/trailing%", "/%", "/x%2"])
def test_invalid_escapes_rejected(bad_path):
    # Go url.PathUnescape error branch: '%' without two hex digits.
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
            port=1, method="GET", path=bad_path, ttl_seconds=1,
        )


@pytest.mark.parametrize(
    "bad",
    ["", "G ET", "GET/", "bad:method", "GÉT"],
)
def test_invalid_method_rejected(bad):
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
            port=1, method=bad, path="/", ttl_seconds=1,
        )


def test_port_bounds():
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
            port=0, method="GET", path="/", ttl_seconds=1,
        )
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
            port=65536, method="GET", path="/", ttl_seconds=1,
        )


def test_empty_uid_rejected():
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="",
            port=1, method="GET", path="/", ttl_seconds=1,
        )


@pytest.mark.parametrize("kid", ["", "../evil", "a" * 129, "with space", "über"])
def test_invalid_key_ids_rejected(kid):
    with pytest.raises(ConfigError):
        mint_scoped_token_v2(
            SEED, kid, namespace="n", sandbox_name="s", sandbox_uid="u",
            port=1, method="GET", path="/", ttl_seconds=1,
        )


def test_exp_uses_now_when_given():
    tok = mint_scoped_token_v2(
        SEED, "k", namespace="n", sandbox_name="s", sandbox_uid="u",
        port=1, method="GET", path="/", ttl_seconds=5, now=123.9,
    )
    payload = json.loads(base64.urlsafe_b64decode(tok.split(".")[2] + "=="))
    assert payload["exp"] == 128  # int(123.9) + 5


def test_tokens_differ_per_request():
    args = dict(
        private_seed=SEED, key_id="k", namespace="n", sandbox_name="s",
        sandbox_uid="u", port=1, method="GET", path="/", ttl_seconds=60,
    )
    assert mint_scoped_token_v2(**args, now=time.time()) != mint_scoped_token_v2(
        **args, now=time.time() + 1
    )