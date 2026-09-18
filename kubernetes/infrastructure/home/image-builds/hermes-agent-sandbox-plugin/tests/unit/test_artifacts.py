"""Artifacts + checkpoints: caps, TTL, round-trip, member validation (2.3/2.6)."""

from __future__ import annotations

import io
import json
import tarfile
import time
from pathlib import Path

import pytest

from hermes_agent_sandbox import artifacts as artifacts_mod
from hermes_agent_sandbox import files
from hermes_agent_sandbox.artifacts import ArtifactStore
from hermes_agent_sandbox.errors import (
    SandboxArtifactError,
    SandboxArtifactExpiredError,
    SandboxArtifactTooLargeError,
    SandboxPathError,
)

from _guest import Guest, GuestTransport, make_config, make_env


def _store(tmp_path, **overrides):
    config = make_config(tmp_path, **overrides)
    return ArtifactStore(config.artifact_root), config


# ---------------- export ----------------


def test_export_pulls_tar_and_records_metadata(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/src/main.py", b"print(1)\n")
    guest.add_file("/workspace/README.md", b"# hi\n")
    transport = GuestTransport(guest)
    store, config = _store(tmp_path)

    ref = store.export(transport, config, ["/workspace"], session="s1")

    assert ref.bytes > 0 and ref.kind == "artifact"
    assert ref.paths == ("/workspace",)
    assert store.data_path(ref.id).exists()
    meta = json.loads(store.meta_path(ref.id).read_text())
    assert meta["id"] == ref.id and meta["sha256"] == ref.sha256
    # The tarball is a real gzip tar containing the session's files.
    with tarfile.open(store.data_path(ref.id)) as archive:
        assert sorted(archive.getnames()) == ["./README.md", "./src", "./src/main.py"]


def test_export_excludes_plugin_scratch(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/keep.txt", b"keep")
    guest.add_file("/workspace/.hermes/artifacts/intermediate.tar.gz", b"junk")
    guest.add_file("/workspace/.hermes/stdin/abc", b"payload")
    transport = GuestTransport(guest)
    store, config = _store(tmp_path)

    ref = store.export(transport, config, ["/workspace"], exclude=(files.SCRATCH_DIR,))

    with tarfile.open(store.data_path(ref.id)) as archive:
        names = archive.getnames()
    assert "./keep.txt" in names
    assert not any(".hermes" in name for name in names)


def test_export_refuses_scratch_path_request(tmp_path):
    guest = Guest()
    store, config = _store(tmp_path)
    with pytest.raises(SandboxPathError):
        store.export(
            GuestTransport(guest), config, ["/workspace/.hermes/stdin/x"]
        )


def test_export_refuses_missing_path(tmp_path):
    store, config = _store(tmp_path)
    with pytest.raises(SandboxPathError):
        store.export(GuestTransport(Guest()), config, ["/workspace/ghost"])


def test_export_enforces_byte_cap_and_cleans_scratch(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/big.bin", b"x" * 4096)
    transport = GuestTransport(guest)
    store, config = _store(tmp_path, artifact_max_bytes=64)

    with pytest.raises(SandboxArtifactTooLargeError) as excinfo:
        store.export(transport, config, ["/workspace/big.bin"])
    assert "64 byte cap" in str(excinfo.value)
    # The guest scratch tarball was removed again, and nothing was stored.
    assert [p for p in guest.files if p.startswith("/workspace/.hermes/artifacts/")] == []
    assert list(store.root.glob("*.tar.gz")) == []


def test_export_ttl_is_bounded_by_config(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"a")
    transport = GuestTransport(guest)
    store, config = _store(tmp_path, artifact_default_ttl_hours=1, artifact_max_ttl_hours=2)

    with pytest.raises(SandboxArtifactError) as excinfo:
        store.export(transport, config, ["/workspace/a.txt"], ttl_hours=9)
    assert "2h cap" in str(excinfo.value)

    ref = store.export(transport, config, ["/workspace/a.txt"], ttl_hours=1)
    assert 3500 < ref.ttl_seconds <= 3600


def test_export_default_ttl_comes_from_config(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"a")
    store, config = _store(tmp_path, artifact_default_ttl_hours=3)
    ref = store.export(GuestTransport(guest), config, ["/workspace/a.txt"])
    assert 3 * 3600 - 5 <= ref.ttl_seconds <= 3 * 3600


def test_expired_artifacts_are_not_loadable_and_are_swept(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"a")
    store, config = _store(tmp_path)
    ref = store.export(GuestTransport(guest), config, ["/workspace/a.txt"], ttl_hours=1)

    # Move the recorded clocks past the TTL instead of sleeping an hour.
    payload = json.loads(store.meta_path(ref.id).read_text())
    payload["expires_at"] = time.time() - 60
    store.meta_path(ref.id).write_text(json.dumps(payload))

    with pytest.raises(SandboxArtifactExpiredError):
        store.load_meta(ref.id)
    assert store.list_artifacts() == []
    removed = store.sweep()
    assert ref.id in removed
    assert not store.data_path(ref.id).exists()


def test_sweep_removes_orphan_tarball(tmp_path):
    store, config = _store(tmp_path)
    store._ensure_root()
    orphan = store.root / f"{'a' * 32}.tar.gz"
    orphan.write_bytes(b"orphan")
    assert store.sweep() == ["a" * 32]
    assert not orphan.exists()


def test_invalid_artifact_ids_are_refused(tmp_path):
    store, config = _store(tmp_path)
    for bad in ("../../etc/passwd", "/etc/passwd", "short", "A" * 32, ""):
        with pytest.raises(SandboxArtifactError):
            store.data_path(bad)


# ---------------- import ----------------


def test_export_import_round_trip(tmp_path):
    producer_guest = Guest()
    producer_guest.add_file("/workspace/src/main.py", b"print(1)\n")
    producer_guest.add_dir("/workspace/empty-dir")
    store, config = _store(tmp_path)
    ref = store.export(GuestTransport(producer_guest), config, ["/workspace"])

    consumer_guest = Guest()
    restored = store.import_artifact(GuestTransport(consumer_guest), config, ref.id)

    assert restored.id == ref.id
    assert consumer_guest.files["/workspace/src/main.py"] == b"print(1)\n"
    # The guest-side copy of the tarball is gone; the tree is what remains.
    assert [p for p in consumer_guest.files if p.startswith("/workspace/.hermes")] == []
    assert any(path.startswith("/workspace/.hermes/artifacts/") for path in consumer_guest.deletes)


def test_import_rejects_unknown_id(tmp_path):
    store, config = _store(tmp_path)
    with pytest.raises(SandboxArtifactExpiredError):
        store.import_artifact(GuestTransport(Guest()), config, "b" * 32)


def test_import_detects_corruption(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"a")
    store, config = _store(tmp_path)
    ref = store.export(GuestTransport(guest), config, ["/workspace/a.txt"])

    store.data_path(ref.id).write_bytes(b"not the tarball")
    with pytest.raises(SandboxArtifactError) as excinfo:
        store.import_artifact(GuestTransport(Guest()), config, ref.id)
    assert "corrupt" in str(excinfo.value)


@pytest.mark.parametrize(
    "member",
    [
        "../escape.txt",
        "/etc/passwd",
        "src/../../escape.txt",
        "",
    ],
)
def test_import_refuses_hostile_members(tmp_path, member):
    store, config = _store(tmp_path)
    ref_id = "c" * 32
    store._ensure_root()
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        info = tarfile.TarInfo(name=member)
        info.size = 1
        archive.addfile(info, io.BytesIO(b"x"))
    store.data_path(ref_id).write_bytes(buffer.getvalue())
    store.meta_path(ref_id).write_text(
        json.dumps(
            {
                "id": ref_id,
                "bytes": len(buffer.getvalue()),
                "created_at": time.time(),
                "expires_at": time.time() + 60,
                "sha256": "",
                "paths": ["/workspace"],
            }
        )
    )
    guest = Guest()
    with pytest.raises(SandboxArtifactError):
        store.import_artifact(GuestTransport(guest), config, ref_id)
    # Nothing was extracted and no guest-side copy was ever written.
    assert guest.files == {}
    assert guest.puts == []


# ---------------- checkpoints (environment surface) ----------------


def test_checkpoint_and_restore_into_fresh_claim(tmp_path):
    env, transport, guest = make_env(tmp_path, artifact_root=str(tmp_path / "artifacts"))
    env.write_file("/workspace/notes.txt", "keep me")

    ref = env.checkpoint()

    assert ref["kind"] == "checkpoint"
    assert ref["paths"] == ["/workspace"]
    assert ref["bytes"] > 0

    # The workspace is disposable: destroy it and restore into a NEW claim.
    before = list(transport.attached)
    guest.files.clear()
    restored = env.restore_checkpoint(ref["id"], fresh=True)

    assert restored["id"] == ref["id"]
    assert env.read_file("/workspace/notes.txt") == "keep me"
    assert len(transport.attached) == len(before) + 1  # a fresh claim was adopted
    assert len(env._claims.deleted) >= 1


def test_restore_without_fresh_reuses_current_claim(tmp_path):
    env, transport, guest = make_env(tmp_path)
    env.write_file("/workspace/a.txt", "one")
    ref = env.checkpoint()
    before = len(transport.attached)

    env.restore_checkpoint(ref["id"], fresh=False)

    assert len(transport.attached) == before


def test_checkpoint_excludes_scratch_and_internal_state(tmp_path):
    env, transport, guest = make_env(tmp_path)
    env.write_file("/workspace/keep.txt", "keep")
    # A stray scratch file must never enter the checkpoint.
    guest.put("/workspace/.hermes/artifacts/leftover.tar.gz", b"internal")

    ref = env.checkpoint()

    with tarfile.open(env.artifacts.data_path(ref["id"])) as archive:
        names = archive.getnames()
    assert "./keep.txt" in names
    assert not any(".hermes" in name for name in names)


def test_artifact_listing_reports_stored_artifacts(tmp_path):
    env, transport, guest = make_env(tmp_path)
    env.write_file("/workspace/a.txt", "a")
    ref = env.checkpoint()
    listed = env.list_artifacts()
    assert [entry["id"] for entry in listed] == [ref["id"]]
    assert listed[0]["kind"] == "checkpoint"
