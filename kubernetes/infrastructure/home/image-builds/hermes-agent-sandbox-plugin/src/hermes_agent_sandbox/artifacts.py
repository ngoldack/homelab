"""Artifacts and workspace checkpoints (plan Units 2.3 + 2.6).

An artifact is a gzip tarball produced INSIDE the guest and stored on the
gateway's own filesystem (the Hermes PVC at ``AGENT_SANDBOX_ARTIFACT_ROOT``),
addressed by an opaque id with an expiry. It is the only way bytes leave a
disposable sandbox in more than one piece:

* the exec channel returns one gRPC message (4 MiB default) and decodes stdout
  as UTF-8 — unusable for a binary tarball;
* sandboxd's REST FilesystemService streams a file with no size cap
  (``http.ServeContent`` in packages/sandboxd/pkg/server/filesystem.go), which
  is exactly what a tarball needs.

TRANSPORT RECIPE (each step's evidence is in files.py's module docstring):

1. ``tar -czf <scratch>/<id>.tar.gz -C /workspace -- <members>`` via the exec
   channel. The scratch file lives under /workspace/.hermes/artifacts because
   sandboxd confines every REST path to the sandbox root — /tmp is NOT
   reachable over REST (403), so a /tmp tarball could never be downloaded.
2. ``stat -c %s`` the tarball and refuse before downloading if it exceeds the
   cap (an over-cap artifact must cost one stat, not a 500 MiB transfer).
3. ``GET /v1/files/<scratch path>`` (the existing scoped-token fetch) and write
   it to ``<root>/<id>.tar.gz`` via a temp file + rename.
4. ``DELETE /v1/files/<scratch path>`` so a running sandbox does not accumulate
   scratch tarballs between exports.

``import_artifact`` reverses it: PUT the stored tarball into the guest scratch
dir, validate the member list (absolute paths and ``..`` traversal refused
BEFORE extraction; the listing is bounded so a pathological archive is refused
rather than under-validated), ``tar -xzf`` into /workspace, then delete the
guest copy. GNU tar's own member-name protections are the second line, not the
first: we refuse what we can prove wrong and fail on any non-zero tar exit.

CHECKPOINTS are the thin, opinionated wrapper: ``checkpoint()`` exports the
whole workspace minus the plugin's scratch namespace, and
``AgentSandboxEnvironment.restore_checkpoint()`` re-imports it into a FRESH claim. The plain workspace
remains disposable — a session that needs its files back must checkpoint before
the sandbox is recycled or its claim expires (default 4 h,
``AGENT_SANDBOX_MAX_LIFETIME_SECONDS``).
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import posixpath
import re
import secrets
import shlex
import tarfile
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence, Tuple

from . import files as files_mod
from .config import WORKSPACE, AgentSandboxConfig
from .errors import (
    SandboxArtifactError,
    SandboxArtifactExpiredError,
    SandboxArtifactTooLargeError,
)

log = logging.getLogger(__name__)

DATA_SUFFIX = ".tar.gz"
META_SUFFIX = ".json"
ID_BYTES = 16  # -> 32 hex chars, opaque and filesystem-safe
_ARTIFACT_ID_RE = re.compile(r"^[0-9a-f]{32}$")

# tar's own exit codes: 0 = ok, 1 = "some files changed/vanished while reading"
# (reported, since a silently incomplete artifact is worse than a retry),
# 2 = fatal.
TAR_EXIT_WARNING = 1


@dataclass(frozen=True)
class ArtifactRef:
    """Metadata for one stored artifact (persisted next to the tarball)."""

    id: str
    bytes: int
    created_at: float
    expires_at: float
    sha256: str
    paths: Tuple[str, ...]
    session: str = ""
    kind: str = "artifact"  # "artifact" | "checkpoint"

    @property
    def ttl_seconds(self) -> float:
        return max(0.0, self.expires_at - self.created_at)

    def is_expired(self, now: Optional[float] = None) -> bool:
        return (now if now is not None else time.time()) >= self.expires_at

    def as_dict(self) -> Dict[str, Any]:
        return {
            "id": self.id,
            "bytes": self.bytes,
            "created_at": self.created_at,
            "expires_at": self.expires_at,
            "sha256": self.sha256,
            "paths": list(self.paths),
            "session": self.session,
            "kind": self.kind,
        }

    @classmethod
    def from_dict(cls, payload: Dict[str, Any]) -> "ArtifactRef":
        return cls(
            id=payload["id"],
            bytes=int(payload["bytes"]),
            created_at=float(payload["created_at"]),
            expires_at=float(payload["expires_at"]),
            sha256=payload.get("sha256", ""),
            paths=tuple(payload.get("paths") or ()),
            session=payload.get("session", ""),
            kind=payload.get("kind", "artifact"),
        )


class ArtifactStore:
    """Gateway-side artifact store: one tarball + one JSON sidecar per artifact.

    Directory layout (both files share the id, so a sweep or a hand inspection
    never has to guess): ``<root>/<id>.tar.gz`` and ``<root>/<id>.json``.
    """

    def __init__(self, root: str | os.PathLike, *, clock: Any = time.time):
        self.root = Path(root)
        self._clock = clock

    # ---------------- store plumbing ----------------
    def _ensure_root(self) -> None:
        try:
            self.root.mkdir(parents=True, exist_ok=True)
        except OSError as exc:
            raise SandboxArtifactError(
                f"artifact root {self.root} is not usable: {exc}"
            ) from exc

    def data_path(self, artifact_id: str) -> Path:
        self._validate_id(artifact_id)
        return self.root / f"{artifact_id}{DATA_SUFFIX}"

    def meta_path(self, artifact_id: str) -> Path:
        self._validate_id(artifact_id)
        return self.root / f"{artifact_id}{META_SUFFIX}"

    @staticmethod
    def _validate_id(artifact_id: str) -> None:
        # An id is opaque but NOT untrusted-shaped: reject anything that is not
        # exactly 32 lowercase hex chars, so a caller cannot steer the store at
        # another path with `../` or an absolute id.
        if not isinstance(artifact_id, str) or not _ARTIFACT_ID_RE.match(artifact_id):
            raise SandboxArtifactError(f"invalid artifact id {artifact_id!r}")

    def load_meta(self, artifact_id: str) -> ArtifactRef:
        """Read one artifact's sidecar; missing/expired are distinct errors."""
        meta_path = self.meta_path(artifact_id)
        try:
            payload = json.loads(meta_path.read_text(encoding="utf-8"))
        except FileNotFoundError as exc:
            raise SandboxArtifactExpiredError(
                f"artifact {artifact_id!r} is unknown (no metadata at {meta_path.name})"
            ) from exc
        except (OSError, ValueError) as exc:
            raise SandboxArtifactError(
                f"artifact {artifact_id!r} metadata is unreadable: {exc}"
            ) from exc
        ref = ArtifactRef.from_dict(payload)
        if ref.is_expired(self._clock()):
            expired_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(ref.expires_at))
            raise SandboxArtifactExpiredError(
                f"artifact {artifact_id!r} expired at {expired_at}"
            )
        return ref

    def list_artifacts(self, *, include_expired: bool = False) -> List[ArtifactRef]:
        """Every artifact the store knows about, newest first."""
        if not self.root.is_dir():
            return []
        refs: List[ArtifactRef] = []
        for meta in sorted(self.root.glob(f"*{META_SUFFIX}")):
            try:
                ref = ArtifactRef.from_dict(json.loads(meta.read_text(encoding="utf-8")))
            except (OSError, ValueError, KeyError):
                continue  # a torn sidecar is ignored, not fatal
            if include_expired or not ref.is_expired(self._clock()):
                refs.append(ref)
        return sorted(refs, key=lambda ref: ref.created_at, reverse=True)

    def sweep(self, *, include_orphans: bool = True) -> List[str]:
        """Delete expired artifacts; returns the ids removed.

        Runs on every export (cheap: one glob) so a long-lived gateway pod does
        not need a CronJob for the common case, and so the PVC cannot fill with
        artifacts nobody will ever read again. Orphan data files (tarball
        without a sidecar, e.g. a crash between the two renames) are removed
        too, but only when the sweep is asked to.
        """
        removed: List[str] = []
        now = self._clock()
        for ref in self.list_artifacts(include_expired=True):
            if ref.is_expired(now):
                self._remove_files(ref.id)
                removed.append(ref.id)
                log.info("artifact %s expired; removed", ref.id)
        if include_orphans:
            for data in self.root.glob(f"*{DATA_SUFFIX}"):
                artifact_id = data.name[: -len(DATA_SUFFIX)]
                if not _ARTIFACT_ID_RE.match(artifact_id):
                    continue
                if not self.meta_path(artifact_id).exists():
                    self._remove_files(artifact_id)
                    removed.append(artifact_id)
                    log.warning("artifact %s had no metadata; removed orphan tarball", artifact_id)
        return removed

    def _remove_files(self, artifact_id: str) -> None:
        for path in (self.data_path(artifact_id), self.meta_path(artifact_id)):
            try:
                path.unlink()
            except FileNotFoundError:
                continue
            except OSError as exc:  # noqa: BLE001 - sweep is best-effort
                log.warning("could not remove artifact file %s: %s", path, exc)

    # ---------------- export ----------------
    def export(
        self,
        transport: Any,
        config: AgentSandboxConfig,
        paths: Sequence[str],
        *,
        exclude: Sequence[str] = (),
        ttl_hours: Optional[float] = None,
        max_bytes: Optional[int] = None,
        session: str = "",
        kind: str = "artifact",
        timeout: Optional[int] = None,
    ) -> ArtifactRef:
        """Tar *paths* inside the guest, pull the tarball, store it with a TTL."""
        if not paths:
            raise SandboxArtifactError("export_artifact requires at least one path")
        cap = int(max_bytes) if max_bytes is not None else config.artifact_max_bytes
        if cap <= 0:
            raise SandboxArtifactError(f"max_bytes must be positive, got {cap}")
        ttl = self._resolve_ttl(config, ttl_hours)
        members: List[str] = []
        for path in paths:
            normalized = files_mod.normalize_workspace_path(path)
            files_mod.require_not_scratch(normalized)
            files_mod.resolve_workspace_path(transport, normalized, must_exist=True)
            members.append(normalized)
        for path in exclude:
            # Excludes are advisory patterns for tar, but they must still be
            # confined: a caller must not be able to aim tar at /etc.
            files_mod.normalize_workspace_path(path)

        self._ensure_root()
        self.sweep()
        artifact_id = secrets.token_hex(ID_BYTES)
        host_data = self.data_path(artifact_id)

        scratch_rel = files_mod.relative_members(
            [files_mod.scratch_file_path("artifacts", f"{artifact_id}{DATA_SUFFIX}")]
        )[0]
        scratch_abs = f"{WORKSPACE}/{scratch_rel}"
        files_mod.ensure_dir(transport, posixpath.dirname(scratch_abs))

        tar_members = files_mod.relative_members(members)
        tar_command = self._tar_create_command(scratch_abs, tar_members, exclude)
        stdout, stderr, code = transport.run(tar_command, transport_timeout(timeout))
        if code == TAR_EXIT_WARNING:
            # GNU tar exit 1 = "some files changed/vanished while reading".
            # The artifact is still usable, but a caller must be able to see
            # that the snapshot may be internally inconsistent.
            log.warning(
                "tar reported an incomplete read for artifact %s (exit 1): %s",
                artifact_id, stderr.strip() or "no stderr",
            )
        elif code != 0:
            raise SandboxArtifactError(
                f"tar failed in the sandbox (exit {code}): "
                f"{(stderr or stdout).strip() or 'no stderr'}"
            )

        size = self._guest_size(transport, scratch_abs)
        if size > cap:
            self._delete_guest_scratch(transport, scratch_abs)
            raise SandboxArtifactTooLargeError(
                f"artifact for {list(members)} is {size} bytes, over the {cap} byte cap; "
                "raise AGENT_SANDBOX_ARTIFACT_MAX_BYTES or export fewer paths"
            )

        try:
            data = transport.fetch_file(scratch_abs, timeout=transport_timeout(timeout))
        except Exception:
            self._delete_guest_scratch(transport, scratch_abs)
            raise
        if len(data) > cap:
            raise SandboxArtifactTooLargeError(
                f"artifact for {list(members)} grew to {len(data)} bytes, over the "
                f"{cap} byte cap"
            )
        digest = hashlib.sha256(data).hexdigest()
        created = self._clock()
        ref = ArtifactRef(
            id=artifact_id,
            bytes=len(data),
            created_at=created,
            expires_at=created + ttl,
            sha256=digest,
            paths=tuple(members),
            session=session,
            kind=kind,
        )
        self._write_atomic(host_data, data)
        self._write_atomic(
            self.meta_path(artifact_id),
            json.dumps(ref.as_dict(), separators=(",", ":")).encode("utf-8"),
        )
        self._delete_guest_scratch(transport, scratch_abs)
        log.info(
            "exported %s (%d bytes, %d path(s), ttl %ds) as artifact %s",
            kind, len(data), len(members), int(ttl), artifact_id,
        )
        return ref

    def _resolve_ttl(self, config: AgentSandboxConfig, ttl_hours: Optional[float]) -> float:
        if ttl_hours is None:
            hours = float(config.artifact_default_ttl_hours)
        else:
            try:
                hours = float(ttl_hours)
            except (TypeError, ValueError) as exc:
                raise SandboxArtifactError(f"ttl_hours must be numeric, got {ttl_hours!r}") from exc
        if hours <= 0:
            raise SandboxArtifactError(f"ttl_hours must be positive, got {hours}")
        if hours > config.artifact_max_ttl_hours:
            raise SandboxArtifactError(
                f"ttl_hours {hours} exceeds the {config.artifact_max_ttl_hours}h cap; "
                "raise AGENT_SANDBOX_ARTIFACT_MAX_TTL_HOURS to allow it"
            )
        return hours * 3600.0

    @staticmethod
    def _tar_create_command(
        scratch_abs: str, members: Sequence[str], exclude: Sequence[str]
    ) -> str:
        """Build the in-guest ``tar -czf`` command (all arguments quoted)."""
        excludes = []
        for pattern in exclude:
            normalized = files_mod.normalize_workspace_path(pattern)
            member = files_mod.relative_members([normalized])[0]
            excludes.append(f"--exclude={shlex.quote('./' + member)}")
        excludes.append(f"--exclude={shlex.quote('./' + files_mod.SCRATCH_DIRNAME)}")
        quoted_members = " ".join(shlex.quote(member) for member in members)
        return (
            f"tar -czf {shlex.quote(scratch_abs)} -C {shlex.quote(WORKSPACE)} "
            f"{' '.join(excludes)} -- {quoted_members}"
        )

    @staticmethod
    def _guest_size(transport: Any, guest_path: str) -> int:
        stdout, stderr, code = transport.run(
            f"stat -c %s -- {shlex.quote(guest_path)}", files_mod.PROBE_TIMEOUT_SECONDS
        )
        if code != 0:
            raise SandboxArtifactError(
                f"could not stat {guest_path!r} in the sandbox: "
                f"{(stderr or stdout).strip() or f'exit code {code}'}"
            )
        try:
            return int(stdout.strip())
        except ValueError as exc:
            raise SandboxArtifactError(
                f"unexpected size output for {guest_path!r}: {stdout.strip()!r}"
            ) from exc

    @staticmethod
    def _delete_guest_scratch(transport: Any, guest_path: str) -> None:
        try:
            transport.delete_file(guest_path)
        except Exception as exc:  # noqa: BLE001 - scratch cleanup must not fail the export
            log.warning("could not remove sandbox scratch file %s: %s", guest_path, exc)

    @staticmethod
    def _write_atomic(path: Path, data: bytes) -> None:
        tmp = path.with_name(path.name + ".tmp")
        try:
            tmp.write_bytes(data)
            os.replace(tmp, path)
        except OSError as exc:
            raise SandboxArtifactError(f"could not write {path}: {exc}") from exc
        finally:
            try:
                tmp.unlink()
            except FileNotFoundError:
                pass

    # ---------------- import ----------------
    def import_artifact(
        self,
        transport: Any,
        config: AgentSandboxConfig,
        artifact_id: str,
        *,
        timeout: Optional[int] = None,
    ) -> ArtifactRef:
        """Restore a stored artifact's tree into the sandbox's /workspace."""
        ref = self.load_meta(artifact_id)
        data_path = self.data_path(artifact_id)
        try:
            data = data_path.read_bytes()
        except OSError as exc:
            raise SandboxArtifactError(
                f"artifact {artifact_id!r} tarball is unreadable: {exc}"
            ) from exc
        if len(data) > config.artifact_max_bytes:
            raise SandboxArtifactTooLargeError(
                f"artifact {artifact_id!r} is {len(data)} bytes, over the "
                f"{config.artifact_max_bytes} byte cap"
            )
        digest = hashlib.sha256(data).hexdigest()
        if ref.sha256 and digest != ref.sha256:
            raise SandboxArtifactError(
                f"artifact {artifact_id!r} is corrupt: sha256 {digest} != recorded {ref.sha256}"
            )
        with tarfile.open(fileobj=_BytesReader(data), mode="r:gz") as archive:
            # tarfile builds the index (and validates the gzip stream) locally
            # before anything reaches the guest: a truncated or non-tar
            # artifact fails here, not halfway through an extraction.
            names = archive.getnames()
        self._validate_members(names)

        scratch_abs = files_mod.scratch_file_path("artifacts", f"{artifact_id}-restore.tar.gz")
        files_mod.ensure_dir(transport, posixpath.dirname(scratch_abs))
        transport.put_file(scratch_abs, data, timeout=transport_timeout(timeout))
        try:
            stdout, stderr, code = transport.run(
                self._tar_extract_command(scratch_abs), transport_timeout(timeout)
            )
            if code == TAR_EXIT_WARNING and stderr.strip():
                log.warning("tar restore of %s reported: %s", artifact_id, stderr.strip())
            elif code != 0:
                raise SandboxArtifactError(
                    f"tar restore of artifact {artifact_id!r} failed (exit {code}): "
                    f"{(stderr or stdout).strip() or 'no stderr'}"
                )
        finally:
            self._delete_guest_scratch(transport, scratch_abs)
        log.info("imported artifact %s (%d bytes, %d member(s))", artifact_id, len(data), len(names))
        return ref

    @staticmethod
    def _validate_members(names: Sequence[str]) -> None:
        """Refuse an archive whose members would land outside /workspace.

        Both checks are lexical and deliberately strict (they run BEFORE
        extraction): an absolute member (tar strips the leading '/' but we do not
        rely on that) or any ``..`` component is refused outright. GNU tar's own
        member-name refusal is a second line, not this one.
        """
        for name in names:
            if not name or name.startswith("/"):
                raise SandboxArtifactError(
                    f"artifact member {name!r} is absolute; refusing to restore"
                )
            parts = name.split("/")
            if any(part == ".." for part in parts):
                raise SandboxArtifactError(
                    f"artifact member {name!r} contains '..'; refusing to restore"
                )
            normalized = posixpath.normpath(posixpath.join(WORKSPACE, name))
            if normalized != WORKSPACE and not normalized.startswith(WORKSPACE + "/"):
                raise SandboxArtifactError(
                    f"artifact member {name!r} escapes the sandbox root; refusing to restore"
                )

    @staticmethod
    def _tar_extract_command(scratch_abs: str) -> str:
        """Extract with the same ``-C /workspace`` confinement tar created with.

        ``--no-same-owner``/``--no-same-permissions`` keep the archive from
        dictating ownership or modes beyond umask: the guest runs as a single
        unprivileged uid, and an artifact must not be able to hand itself
        setuid-shaped modes.
        """
        return (
            f"tar -xzf {shlex.quote(scratch_abs)} -C {shlex.quote(WORKSPACE)} "
            "--no-same-owner --no-same-permissions"
        )


class _BytesReader:
    """Minimal seekable file-like wrapper so tarfile can index an in-memory blob.

    ``tarfile.open(fileobj=...)`` needs seek/tell to build the member index, and
    ``io.BytesIO`` would copy the whole tarball; this wrapper exposes the bytes
    we already hold without a second allocation.
    """

    def __init__(self, data: bytes):
        self._data = data
        self._pos = 0

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            chunk = self._data[self._pos :]
            self._pos = len(self._data)
            return chunk
        chunk = self._data[self._pos : self._pos + size]
        self._pos += len(chunk)
        return chunk

    def seek(self, offset: int, whence: int = 0) -> int:
        if whence == 0:
            self._pos = offset
        elif whence == 1:
            self._pos += offset
        elif whence == 2:
            self._pos = len(self._data) + offset
        else:  # pragma: no cover - tarfile only uses 0/1/2
            raise ValueError(f"unsupported whence {whence}")
        return self._pos

    def tell(self) -> int:
        return self._pos

    def seekable(self) -> bool:
        return True


def transport_timeout(timeout: Optional[int]) -> Optional[int]:
    """Normalize an optional caller timeout into the transport's kwargs shape."""
    return int(timeout) if timeout else None


# ---------------- checkpoints (Unit 2.6) ----------------
# The plugin's own scratch namespace, never part of a checkpoint: it holds
# in-flight stdin payloads and artifact tarballs, not session work.
CHECKPOINT_EXCLUDES = (f"{WORKSPACE}/{files_mod.SCRATCH_DIRNAME}",)


def checkpoint(
    store: ArtifactStore,
    transport: Any,
    config: AgentSandboxConfig,
    *,
    session: str = "",
    ttl_hours: Optional[float] = None,
    max_bytes: Optional[int] = None,
    timeout: Optional[int] = None,
) -> ArtifactRef:
    """Export the whole workspace as a checkpoint artifact.

    The workspace is disposable by design (emptyDir, destroyed with the claim),
    so a checkpoint is the only way a session can carry work across a recycle or
    a claim expiry. ``ttl_hours`` defaults to the configured artifact TTL.
    """
    return store.export(
        transport,
        config,
        [WORKSPACE],
        exclude=CHECKPOINT_EXCLUDES,
        ttl_hours=ttl_hours,
        max_bytes=max_bytes,
        session=session,
        kind="checkpoint",
        timeout=timeout,
    )
