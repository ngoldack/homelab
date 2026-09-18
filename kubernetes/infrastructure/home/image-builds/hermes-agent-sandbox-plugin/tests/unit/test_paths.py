"""Path contract: lexical confinement, guest resolution, kind checks (Unit 2.1)."""

from __future__ import annotations

import pytest

from hermes_agent_sandbox import files
from hermes_agent_sandbox.errors import (
    SandboxCommandError,
    SandboxPathError,
    SandboxUnsupportedError,
)

from _guest import Guest, GuestTransport

# ---------------- lexical layer (pure) ----------------


@pytest.mark.parametrize(
    "requested, expected",
    [
        ("/workspace", "/workspace"),
        ("/workspace/", "/workspace"),
        ("/workspace/src/main.py", "/workspace/src/main.py"),
        ("src/main.py", "/workspace/src/main.py"),
        ("./src/../src/a.txt", "/workspace/src/a.txt"),
        ("/workspace/./a/./b", "/workspace/a/b"),
        ("a/b/../c", "/workspace/a/c"),
        ("   /workspace/x   ", "/workspace/x"),
    ],
)
def test_normalize_accepts_and_canonicalizes(requested, expected):
    assert files.normalize_workspace_path(requested) == expected


@pytest.mark.parametrize(
    "requested",
    [
        "",
        "   ",
        "/workspace/../etc/passwd",
        "../../etc/passwd",
        "src/../../outside",
        "/etc/passwd",
        "/workspace-evil/x",
        "/workspacex",
        "~/.bashrc",
        "/workspace/bad\x00name",
    ],
)
def test_normalize_rejects(requested):
    with pytest.raises(SandboxPathError):
        files.normalize_workspace_path(requested)


def test_normalize_is_prefix_boundary_aware():
    # `/workspace-evil` must NOT be treated as a child of /workspace.
    with pytest.raises(SandboxPathError):
        files.normalize_workspace_path("/workspace-evil")


def test_scratch_paths_are_reserved():
    files.require_not_scratch("/workspace/src/a.txt")  # allowed
    for path in ("/workspace/.hermes", "/workspace/.hermes/x", "/workspace/.hermes/artifacts/a.tar.gz"):
        with pytest.raises(SandboxPathError):
            files.require_not_scratch(path)
    assert files.is_scratch_path("/workspace/.hermes/stdin/x")
    assert not files.is_scratch_path("/workspace/.hermesx")


# ---------------- guest layer ----------------


def test_probe_reports_types_and_size():
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"hello")
    guest.add_dir("/workspace/sub")
    transport = GuestTransport(guest)

    probed = files.resolve_workspace_path(transport, "a.txt", must_exist=True, kind="file")
    assert probed.resolved == "/workspace/a.txt"
    assert probed.is_file and not probed.is_dir and probed.size == 5

    probed_dir = files.resolve_workspace_path(transport, "/workspace/sub", must_exist=True, kind="dir")
    assert probed_dir.is_dir and not probed_dir.is_file


def test_probe_rejects_symlink_escape():
    """A link pointing outside /workspace must fail before any request is made."""
    guest = Guest()
    guest.add_file("/tmp/secret", b"top secret")
    guest.add_symlink("/workspace/link", "/tmp/secret")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxPathError) as excinfo:
        files.resolve_workspace_path(transport, "/workspace/link", must_exist=True, kind="file")
    assert "outside the sandbox root" in str(excinfo.value)


def test_probe_allows_symlink_inside_root():
    guest = Guest()
    guest.add_file("/workspace/real.txt", b"data")
    guest.add_symlink("/workspace/alias", "/workspace/real.txt")
    transport = GuestTransport(guest)

    probed = files.resolve_workspace_path(transport, "/workspace/alias", must_exist=True, kind="file")
    assert probed.resolved == "/workspace/real.txt"
    assert probed.is_symlink


def test_probe_rejects_symlinked_parent_escape():
    """The *parent* is what a write resolves through: a link out must fail."""
    guest = Guest()
    guest.add_dir("/tmp/elsewhere")
    guest.add_symlink("/workspace/out", "/tmp/elsewhere")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxPathError):
        files.resolve_workspace_path(transport, "/workspace/out/new.txt")


def test_probe_rejects_directory_for_file_read():
    guest = Guest()
    guest.add_dir("/workspace/dir")
    transport = GuestTransport(guest)
    with pytest.raises(SandboxPathError) as excinfo:
        files.resolve_workspace_path(transport, "/workspace/dir", must_exist=True, kind="file")
    assert "not a regular file" in str(excinfo.value)


def test_probe_reports_missing_path():
    transport = GuestTransport(Guest())
    probed = files.resolve_workspace_path(transport, "/workspace/nope")
    assert not probed.exists
    with pytest.raises(SandboxPathError):
        files.resolve_workspace_path(transport, "/workspace/nope", must_exist=True)


def test_probe_without_realpath_is_unsupported_not_silent():
    """A guest that cannot resolve symlinks must fail loudly, never degrade."""

    class NoRealpath(GuestTransport):
        def run(self, command: str, timeout: int):
            return "", "sh: 1: realpath: not found", 3

    with pytest.raises(SandboxUnsupportedError):
        files.resolve_workspace_path(NoRealpath(), "/workspace/a.txt")


def test_probe_transport_failure_is_command_error():
    class Broken(GuestTransport):
        def run(self, command: str, timeout: int):
            raise RuntimeError("connection reset")

    with pytest.raises(SandboxCommandError):
        files.resolve_workspace_path(Broken(), "/workspace/a.txt")


def test_relative_members():
    assert files.relative_members(["/workspace"]) == ["."]
    assert files.relative_members(["/workspace/src", "/workspace/a.txt"]) == ["src", "a.txt"]


def test_scratch_file_path_rejects_junk_names():
    assert files.scratch_file_path("stdin", "abc123") == "/workspace/.hermes/stdin/abc123"
    with pytest.raises(SandboxPathError):
        files.scratch_file_path("stdin", "../../evil")
