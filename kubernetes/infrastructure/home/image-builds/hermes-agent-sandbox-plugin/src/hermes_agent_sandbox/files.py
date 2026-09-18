"""Sandbox-native file operations: write, read, list (plan Unit 2.1).

TRANSPORT DECISION (evidence, not guesswork). Writes and reads go over the
sandboxd REST FilesystemService through the Router, as raw
``application/octet-stream``:
``PUT /v1/files/{path}`` (atomic temp+rename, creates parents, 204 on success)
and ``GET /v1/files/{path}`` (file → bytes, directory → ``DirectoryListing``
JSON), both authenticated with the same per-request Ed25519 v2 scoped token the
plugin already uses for fetches.

Why not base64-over-exec (the plan's stated fallback): a real upload path DOES
exist in the pinned runtime, so the fallback would be strictly worse (it would
inflate every payload by 4/3 and cap writes at the gRPC message limit). Evidence:

* sandboxd source at the shipped commit 9a85153590e54cb980f3241f9e7a9228449412c9
  (pinned by hermes-sandbox-runtime/Dockerfile):
  ``packages/sandboxd/pkg/server/filesystem.go`` registers
  ``mux.HandleFunc("PUT /v1/files/{path...}", s.handlePutFile)`` (and GET,
  DELETE; the GET pattern also serves HEAD) and ``handlePutFile`` streams the
  raw body into ``atomicWrite`` with auto-created parents, replying 204;
  ``serveDirectoryListing`` returns ``DirectoryListing{path, entries[]}``.
* ``packages/sandboxd/USER_GUIDE.md`` (same commit) documents the same surface:
  ``PUT /v1/files/{path}`` = "Atomic write (temp file + rename), auto-creates
  parents", ``GET`` = file bytes or directory JSON.
* The plugin's own Router-authenticated PUT/DELETE path is already exercised by
  tests/integration/test_live_sandbox.py::test_file_roundtrip_through_authenticated_router.

PATH CONFINEMENT. Two layers, deliberately redundant:

1. sandboxd evaluates symlinks and refuses anything resolving outside
   ``--root-dir`` /workspace (``packages/sandboxd/pkg/pathutil/sandbox.go``,
   SanitizePath: nearest-existing-ancestor resolution for not-yet-existing
   targets, ``403 PERMISSION_DENIED`` on escape). That is the *enforcement*.
2. This module adds the plugin's own contract, evaluated INSIDE the guest with
   ``realpath -m`` (coreutils is installed in hermes-sandbox-runtime) plus
   ``test -f/-d/-L``: it rejects lexical escapes before a request is made, turns
   an escape into a typed :class:`SandboxPathError` instead of a bare 403, and
   gives callers explicit, actionable errors for missing parents and
   non-regular files. Layer 2 is advisory (a TOCTOU window between probe and
   request exists); layer 1 is what makes the guarantee hold.
"""

from __future__ import annotations

import json
import logging
import posixpath
import shlex
from dataclasses import dataclass
from typing import Any, Dict, List, Optional, Sequence

from .config import WORKSPACE, AgentSandboxConfig
from .errors import (
    SandboxCommandError,
    SandboxFileSizeError,
    SandboxPathError,
    SandboxUnsupportedError,
)

log = logging.getLogger(__name__)

# The plugin's internal scratch namespace, always under the sandbox root
# because sandboxd confines every path to /workspace (a /tmp path is refused
# with 403). Contents: stdin payloads (file-mode fallback) and artifact
# tarballs in flight. Excluded from exports/checkpoints by default.
SCRATCH_DIRNAME = ".hermes"
SCRATCH_DIR = f"{WORKSPACE}/{SCRATCH_DIRNAME}"
STDIN_SCRATCH_DIR = f"{SCRATCH_DIR}/stdin"
ARTIFACT_SCRATCH_DIR = f"{SCRATCH_DIR}/artifacts"

# Path probes are tiny metadata commands; a wedged probe must fail fast instead
# of consuming the session's command timeout (which is meant for real work).
PROBE_TIMEOUT_SECONDS = 30

# Default response budget for one directory listing returned to the model. A
# huge directory is truncated (with an explicit marker) rather than dumped into
# the context window.
LIST_MAX_ENTRIES = 1000

# One in-guest probe yields everything the plugin needs to decide, so a path
# check costs exactly one round trip. The output is line-oriented `key=value`
# because the exec channel returns a single string; values are single tokens
# (paths are quoted by the shell, `%s` of a path may contain spaces — see the
# parser, which splits on the FIRST '=' only).
_PROBE_TEMPLATE = """\
p={quoted}
r=$(realpath -m -- "$p" 2>/dev/null) || exit 3
printf 'resolved=%s\\n' "$r"
[ -e "$p" ] && printf 'exists=1\\n'
[ -L "$p" ] && printf 'symlink=1\\n'
[ -f "$p" ] && printf 'file=1\\n'
[ -d "$p" ] && printf 'dir=1\\n'
if [ -f "$p" ]; then printf 'size=%s\\n' "$(stat -c %s -- "$p" 2>/dev/null || echo 0)"; fi
if [ -d "$p" ]; then printf 'size=0\\n'; fi
exit 0
"""


@dataclass(frozen=True)
class GuestPath:
    """The result of resolving one path inside the sandbox guest.

    ``requested`` is the normalized /workspace path the caller asked for;
    ``resolved`` is what the guest's ``realpath -m`` produced (symlinks
    evaluated, missing leaf components kept lexically). Both are guaranteed
    under /workspace by :func:`resolve_workspace_path`.
    """

    requested: str
    resolved: str
    exists: bool
    is_file: bool
    is_dir: bool
    is_symlink: bool
    size: Optional[int]

    def as_dict(self) -> Dict[str, Any]:
        return {
            "path": self.requested,
            "resolved": self.resolved,
            "exists": self.exists,
            "is_file": self.is_file,
            "is_dir": self.is_dir,
            "is_symlink": self.is_symlink,
            "size": self.size,
        }


# ---------------- lexical layer (pure; no I/O) ----------------
def normalize_workspace_path(requested: str, *, root: str = WORKSPACE) -> str:
    """Canonicalize *requested* lexically and require it under *root*.

    Accepts both absolute (``/workspace/src/main.py``) and root-relative
    (``src/main.py``) spellings, because both read naturally to a model that was
    told the workspace is the project root. Rejects: empty strings, NUL bytes,
    anything that normalizes outside the root (``..`` traversal), a bare ``~``
    (the guest's HOME is /tmp, so tilde expansion is never what a workspace tool
    wants), and paths that would be the root itself only when the caller asked
    for something else (the root is a legitimate input for list/export, so it is
    allowed and returned as ``/workspace``).
    """
    if not isinstance(requested, str) or not requested.strip():
        raise SandboxPathError("path must be a non-empty string")
    if "\x00" in requested:
        raise SandboxPathError("path must not contain NUL bytes")
    candidate = requested.strip()
    if candidate.startswith("~"):
        raise SandboxPathError(
            f"path {requested!r} uses '~' expansion, which is not supported; "
            f"paths are confined to {root}"
        )
    if not posixpath.isabs(candidate):
        candidate = posixpath.join(root, candidate)
    normalized = posixpath.normpath(candidate)
    if normalized != root and not normalized.startswith(root + "/"):
        raise SandboxPathError(
            f"path {requested!r} resolves outside the sandbox root {root}"
        )
    return normalized


def is_scratch_path(path: str, *, root: str = WORKSPACE) -> bool:
    """True for the plugin's own scratch namespace (never exported/imported)."""
    scratch = f"{root}/{SCRATCH_DIRNAME}"
    return path == scratch or path.startswith(scratch + "/")


def require_scratch_path(path: str, *, root: str = WORKSPACE) -> None:
    """Refuse a path OUTSIDE the plugin scratch namespace (internal writers).

    The mirror image of :func:`require_not_scratch`: an internal writer that
    lands outside scratch would let a bug drop payloads into the session's own
    workspace (and into every checkpoint).
    """
    if not is_scratch_path(path, root=root):
        raise SandboxPathError(
            f"path {path!r} is not inside the plugin scratch directory {root}/{SCRATCH_DIRNAME}"
        )


def require_not_scratch(path: str, *, root: str = WORKSPACE) -> None:
    """Refuse caller-supplied paths inside the plugin's scratch namespace.

    Scratch holds in-flight stdin payloads and artifact tarballs. Exporting it
    would archive a tarball while it is being written; importing into it would
    let an artifact overwrite plugin state. Both are refused loudly.
    """
    if is_scratch_path(path, root=root):
        raise SandboxPathError(
            f"path {path!r} is inside the plugin scratch directory "
            f"{root}/{SCRATCH_DIRNAME}; it is internal and cannot be addressed directly"
        )


# ---------------- guest-side layer ----------------
def probe_guest_path(
    transport: Any,
    path: str,
    *,
    timeout: int = PROBE_TIMEOUT_SECONDS,
    root: str = WORKSPACE,
) -> GuestPath:
    """Resolve *path* inside the guest and report its type (one exec).

    ``realpath -m`` is used deliberately: ``-m`` resolves what exists and keeps
    the rest lexically, so a write target that does not exist yet still gets a
    symlink-evaluated parent chain — which is exactly the case sandboxd's own
    SanitizePath handles for PUT. A missing ``realpath``/``stat`` toolchain is
    reported as :class:`SandboxUnsupportedError` rather than silently degrading
    to a lexical check, because a lexical-only check would claim a safety
    property the plugin cannot then honour.
    """
    requested = normalize_workspace_path(path, root=root)
    command = _PROBE_TEMPLATE.format(quoted=shlex.quote(requested))
    try:
        stdout, stderr, code = transport.run(command, timeout)
    except SandboxCommandError:
        raise
    except Exception as exc:  # noqa: BLE001 - unexpected transport failure
        raise SandboxCommandError(f"path probe for {requested!r} failed: {exc}") from exc

    fields = _parse_probe_output(stdout)
    if not fields.get("resolved"):
        detail = (stderr or stdout or "").strip() or f"exit code {code}"
        if code == 3 or "realpath" in detail:
            raise SandboxUnsupportedError(
                "the sandbox image cannot resolve paths (coreutils `realpath` "
                f"missing or failing: {detail})"
            )
        raise SandboxCommandError(f"path probe for {requested!r} returned {detail!r}")

    resolved = fields["resolved"]
    if resolved != root and not resolved.startswith(root + "/"):
        raise SandboxPathError(
            f"path {requested!r} resolves to {resolved!r}, outside the sandbox root {root}"
        )
    size: Optional[int] = None
    if "size" in fields:
        try:
            size = int(fields["size"])
        except ValueError:
            size = None
    return GuestPath(
        requested=requested,
        resolved=resolved,
        exists=bool(fields.get("exists")),
        is_file=bool(fields.get("file")),
        is_dir=bool(fields.get("dir")),
        is_symlink=bool(fields.get("symlink")),
        size=size,
    )


def _parse_probe_output(stdout: str) -> Dict[str, str]:
    """Parse the line-oriented probe output; ignore anything unexpected."""
    fields: Dict[str, str] = {}
    for line in (stdout or "").splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        if key in ("resolved", "exists", "symlink", "file", "dir", "size"):
            fields[key] = value.strip()
    return fields


def resolve_workspace_path(
    transport: Any,
    path: str,
    *,
    must_exist: bool = False,
    kind: Optional[str] = None,
    timeout: int = PROBE_TIMEOUT_SECONDS,
) -> GuestPath:
    """Probe *path* and enforce the plugin's path contract.

    *kind* is ``"file"`` or ``"dir"``; ``None`` accepts either (or nothing yet).
    Symlinks are followed by the guest probe, so a symlink pointing outside the
    root is rejected by the ``resolves outside`` check, and a symlink pointing
    at a regular file inside the root is accepted like sandboxd would accept it.
    """
    probed = probe_guest_path(transport, path, timeout=timeout)
    if must_exist and not probed.exists:
        raise SandboxPathError(f"{probed.requested!r} does not exist in the sandbox")
    if kind == "file":
        if not probed.is_file:
            raise SandboxPathError(
                f"{probed.requested!r} is not a regular file "
                f"(exists={probed.exists}, dir={probed.is_dir})"
            )
    elif kind == "dir":
        if not probed.is_dir:
            raise SandboxPathError(
                f"{probed.requested!r} is not a directory (exists={probed.exists})"
            )
    elif kind is not None:
        raise ValueError(f"unsupported kind {kind!r}")
    return probed


# ---------------- operations ----------------
def write_file(
    transport: Any,
    config: AgentSandboxConfig,
    path: str,
    content: Any,
    *,
    create_parents: bool = False,
    timeout: Optional[int] = None,
) -> Dict[str, Any]:
    """Write *content* to a caller-visible /workspace path (Router PUT).

    *create_parents* defaults to False: the plan requires a missing parent to be
    an explicit error, even though sandboxd would happily create it — silent
    directory creation hides a wrong path (``/workspace/src/maim.py`` typo'd as
    a new tree) instead of surfacing it.

    A write over an existing directory is refused before the request; an
    existing symlink-to-file inside the root is written through (the guest probe
    already proved where it points). The plugin's own scratch namespace is not
    addressable here — internal callers use :func:`write_scratch_file`.
    """
    target = normalize_workspace_path(path)
    require_not_scratch(target)
    return _write_bytes(
        transport, config, target, content, create_parents=create_parents, timeout=timeout
    )


def write_scratch_file(
    transport: Any,
    config: AgentSandboxConfig,
    path: str,
    content: Any,
    *,
    timeout: Optional[int] = None,
) -> Dict[str, Any]:
    """Write into the plugin's scratch namespace (internal callers only).

    Split from :func:`write_file` rather than exposed as a flag: scratch writes
    (stdin payloads, artifact tarballs) must never be reachable from a
    model-supplied path, and a boolean escape hatch is one refactor away from
    being passed the wrong value. Parents are always created here — scratch
    directories are created on demand by construction.
    """
    target = normalize_workspace_path(path)
    require_scratch_path(target)
    return _write_bytes(
        transport, config, target, content, create_parents=True, timeout=timeout
    )


def _write_bytes(
    transport: Any,
    config: AgentSandboxConfig,
    target: str,
    content: Any,
    *,
    create_parents: bool,
    timeout: Optional[int],
) -> Dict[str, Any]:
    """Shared write core: size cap, path probe, parent policy, PUT."""
    data = _to_bytes(content)
    limit = config.file_write_max_bytes
    if len(data) > limit:
        raise SandboxFileSizeError(
            f"content for {target!r} is {len(data)} bytes, over the "
            f"{limit} byte write limit; use export_artifact for large payloads"
        )
    probed = resolve_workspace_path(transport, target)
    if probed.is_dir:
        raise SandboxPathError(f"{target!r} is a directory")
    if not probed.exists:
        parent = posixpath.dirname(probed.resolved)
        if not create_parents:
            resolve_workspace_path(
                transport, parent, must_exist=True, kind="dir"
            )  # raises SandboxPathError naming the missing parent
        elif not _dir_exists(transport, parent):
            ensure_dir(transport, parent)
    elif not probed.is_file:
        raise SandboxPathError(
            f"{target!r} exists but is not a regular file (symlink={probed.is_symlink})"
        )
    transport.put_file(target, data, timeout=timeout)
    log.info("sandbox wrote %d bytes to %s", len(data), target)
    return {
        "path": target,
        "resolved": probed.resolved,
        "bytes": len(data),
        "created": not probed.exists,
    }


def _dir_exists(transport: Any, path: str) -> bool:
    try:
        resolve_workspace_path(transport, path, must_exist=True, kind="dir")
        return True
    except SandboxPathError:
        return False


def ensure_dir(transport: Any, path: str) -> None:
    """Create a directory chain in the guest (``mkdir -p``).

    Only used for the plugin's own scratch paths; the REST API would create
    parents on PUT anyway, but creating them first makes a permission failure
    (read-only rootfs, wrong uid) an explicit error instead of a confusing 403
    from the write.
    """
    stdout, stderr, code = transport.run(
        f"mkdir -p -- {shlex.quote(path)}", PROBE_TIMEOUT_SECONDS
    )
    if code != 0:
        raise SandboxCommandError(
            f"could not create {path!r} in the sandbox: "
            f"{(stderr or stdout).strip() or f'exit code {code}'}"
        )


def read_file(
    transport: Any,
    config: AgentSandboxConfig,
    path: str,
    *,
    max_bytes: Optional[int] = None,
    encoding: str = "utf-8",
    errors: str = "replace",
    timeout: Optional[int] = None,
) -> Dict[str, Any]:
    """Read a regular file from the sandbox as text.

    The size is checked from the guest probe BEFORE the body is transferred, so
    an oversized file costs one ``stat`` instead of a multi-megabyte download;
    the post-transfer length check stays as a TOCTOU guard (the file can grow
    between probe and GET).
    """
    limit = int(max_bytes) if max_bytes is not None else config.file_read_max_bytes
    target = normalize_workspace_path(path)
    require_not_scratch(target)
    probed = resolve_workspace_path(transport, target, must_exist=True, kind="file")
    if probed.size is not None and probed.size > limit:
        raise SandboxFileSizeError(
            f"{target!r} is {probed.size} bytes, over the {limit} byte read limit; "
            "export it as an artifact or read it in chunks"
        )
    data = transport.fetch_file(target, timeout=timeout)
    if len(data) > limit:
        raise SandboxFileSizeError(
            f"{target!r} grew to {len(data)} bytes, over the {limit} byte read limit"
        )
    return {
        "path": target,
        "bytes": len(data),
        "content": data.decode(encoding, errors=errors),
        "resolved": probed.resolved,
    }


def list_dir(
    transport: Any,
    config: AgentSandboxConfig,
    path: str = WORKSPACE,
    *,
    max_entries: int = LIST_MAX_ENTRIES,
    timeout: Optional[int] = None,
) -> Dict[str, Any]:
    """List a directory through sandboxd's ``DirectoryListing`` GET.

    sandboxd returns JSON for directories and raw bytes for files; asking for a
    file here is a caller error, so the probe's kind check runs first and the
    JSON parse is validated (an unexpected body is reported as a transport
    failure, never half-interpreted).
    """
    target = normalize_workspace_path(path)
    require_not_scratch(target)
    resolve_workspace_path(transport, target, must_exist=True, kind="dir")
    raw = transport.fetch_file(target, timeout=timeout)
    try:
        payload = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, ValueError) as exc:
        raise SandboxCommandError(
            f"directory listing for {target!r} was not JSON ({len(raw)} bytes)"
        ) from exc
    entries = payload.get("entries") if isinstance(payload, dict) else None
    if not isinstance(entries, list):
        raise SandboxCommandError(
            f"directory listing for {target!r} had no `entries` array"
        )
    cleaned = [
        {
            "name": entry.get("name", ""),
            "type": entry.get("type", "file"),
            "size": entry.get("size", 0),
            "modified_at": entry.get("modified_at", ""),
            "mode": entry.get("mode", ""),
        }
        for entry in entries
        if isinstance(entry, dict)
    ]
    truncated = len(cleaned) > max_entries
    return {
        "path": target,
        "count": len(cleaned),
        "entries": cleaned[:max_entries],
        "truncated": truncated,
    }


def delete_path(transport: Any, path: str, *, recursive: bool = False, timeout: Optional[int] = None) -> None:
    """Delete a file or directory through the Router (best-effort callers)."""
    target = normalize_workspace_path(path)
    require_not_scratch(target)
    transport.delete_file(target, recursive=recursive, timeout=timeout)


def scratch_file_path(subdir: str, name: str, *, root: str = WORKSPACE) -> str:
    """Build an absolute scratch path under the plugin's internal namespace.

    Names come from the plugin itself (a hex id) or from an artifact id, never
    from a model, but the check is still strict: separators are refused rather
    than filtered (a filtered separator would turn ``a/../b`` into a *different*
    scratch file instead of an error), and a name that is all dots is refused
    because ``.``/``..`` as a basename is never a file.
    """
    if not isinstance(name, str) or not name or "/" in name or "\\" in name:
        raise SandboxPathError(f"invalid scratch file name {name!r}")
    safe_name = "".join(c for c in name if c.isalnum() or c in "-_.")
    if not safe_name or set(safe_name) == {"."}:
        raise SandboxPathError(f"invalid scratch file name {name!r}")
    return normalize_workspace_path(f"{root}/{SCRATCH_DIRNAME}/{subdir}/{safe_name}", root=root)


def relative_members(paths: Sequence[str], *, root: str = WORKSPACE) -> List[str]:
    """Convert /workspace paths into tar members relative to the root.

    GNU tar is invoked with ``-C /workspace``, so members must be relative;
    ``.`` denotes the root itself (a whole-workspace export). Callers have
    already normalized + confined every path.
    """
    members: List[str] = []
    for path in paths:
        normalized = normalize_workspace_path(path, root=root)
        members.append("." if normalized == root else normalized[len(root) + 1 :])
    return members


def _to_bytes(content: Any) -> bytes:
    if isinstance(content, bytes):
        return content
    if isinstance(content, bytearray):
        return bytes(content)
    if isinstance(content, str):
        return content.encode("utf-8")
    raise SandboxPathError(
        f"content must be str or bytes, got {type(content).__name__}"
    )
