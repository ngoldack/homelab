"""REST file transport against a REAL local HTTP server (no cluster needed).

The transport decision for the capability layer (PUT/GET/DELETE on sandboxd's
FilesystemService through the Router) is only as good as the HTTP it emits:
method, path shape, scoped-token binding, body framing and status handling. This
module drives the real ``SandboxTransport`` -> ``SdkCommandConnector`` code over
a loopback server that behaves like sandboxd's documented API, so a mistake in
any of those details fails here instead of only in the cluster.
"""

from __future__ import annotations

import base64
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, unquote, urlparse

import pytest

from hermes_agent_sandbox.errors import SandboxPathError, SandboxTransportError
from hermes_agent_sandbox.transport import SandboxTransport

from _guest import make_config

SEED = b"s" * 32


def _decode_scoped_token(token: str) -> dict:
    version, kid, payload, _signature = token.split(".")
    padded = payload + "=" * (-len(payload) % 4)
    return json.loads(base64.urlsafe_b64decode(padded))


class SandboxdHandler(BaseHTTPRequestHandler):
    """Minimal stand-in for sandboxd's FilesystemService REST surface."""

    protocol_version = "HTTP/1.1"
    files: dict = {}
    requests: list = []
    directory_body = {"path": "/workspace", "entries": [{"name": "a.txt", "size": 1, "type": "file"}]}

    def log_message(self, *args):  # keep pytest output clean
        return

    def _record(self, body: bytes) -> None:
        auth = self.headers.get("Authorization", "")
        claims = _decode_scoped_token(auth.split(" ", 1)[1]) if auth.startswith("Bearer ") else {}
        self.requests.append(
            {
                "method": self.command,
                "path": self.path,
                "content_type": self.headers.get("Content-Type"),
                "sandbox_headers": {
                    key: self.headers.get(key)
                    for key in ("X-Sandbox-ID", "X-Sandbox-Namespace", "X-Sandbox-Port", "X-Sandbox-Uid")
                },
                "claims": claims,
                "body": body,
            }
        )

    def _respond(self, status: int, payload: bytes = b"", content_type: str = "application/json") -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        if payload:
            self.wfile.write(payload)

    def _error(self, status: int, code: str) -> None:
        self._respond(status, json.dumps({"code": code, "message": code}).encode())

    def do_PUT(self):  # noqa: N802 - BaseHTTPRequestHandler contract
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length) if length else b""
        self._record(body)
        path = unquote(urlparse(self.path).path)
        if "escape" in path:
            self._error(403, "PERMISSION_DENIED")
            return
        if "isdir" in path:
            self._error(400, "INVALID_ARGUMENT")
            return
        self.__class__.files[path] = body
        self._respond(204)

    def do_GET(self):  # noqa: N802
        self._record(b"")
        parsed = urlparse(self.path)
        path = unquote(parsed.path)
        if path.endswith("/listing"):
            self._respond(200, json.dumps(self.directory_body).encode())
            return
        if path in self.__class__.files:
            self._respond(200, self.__class__.files[path], "application/octet-stream")
            return
        self._error(404, "NOT_FOUND")

    def do_DELETE(self):  # noqa: N802
        self._record(b"")
        path = unquote(urlparse(self.path).path)
        self.__class__.files.pop(path, None)
        self._respond(204)


@pytest.fixture
def server(tmp_path: Path):
    SandboxdHandler.files = {}
    SandboxdHandler.requests = []
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), SandboxdHandler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        yield httpd
    finally:
        httpd.shutdown()
        httpd.server_close()


def _transport(server, tmp_path) -> SandboxTransport:
    config = make_config(tmp_path, router_url=f"http://127.0.0.1:{server.server_address[1]}")
    transport = SandboxTransport(config, secret=SEED)
    transport.attach("sbx-1", "uid-1", "10.0.0.5")
    return transport


def test_put_file_sends_octet_stream_body_and_signed_put(server, tmp_path):
    transport = _transport(server, tmp_path)

    transport.put_file("/workspace/src/main.py", b"print(1)\n")

    request = SandboxdHandler.requests[-1]
    assert request["method"] == "PUT"
    assert request["path"] == "/v1/files/workspace/src/main.py"
    assert request["content_type"] == "application/octet-stream"
    assert request["body"] == b"print(1)\n"
    assert SandboxdHandler.files["/v1/files/workspace/src/main.py"] == b"print(1)\n"
    # Identity headers and a token bound to THIS method and path.
    assert request["sandbox_headers"]["X-Sandbox-ID"] == "sbx-1"
    assert request["sandbox_headers"]["X-Sandbox-Uid"] == "uid-1"
    assert request["sandbox_headers"]["X-Sandbox-Port"] == "8080"
    assert request["claims"]["method"] == "PUT"
    assert request["claims"]["path"] == "/v1/files/workspace/src/main.py"
    assert request["claims"]["uid"] == "uid-1"


def test_put_file_403_maps_to_path_error(server, tmp_path):
    transport = _transport(server, tmp_path)
    with pytest.raises(SandboxPathError) as excinfo:
        transport.put_file("/workspace/escape/out.txt", b"x")
    assert "HTTP 403" in str(excinfo.value)


def test_put_file_other_status_is_transport_error(server, tmp_path):
    transport = _transport(server, tmp_path)
    with pytest.raises(SandboxTransportError):
        transport.put_file("/workspace/isdir", b"x")


def test_fetch_file_round_trips_bytes_and_signed_get(server, tmp_path):
    transport = _transport(server, tmp_path)
    transport.put_file("/workspace/data.bin", b"\x00\x01\x02")

    data = transport.fetch_file("/workspace/data.bin")

    assert data == b"\x00\x01\x02"
    request = SandboxdHandler.requests[-1]
    assert request["method"] == "GET"
    assert request["claims"]["method"] == "GET"
    assert request["claims"]["path"] == "/v1/files/workspace/data.bin"


def test_fetch_file_404_is_transport_error(server, tmp_path):
    transport = _transport(server, tmp_path)
    with pytest.raises(SandboxTransportError) as excinfo:
        transport.fetch_file("/workspace/missing.bin")
    assert "not found" in str(excinfo.value)


def test_fetch_file_403_maps_to_path_error(server, tmp_path):
    transport = _transport(server, tmp_path)

    class Forbidden(SandboxdHandler):
        def do_GET(self):  # noqa: N802
            self._record(b"")
            self._error(403, "PERMISSION_DENIED")

    # A second server keeps the shared handler class untouched.
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), Forbidden)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    try:
        config = make_config(tmp_path, router_url=f"http://127.0.0.1:{httpd.server_address[1]}")
        transport = SandboxTransport(config, secret=SEED)
        transport.attach("sbx-1", "uid-1", "10.0.0.5")
        with pytest.raises(SandboxPathError):
            transport.fetch_file("/workspace/escape")
    finally:
        httpd.shutdown()
        httpd.server_close()


def test_delete_file_sends_recursive_query_but_signs_only_the_path(server, tmp_path):
    transport = _transport(server, tmp_path)
    transport.put_file("/workspace/dir/a.txt", b"x")

    transport.delete_file("/workspace/dir/a.txt")

    request = SandboxdHandler.requests[-1]
    assert request["method"] == "DELETE"
    assert request["claims"]["path"] == "/v1/files/workspace/dir/a.txt"

    transport.delete_file("/workspace/dir", recursive=True)
    request = SandboxdHandler.requests[-1]
    assert parse_qs(urlparse(request["path"]).query) == {"recursive": ["true"]}
    # The signature covers the PATH: the query is not part of the signed target
    # (router README: "Query parameters and request bodies are not signed").
    assert request["claims"]["path"] == "/v1/files/workspace/dir"


def test_directory_listing_comes_back_as_json(server, tmp_path):
    transport = _transport(server, tmp_path)
    raw = transport.fetch_file("/workspace/listing")
    payload = json.loads(raw)
    assert payload["entries"][0]["name"] == "a.txt"
