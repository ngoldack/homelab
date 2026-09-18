"""File operations over the Router REST filesystem (Unit 2.1)."""

from __future__ import annotations

import pytest

from hermes_agent_sandbox import files
from hermes_agent_sandbox.errors import (
    SandboxFileSizeError,
    SandboxPathError,
)

from _guest import Guest, GuestTransport, make_config, make_env


def _cfg(tmp_path, **overrides):
    return make_config(tmp_path, **overrides)


# ---------------- write ----------------


def test_write_creates_file_through_rest(tmp_path):
    guest = Guest()
    guest.add_dir("/workspace/src")
    transport = GuestTransport(guest)
    config = _cfg(tmp_path)

    result = files.write_file(transport, config, "src/main.py", "print('hi')\n")

    assert result["bytes"] == len("print('hi')\n")
    assert result["created"] is True
    assert guest.files["/workspace/src/main.py"] == b"print('hi')\n"
    # One PUT, with the workspace-confined path.
    assert guest.puts == [("/workspace/src/main.py", b"print('hi')\n")]


def test_write_missing_parent_is_explicit_error(tmp_path):
    guest = Guest()
    transport = GuestTransport(guest)
    config = _cfg(tmp_path)

    with pytest.raises(SandboxPathError) as excinfo:
        files.write_file(transport, config, "/workspace/src/main.py", "x")
    assert "does not exist" in str(excinfo.value)
    assert guest.puts == []  # nothing was attempted


def test_write_create_parents_true_creates_them(tmp_path):
    guest = Guest()
    transport = GuestTransport(guest)
    config = _cfg(tmp_path)

    files.write_file(
        transport, config, "/workspace/a/b/c.txt", "x", create_parents=True
    )
    assert guest.files["/workspace/a/b/c.txt"] == b"x"
    assert "/workspace/a/b" in guest.mkdir_calls


def test_write_over_directory_rejected(tmp_path):
    guest = Guest()
    guest.add_dir("/workspace/dir")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxPathError) as excinfo:
        files.write_file(transport, _cfg(tmp_path), "/workspace/dir", "x")
    assert "is a directory" in str(excinfo.value)
    assert guest.puts == []


def test_write_over_size_cap_rejected_before_transport(tmp_path):
    guest = Guest()
    transport = GuestTransport(guest)
    config = _cfg(tmp_path, file_write_max_bytes=16)

    with pytest.raises(SandboxFileSizeError) as excinfo:
        files.write_file(transport, config, "/workspace/big", b"x" * 17)
    assert "17 bytes" in str(excinfo.value) and "16 byte write limit" in str(excinfo.value)
    assert guest.puts == [] and guest.commands == []


def test_write_rejects_scratch_namespace(tmp_path):
    transport = GuestTransport(Guest())
    with pytest.raises(SandboxPathError):
        files.write_file(
            transport, _cfg(tmp_path), "/workspace/.hermes/x", "x", create_parents=True
        )


def test_write_through_in_root_symlink_writes_target(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/real.txt", b"old")
    guest.add_symlink("/workspace/alias", "/workspace/real.txt")
    transport = GuestTransport(guest)

    files.write_file(transport, _cfg(tmp_path), "/workspace/alias", "new")
    assert guest.files["/workspace/real.txt"] == b"new"


def test_write_rejects_non_str_bytes_content(tmp_path):
    transport = GuestTransport(Guest())
    with pytest.raises(SandboxPathError):
        files.write_file(transport, _cfg(tmp_path), "/workspace/a", ["not bytes"])


# ---------------- read ----------------


def test_read_returns_text_and_size(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/notes.md", b"# hello\n")
    transport = GuestTransport(guest)

    result = files.read_file(transport, _cfg(tmp_path), "/workspace/notes.md")
    assert result["content"] == "# hello\n"
    assert result["bytes"] == 8


def test_read_missing_file_is_endpoint_error(tmp_path):
    transport = GuestTransport(Guest())
    with pytest.raises(SandboxPathError):
        files.read_file(transport, _cfg(tmp_path), "/workspace/nope.txt")


def test_read_directory_rejected_before_download(tmp_path):
    guest = Guest()
    guest.add_dir("/workspace/sub")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxPathError):
        files.read_file(transport, _cfg(tmp_path), "/workspace/sub")
    # Only the probe ran; no REST GET was attempted.
    assert len(guest.commands) == 1


def test_read_over_limit_rejected_from_probe_size(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/big.txt", b"x" * 100)
    transport = GuestTransport(guest)

    with pytest.raises(SandboxFileSizeError) as excinfo:
        files.read_file(transport, _cfg(tmp_path), "/workspace/big.txt", max_bytes=10)
    assert "100 bytes" in str(excinfo.value)


def test_read_over_configured_cap_rejected(tmp_path):
    """The configured cap (not a hardcoded number) is what earns the refusal."""
    guest = Guest()
    guest.add_file("/workspace/small.txt", b"12345")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxFileSizeError):
        files.read_file(transport, _cfg(tmp_path, file_read_max_bytes=4), "/workspace/small.txt")

    result = files.read_file(transport, _cfg(tmp_path, file_read_max_bytes=5), "/workspace/small.txt")
    assert result["content"] == "12345"


def test_read_binary_decodes_with_replacement(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/bin", b"\xff\xfe")
    transport = GuestTransport(guest)
    result = files.read_file(transport, _cfg(tmp_path), "/workspace/bin")
    assert result["content"] == "\ufffd\ufffd"


def test_read_symlink_escape_refused(tmp_path):
    guest = Guest()
    guest.add_file("/tmp/secret", b"top")
    guest.add_symlink("/workspace/leak", "/tmp/secret")
    transport = GuestTransport(guest)

    with pytest.raises(SandboxPathError):
        files.read_file(transport, _cfg(tmp_path), "/workspace/leak")


# ---------------- list ----------------


def test_list_dir_returns_entries(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"1")
    guest.add_file("/workspace/sub/b.txt", b"22")
    guest.add_dir("/workspace/sub")
    transport = GuestTransport(guest)

    result = files.list_dir(transport, _cfg(tmp_path), "/workspace")
    names = sorted(entry["name"] for entry in result["entries"])
    assert names == ["a.txt", "sub"]
    assert result["count"] == 2
    assert result["truncated"] is False


def test_list_dir_truncates_to_max_entries(tmp_path):
    guest = Guest()
    for index in range(5):
        guest.add_file(f"/workspace/f{index}.txt", b"x")
    transport = GuestTransport(guest)

    result = files.list_dir(transport, _cfg(tmp_path), "/workspace", max_entries=2)
    assert result["count"] == 5
    assert len(result["entries"]) == 2
    assert result["truncated"] is True


def test_list_dir_on_file_rejected(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/a.txt", b"1")
    transport = GuestTransport(guest)
    with pytest.raises(SandboxPathError):
        files.list_dir(transport, _cfg(tmp_path), "/workspace/a.txt")


def test_list_dir_unexpected_payload_is_command_error(tmp_path):
    class Weird(GuestTransport):
        def fetch_file(self, remote_path, timeout=None):
            return b"not json"

    from hermes_agent_sandbox.errors import SandboxCommandError

    guest = Guest()
    guest.add_dir("/workspace/sub")
    transport = Weird(guest)
    with pytest.raises(SandboxCommandError):
        files.list_dir(transport, _cfg(tmp_path), "/workspace/sub")


def test_delete_path_removes_file(tmp_path):
    guest = Guest()
    guest.add_file("/workspace/gone.txt", b"x")
    transport = GuestTransport(guest)

    files.delete_path(transport, "/workspace/gone.txt")
    assert "/workspace/gone.txt" not in guest.files


# ---------------- environment surface ----------------


def test_environment_file_methods_end_to_end(tmp_path):
    env, transport, guest = make_env(tmp_path)

    written = env.write_file("/workspace/hello.txt", "hi there")
    assert written["bytes"] == 8
    assert env.read_file("/workspace/hello.txt") == "hi there"
    assert [entry["name"] for entry in env.list_dir("/workspace")] == ["hello.txt"]


def test_environment_write_applies_configured_cap(tmp_path):
    env, transport, guest = make_env(tmp_path, file_write_max_bytes=4)
    with pytest.raises(SandboxFileSizeError):
        env.write_file("/workspace/x", "12345")
