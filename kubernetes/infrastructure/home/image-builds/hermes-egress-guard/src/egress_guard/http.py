"""Shared stdlib HTTP plumbing for both services (no framework, no deps).

Both roles speak plain HTTP/1.1 on :8080. The handlers here are deliberately
tiny: request parsing, a bounded body read and byte-exact responses (the
reaper must verify the HMAC over the *raw* event body, so the body is read as
bytes and only decoded afterwards).
"""

from __future__ import annotations

import json
import logging
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Optional

LOG = logging.getLogger("egress_guard")

# An ext_authz check request or a violation event is a few hundred bytes; 64 KiB
# is generous headroom and bounds memory per connection on a public-ish path.
MAX_BODY_BYTES = 64 * 1024


class HttpResult:
    """A response: status, extra headers, body bytes and content type."""

    __slots__ = ("status", "headers", "body", "content_type")

    def __init__(
        self,
        status: int,
        body: bytes = b"",
        headers: Optional[dict[str, str]] = None,
        content_type: str = "application/json",
    ) -> None:
        self.status = status
        self.headers = headers or {}
        self.body = body
        self.content_type = content_type


def json_result(status: int, payload: Any, headers: Optional[dict[str, str]] = None) -> HttpResult:
    body = json.dumps(payload, sort_keys=True).encode("utf-8")
    return HttpResult(status, body, headers, "application/json")


class JsonHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "egress-guard"
    sys_version = ""
    # Socket timeout so a stalled client cannot pin a worker thread forever.
    timeout = 30

    def log_message(self, fmt: str, *args: object) -> None:
        LOG.info("%s %s", self.address_string(), fmt % args)

    def read_body(self) -> bytes:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0:
            raise ValueError("missing or empty request body")
        if length > MAX_BODY_BYTES:
            raise ValueError(f"request body too large: {length} bytes")
        body = self.rfile.read(length)
        if len(body) != length:
            raise ValueError("truncated request body")
        return body

    def read_json(self) -> Any:
        try:
            return json.loads(self.read_body())
        except json.JSONDecodeError as exc:
            raise ValueError(f"invalid JSON body: {exc}") from None

    def write_result(self, result: HttpResult) -> None:
        try:
            self.send_response(result.status)
            for name, value in result.headers.items():
                self.send_header(name, value)
            self.send_header("Content-Type", result.content_type)
            self.send_header("Content-Length", str(len(result.body)))
            self.end_headers()
            if result.body:
                self.wfile.write(result.body)
        except (BrokenPipeError, ConnectionResetError):
            # The client (Envoy or curl) went away mid-response; nothing to do.
            self.close_connection = True


class GuardServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True
