"""In-process guest emulator shared by the capability-layer unit tests.

WHY an emulator instead of per-test ad-hoc fakes: the capability layer talks to
the sandbox through two very different surfaces (the exec channel and the
Router REST filesystem), and the safety properties under test — /workspace
confinement, symlink resolution, parent existence, tar member validation, size
caps — only mean something if the fake behaves like the real sandboxd. This
module models the parts of sandboxd's protocol the plugin actually uses:

* the line-oriented path probe the plugin runs over exec
  (``realpath -m`` + ``test -f/-d/-L`` + ``stat -c %s``),
* ``mkdir -p`` for scratch directories,
* ``tar -czf`` / ``tar -xzf`` implemented with Python's tarfile over a real
  temporary directory, so artifact/checkpoint tests exercise genuine tarballs,
* REST ``PUT``/``GET``/``DELETE /v1/files`` semantics: symlink-aware resolution
  against the root, auto-created parents on PUT, JSON listings for directories.

It is deliberately strict: an unrecognized command raises, so a change in the
plugin's command shapes fails the suite instead of silently passing.
"""

from __future__ import annotations

import io
import json
import posixpath
import shlex
import tarfile
import threading
import time
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Sequence, Tuple

from hermes_agent_sandbox.config import AgentSandboxConfig

WORKSPACE = "/workspace"


class GuestError(RuntimeError):
    """Raised by the emulator for a request the real sandboxd would refuse."""


def _strip_dot_slash(name: str) -> str:
    """Normalize a tar member token the way tar reports it back to us.

    ``./x`` -> ``x`` (what ``getnames()``/``--exclude`` patterns use), while a
    bare ``.`` stays ``.`` because it means "the whole tree". A naive
    ``lstrip('./')`` would eat the leading dot of ``.hermes`` and silently stop
    excluding scratch.
    """
    if name in (".", "./"):
        return "."
    while name.startswith("./"):
        name = name[2:]
    return name.lstrip("/")


class Guest:
    """A symlink-aware stand-in for one sandbox's filesystem + probe surface."""

    def __init__(self, root: str = WORKSPACE):
        self.root = root
        self.files: Dict[str, bytes] = {}
        self.symlinks: Dict[str, str] = {}
        self.extra_dirs: set = set()
        self.tmpdir = None  # set by tests that need real tarball bytes
        self.commands: List[str] = []
        self.puts: List[Tuple[str, bytes]] = []
        self.deletes: List[str] = []
        self.stdin_payloads: List[bytes] = []
        self.signals: List[Tuple[int, str]] = []
        self.processes: Dict[str, List[Tuple[str, Any]]] = {}
        self.mkdir_calls: List[str] = []

    # ---------------- tree helpers ----------------
    def add_file(self, path: str, data: bytes) -> None:
        self.files[self.resolve(path)] = data

    def add_dir(self, path: str) -> None:
        self.extra_dirs.add(self.resolve(path))

    def add_symlink(self, path: str, target: str) -> None:
        self.symlinks[path] = target

    def dirs(self) -> set:
        out = {self.root} | set(self.extra_dirs)
        for path in list(self.files) + list(self.symlinks):
            parent = posixpath.dirname(path)
            while parent and parent != "/":
                out.add(parent)
                parent = posixpath.dirname(parent)
        return out

    def resolve(self, path: str, _depth: int = 0) -> str:
        """``realpath -m`` semantics: follow existing links, keep the rest."""
        if _depth > 20:
            raise GuestError(f"symlink loop resolving {path}")
        if not path.startswith("/"):
            path = posixpath.join(self.root, path)
        normalized = posixpath.normpath(path)
        cur = "/"
        parts = [p for p in normalized.split("/") if p]
        for index, part in enumerate(parts):
            candidate = posixpath.join(cur, part)
            if candidate in self.symlinks:
                target = self.symlinks[candidate]
                if not target.startswith("/"):
                    target = posixpath.join(cur, target)
                rest = "/".join(parts[index + 1 :])
                combined = posixpath.join(target, rest) if rest else target
                return self.resolve(combined, _depth + 1)
            cur = candidate
        return cur

    def exists(self, path: str) -> bool:
        resolved = self.resolve(path)
        return resolved in self.files or resolved in self.dirs()

    # ---------------- exec surface ----------------
    def run(self, command: str, timeout: int) -> Tuple[str, str, int]:
        self.commands.append(command)
        line = command.splitlines()[0] if command else ""
        if line.startswith("p=") and "realpath -m" in command:
            return self._probe(line)
        if command.startswith("mkdir -p -- "):
            path = shlex.split(command[len("mkdir -p -- ") :])[0]
            resolved = self.resolve(path)
            # `-p` creates the whole chain; it only fails when an existing
            # component is not a directory (nothing to model here: the emulator
            # has no non-directory ancestors).
            cursor = resolved
            while cursor and cursor != "/":
                self.extra_dirs.add(cursor)
                cursor = posixpath.dirname(cursor)
            self.mkdir_calls.append(resolved)
            return "", "", 0
        if command.startswith("stat -c %s -- "):
            path = shlex.split(command[len("stat -c %s -- ") :])[0]
            resolved = self.resolve(path)
            if resolved not in self.files:
                return "", f"stat: cannot statx '{path}'", 1
            return f"{len(self.files[resolved])}\n", "", 0
        if command.startswith("tar -czf "):
            return self._tar_create(command)
        if command.startswith("tar -xzf "):
            return self._tar_extract(command)
        if "exec 0<" in command:
            return self._stdin_redirect(command)
        if command.startswith("cd ") and " && " in command:
            head, _, body = command.partition(" && ")
            self.cwd_seen = shlex.split(head[3:])[0]
            return self._run_body(body, self.cwd_seen)
        raise GuestError(f"emulator does not know how to run {command!r}")

    def _run_body(self, body: str, cwd: str) -> Tuple[str, str, int]:
        """Model the shapes the plugin emits behind `cd <cwd> &&`."""
        body = body.strip().strip(";").strip()
        if body in ("cat", "true", ""):
            return "", "", 0
        if body.startswith("cat "):
            path = shlex.split(body[len("cat ") :])[0]
            if not path.startswith("/"):
                path = posixpath.join(cwd, path)
            data = self.files.get(self.resolve(path))
            if data is None:
                return "", f"cat: {path}: No such file or directory", 1
            return data.decode("utf-8", errors="replace"), "", 0
        raise GuestError(f"emulator cannot model command body {body!r}")

    def _probe(self, line: str) -> Tuple[str, str, int]:
        first = line.split("=", 1)[1]
        path = shlex.split(first)[0]
        resolved = self.resolve(path)
        fields = [f"resolved={resolved}"]
        if self.exists(path):
            fields.append("exists=1")
        if path in self.symlinks:
            fields.append("symlink=1")
        if resolved in self.files:
            fields.append("file=1")
            fields.append(f"size={len(self.files[resolved])}")
        if resolved in self.dirs():
            fields.append("dir=1")
            fields.append("size=0")
        return "\n".join(fields) + "\n", "", 0

    def _tar_create(self, command: str) -> Tuple[str, str, int]:
        tokens = shlex.split(command)
        scratch = tokens[2]
        members: List[str] = []
        excludes: List[str] = []
        idx = 3
        while idx < len(tokens):
            token = tokens[idx]
            if token == "-C":
                idx += 2
                continue
            if token == "--":
                idx += 1
                continue
            if token.startswith("--exclude="):
                excludes.append(_strip_dot_slash(token.split("=", 1)[1]))
                idx += 1
                continue
            members.append(_strip_dot_slash(token))
            idx += 1
        buffer = io.BytesIO()
        included: List[str] = []
        with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
            for name in sorted(self.files):
                if not name.startswith(self.root + "/"):
                    continue
                relative = name[len(self.root) + 1 :]
                if not self._member_selected(relative, members, excludes):
                    continue
                data = self.files[name]
                info = tarfile.TarInfo(name="./" + relative)
                info.size = len(data)
                info.mtime = int(time.time())
                archive.addfile(info, io.BytesIO(data))
                included.append(relative)
            for name in sorted(self.dirs()):
                if name == self.root or not name.startswith(self.root + "/"):
                    continue
                relative = name[len(self.root) + 1 :]
                if not self._member_selected(relative, members, excludes):
                    continue
                if relative in included:
                    continue
                info = tarfile.TarInfo(name="./" + relative + "/")
                info.type = tarfile.DIRTYPE
                info.mode = 0o755
                archive.addfile(info)
                included.append(relative)
        payload = buffer.getvalue()
        self.files[self.resolve(scratch)] = payload
        if not included:
            return "", "tar: Cowardly refusing to create an empty archive", 2
        return "", "", 0

    @staticmethod
    def _member_selected(relative: str, members: Sequence[str], excludes: Sequence[str]) -> bool:
        for exclude in excludes:
            if relative == exclude or relative.startswith(exclude + "/"):
                return False
        if "." in members:
            return True
        for member in members:
            trimmed = member.rstrip("/")
            if relative == trimmed or relative.startswith(trimmed + "/"):
                return True
        return False

    def _tar_extract(self, command: str) -> Tuple[str, str, int]:
        tokens = shlex.split(command)
        scratch = tokens[2]
        resolved = self.resolve(scratch)
        if resolved not in self.files:
            return "", f"tar: {scratch}: Cannot open: No such file or directory", 2
        payload = self.files[resolved]
        with tarfile.open(fileobj=io.BytesIO(payload), mode="r:gz") as archive:
            for member in archive.getmembers():
                target = posixpath.normpath(posixpath.join(self.root, member.name))
                if target != self.root and not target.startswith(self.root + "/"):
                    # GNU tar refuses members that escape the extraction root.
                    return "", f"tar: {member.name}: Member name contains '..'", 2
                if member.isdir():
                    self.extra_dirs.add(target)
                    continue
                extracted = archive.extractfile(member)
                self.files[target] = extracted.read() if extracted else b""
        return "", "", 0

    def _stdin_redirect(self, command: str) -> Tuple[str, str, int]:
        """Model `{ exec 0< <file>; <command>; }` for the fallback tests.

        Only the shape the plugin emits is supported: the redirect target is
        read, its bytes are recorded as the process' stdin, and the rest of the
        command string decides the result (only `cat` is meaningful here).
        """
        head, _, tail = command.partition(";")
        redirect = shlex.split(head[head.index("exec 0<") + len("exec 0<") :].strip())[0]
        payload = self.files.get(self.resolve(redirect), b"")
        self.stdin_payloads.append(payload)
        body = tail.strip().rstrip(";").strip().rstrip("}").strip()
        if body.startswith("cd ") and " && " in body:
            body = body.split(" && ", 1)[1].strip().rstrip(";").strip()
        if body in ("cat", ""):
            return payload.decode("utf-8", errors="replace"), "", 0
        raise GuestError(f"emulator cannot model stdin redirect for {body!r}")

    # ---------------- REST surface ----------------
    def put(self, path: str, data: bytes) -> None:
        resolved = self.resolve(path)
        if resolved != self.root and not resolved.startswith(self.root + "/"):
            raise GuestError("HTTP 403 PERMISSION_DENIED: path traversal forbidden")
        parent = posixpath.dirname(resolved)
        # sandboxd's atomicWrite creates parents.
        while parent and parent != "/":
            self.extra_dirs.add(parent)
            parent = posixpath.dirname(parent)
        self.files[resolved] = data
        self.puts.append((resolved, data))

    def get(self, path: str) -> bytes:
        resolved = self.resolve(path)
        if resolved != self.root and not resolved.startswith(self.root + "/"):
            raise GuestError("HTTP 403 PERMISSION_DENIED: path traversal forbidden")
        if resolved in self.files:
            return self.files[resolved]
        if resolved in self.dirs():
            entries = []
            prefix = resolved + "/"
            for name in sorted(self.files):
                if name.startswith(prefix) and "/" not in name[len(prefix) :]:
                    entries.append(
                        {
                            "name": name[len(prefix) :],
                            "size": len(self.files[name]),
                            "type": "file",
                            "modified_at": "2026-09-18T00:00:00Z",
                            "mode": "0644",
                        }
                    )
            for name in sorted(self.dirs()):
                if name == resolved or not name.startswith(prefix):
                    continue
                if "/" in name[len(prefix) :]:
                    continue
                entries.append(
                    {
                        "name": name[len(prefix) :],
                        "size": 0,
                        "type": "directory",
                        "modified_at": "2026-09-18T00:00:00Z",
                        "mode": "0755",
                    }
                )
            return json.dumps({"path": path, "entries": entries}).encode("utf-8")
        raise GuestError("HTTP 404 NOT_FOUND: path does not exist")

    def delete(self, path: str, recursive: bool) -> None:
        resolved = self.resolve(path)
        if resolved not in self.files and resolved not in self.dirs():
            raise GuestError("HTTP 404 NOT_FOUND: path does not exist")
        self.files.pop(resolved, None)
        self.deletes.append(resolved)
        for name in list(self.dirs()):
            if name.startswith(resolved + "/"):
                self.extra_dirs.discard(name)
                self.files.pop(name, None)


class GuestTransport:
    """SandboxTransport surface implemented over a :class:`Guest`.

    Mirrors the real transport's call signatures exactly so the environment and
    module functions can run unmodified: ``run``, ``fetch_file``, ``put_file``,
    ``delete_file``, ``run_with_stdin``, ``start_process``, ``signal_process``.
    """

    def __init__(self, guest: Optional[Guest] = None, *, stdin_native: bool = True):
        self.guest = guest or Guest()
        self.stdin_native = stdin_native
        self.attached: List[Tuple[str, str, str]] = []
        self.closed = 0
        self.cancelled = 0
        self.sandbox_name = "sbx-1"
        self.uid = "uid-1"
        self.pod_ip = "10.0.0.5"
        self.process_scripts: Dict[str, List[Tuple[str, Any]]] = {}
        self.started_commands: List[str] = []
        self._process_id = 400

    # ---------------- lifecycle ----------------
    def attach(self, sandbox_name: str, sandbox_uid: str, pod_ip: str) -> None:
        self.sandbox_name, self.uid, self.pod_ip = sandbox_name, sandbox_uid, pod_ip
        self.attached.append((sandbox_name, sandbox_uid, pod_ip))

    def close(self) -> None:
        self.closed += 1

    def cancel(self) -> None:
        self.cancelled += 1

    # ---------------- exec ----------------
    def run(self, command: str, timeout: int) -> Tuple[str, str, int]:
        return self.guest.run(command, timeout)

    def run_with_stdin(self, command: str, payload: bytes, timeout: int):
        if not self.stdin_native:
            from hermes_agent_sandbox.errors import SandboxUnsupportedError

            raise SandboxUnsupportedError("sandboxd does not implement WriteStdin (test)")
        self.guest.stdin_payloads.append(payload)
        if "cat" in command:
            return payload.decode("utf-8", errors="replace"), "", 0
        return "", "", 0

    # ---------------- REST ----------------
    def fetch_file(self, remote_path: str, timeout: Any = None) -> bytes:
        return self.guest.get(remote_path)

    def put_file(self, remote_path: str, data: bytes, timeout: Any = None, **kwargs: Any) -> None:
        self.guest.put(remote_path, data)

    def delete_file(self, remote_path: str, recursive: bool = False, timeout: Any = None) -> None:
        self.guest.delete(remote_path, recursive)

    # ---------------- background processes ----------------
    def start_process(self, command: str, start_timeout: int):
        self.started_commands.append(command)
        self._process_id += 1
        events = self.process_scripts.get(command, [("exit", 0)])
        return FakeStartedProcess(self._process_id, events, self)

    def signal_process(self, process_id: int, signal_name: str, timeout: int = 30) -> None:
        self.guest.signals.append((process_id, signal_name))
        for script in self.process_scripts.values():
            script.append(("exit", 143 if signal_name == "TERM" else 137))


class FakeStartedProcess:
    """The ``StartedProcess`` surface: ``process_id``, ``events()``, ``cancel()``."""

    def __init__(self, process_id: int, script: List[Tuple[str, Any]], transport: GuestTransport):
        self.process_id = process_id
        # The LIVE list, not a copy: signal_process appends the exit event a
        # signal produces, and the reader must observe it (that is how the
        # emulator models sandboxd's SendSignal reaching a running process).
        self._script = script
        self._transport = transport
        self.cancelled = False
        self._lock = threading.Lock()

    def events(self):
        """Yield the scripted events; a late fake signal appends an exit event."""
        index = 0
        while True:
            with self._lock:
                if index < len(self._script):
                    event = self._script[index]
                else:
                    event = None
            if event is not None:
                index += 1
                yield event
                if event[0] == "exit":
                    return
                continue
            if self.cancelled:
                return
            time.sleep(0.005)

    def cancel(self) -> None:
        self.cancelled = True
        self._transport.cancelled += 1


class FakeClaims:
    """Claim lifecycle stub (same shape as the provider-facing client)."""

    def __init__(self, conditions: Optional[Iterable[Dict[str, Any]]] = None):
        self.created: List[str] = []
        self.deleted: List[str] = []
        self.waited: List[Tuple[str, int]] = []
        self.conditions = list(conditions or [])
        self.fail_delete = False
        self.sandbox_serial = 0

    def create_claim(self, name: str, pod_labels: Optional[Dict[str, str]] = None) -> Dict[str, Any]:
        self.created.append(name)
        return {"metadata": {"name": name}}

    def wait_ready(self, name: str, timeout: int) -> str:
        self.waited.append((name, timeout))
        self.sandbox_serial += 1
        return f"sbx-{self.sandbox_serial}"

    def get_sandbox_uid(self, name: str) -> str:
        return f"uid-{name}"

    def get_sandbox_ip(self, name: str) -> str:
        return "10.0.0.5"

    def get_claim(self, name: str) -> Dict[str, Any]:
        return {"metadata": {"name": name}, "status": {"conditions": self.conditions}}

    def delete_claim(self, name: str) -> bool:
        self.deleted.append(name)
        return not self.fail_delete


def make_config(tmp_path: Path, **overrides: Any) -> AgentSandboxConfig:
    """Config for capability tests: token file on disk, artifact root in tmp."""
    token = tmp_path / "router-token"
    token.write_bytes(b"x" * 32)
    base: Dict[str, Any] = {
        "router_url": "http://router.svc.cluster.local:8080",
        "token_file": str(token),
        "artifact_root": str(tmp_path / "artifacts"),
    }
    base.update(overrides)
    return AgentSandboxConfig(**base)


def make_env(
    tmp_path: Path,
    *,
    guest: Optional[Guest] = None,
    transport: Optional[GuestTransport] = None,
    claims: Optional[FakeClaims] = None,
    task_id: str = "t",
    stdin_native: bool = True,
    **overrides: Any,
):
    """Build an AgentSandboxEnvironment wired to the emulator (no cluster).

    ``stdin_native`` is a TRANSPORT knob (whether the fake sandboxd implements
    WriteStdin), not a config field; every other keyword is a config override.
    """
    from hermes_agent_sandbox.environment import AgentSandboxEnvironment

    guest = guest or (transport.guest if transport else Guest())
    transport = transport or GuestTransport(guest, stdin_native=stdin_native)
    return AgentSandboxEnvironment(
        make_config(tmp_path, **overrides),
        task_id=task_id,
        claims=claims or FakeClaims(),
        transport=transport,
    ), transport, guest
