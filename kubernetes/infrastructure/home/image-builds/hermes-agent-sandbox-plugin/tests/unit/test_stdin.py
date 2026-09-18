"""stdin delivery: native frames, chunking, and the workspace-file fallback."""

from __future__ import annotations

import threading

import pytest

from hermes_agent_sandbox import files
from hermes_agent_sandbox.errors import (
    SandboxCommandError,
    SandboxFileSizeError,
    SandboxUnsupportedError,
)
from hermes_agent_sandbox.transport import (
    SdkCommandConnector,
    drive_start_with_stdin,
)

from _guest import Guest, GuestTransport, make_env

# Real generated protobuf (k8s-agent-sandbox 1.0.2 ships it and the plugin
# declares grpcio/protobuf as hard dependencies), so the frames under test are
# the ones the RPC actually expects rather than a hand-rolled lookalike.
from k8s_agent_sandbox.commands._process_stubs import process_pb2
from google.protobuf import empty_pb2


class FakeStub:
    """A ProcessService stub whose ``cat`` echoes whatever WriteStdin received."""

    def __init__(self, exit_code: int = 0, write_failure: bool = False):
        self.process_id = 1001
        self.frames = []
        self.write_failure = write_failure
        self._eof = threading.Event()
        self.start_requests = []
        self.write_calls = 0

    def Start(self, request, timeout=None):
        self.start_requests.append(request)
        return self._stream()

    def _stream(self):
        yield process_pb2.StartResponse(
            init=process_pb2.InitEvent(process_id=self.process_id)
        )
        if self.write_failure:
            # The child is gone before it reads stdin: the real server answers
            # NOT_FOUND/broken-pipe to WriteStdin and still streams the exit.
            yield process_pb2.StartResponse(exit=process_pb2.ExitEvent(exit_code=0))
            return
        assert self._eof.wait(5), "stdin writer never sent EOF"
        payload = b"".join(chunk for _, chunk in self.frames if chunk)
        if payload:
            yield process_pb2.StartResponse(stdout=payload)
        yield process_pb2.StartResponse(
            exit=process_pb2.ExitEvent(exit_code=0)
        )

    def WriteStdin(self, request, timeout=None):
        self.write_calls += 1
        if self.write_failure:
            raise RuntimeError("broken pipe")
        if request.HasField("eof"):
            self.frames.append((request.process_id, None))
            self._eof.set()
        else:
            self.frames.append((request.process_id, request.input))
        return process_pb2.WriteStdinResponse()


# ---------------- native protocol driver ----------------


def test_cat_round_trip_through_native_frames():
    """The plan's proof for Unit 2.2: `cat` gets the exact bytes back."""
    stub = FakeStub()
    payload = b"hello stdin\n" * 3

    stdout, stderr, code = drive_start_with_stdin(
        stub, process_pb2, empty_pb2, "cat", payload, 30
    )

    assert (stdout, stderr, code) == (payload.decode(), "", 0)
    # Every frame addressed the process the InitEvent announced.
    assert {pid for pid, _ in stub.frames} == {stub.process_id}
    # Exactly one EOF frame, and it is last: without it `cat` never returns.
    assert stub.frames[-1][1] is None
    assert sum(1 for _, chunk in stub.frames if chunk is None) == 1
    # The command went over `/bin/sh -c`, like every other exec.
    assert stub.start_requests[0].config.command[:2] == ["/bin/sh", "-c"]


def test_payload_is_chunked_and_reassembled():
    stub = FakeStub()
    payload = bytes(range(10))

    stdout, _, code = drive_start_with_stdin(
        stub, process_pb2, empty_pb2, "cat", payload, 30, chunk_bytes=4
    )

    chunks = [chunk for _, chunk in stub.frames if chunk is not None]
    assert [len(chunk) for chunk in chunks] == [4, 4, 2]
    assert b"".join(chunks) == payload
    assert stdout.encode("latin-1") == payload
    assert code == 0


def test_empty_payload_still_sends_eof():
    stub = FakeStub()
    stdout, _, code = drive_start_with_stdin(
        stub, process_pb2, empty_pb2, "cat", b"", 30
    )
    assert stub.frames == [(stub.process_id, None)]
    assert (stdout, code) == ("", 0)


def test_write_failure_does_not_fail_a_command_that_exited():
    """A child that exits before reading stdin is a race, not an error."""
    stub = FakeStub(write_failure=True)
    stdout, _, code = drive_start_with_stdin(
        stub, process_pb2, empty_pb2, "true", b"ignored", 30
    )
    assert code == 0


def test_stream_without_exit_event_is_an_error():
    class NoExit(FakeStub):
        def _stream(self):
            yield process_pb2.StartResponse(
                init=process_pb2.InitEvent(process_id=self.process_id)
            )
            return

    with pytest.raises(SandboxCommandError) as excinfo:
        drive_start_with_stdin(NoExit(), process_pb2, empty_pb2, "cat", b"x", 5)
    assert "without an exit event" in str(excinfo.value)


def test_connector_run_with_stdin_uses_the_attached_stub():
    """The connector's real code path (lazy imports included) drives the stub."""
    connector = SdkCommandConnector.__new__(SdkCommandConnector)  # no channel
    connector._stub = FakeStub()
    connector._sandbox_name = "sbx-1"
    connector._pod_ip = "10.0.0.5"
    connector.config = None

    stdout, stderr, code = connector.run_with_stdin("cat", b"payload", 30)
    assert (stdout, code) == ("payload", 0)


# ---------------- environment surface ----------------


def test_execute_native_stdin_round_trip(tmp_path):
    env, transport, guest = make_env(tmp_path)

    result = env.execute("cat", stdin_data="hello\n")

    assert result["returncode"] == 0
    assert result["output"] == "hello\n"
    assert guest.stdin_payloads == [b"hello\n"]


def test_execute_bytes_payload(tmp_path):
    env, transport, guest = make_env(tmp_path)
    result = env.execute("cat", stdin_data=b"\xff\xfe")
    assert result["returncode"] == 0
    assert guest.stdin_payloads == [b"\xff\xfe"]


def test_execute_stdin_falls_back_to_workspace_file(tmp_path):
    """UNIMPLEMENTED native stdin must transparently use the temp-file path."""
    env, transport, guest = make_env(tmp_path, stdin_native=False)

    result = env.execute("cat", stdin_data="fallback payload")

    assert result["returncode"] == 0
    assert result["output"] == "fallback payload"
    # Delivered via the workspace scratch file, and removed afterwards.
    assert guest.stdin_payloads == [b"fallback payload"]
    scratch_files = [path for path in guest.files if path.startswith("/workspace/.hermes/stdin/")]
    assert scratch_files == []
    assert [path for path in guest.deletes if path.startswith("/workspace/.hermes/stdin/")]


def test_execute_stdin_file_mode_skips_native(tmp_path):
    env, transport, guest = make_env(tmp_path, stdin_mode="file")

    result = env.execute("cat", stdin_data="file mode")

    assert result["output"] == "file mode"
    assert guest.stdin_payloads == [b"file mode"]


def test_execute_stdin_native_mode_propagates_unsupported(tmp_path):
    """`native` pins the transport: no silent downgrade when it is missing."""
    env, transport, guest = make_env(tmp_path, stdin_mode="native", stdin_native=False)

    with pytest.raises(SandboxUnsupportedError):
        env.execute("cat", stdin_data="x")
    assert guest.puts == []  # the fallback never ran


def test_execute_stdin_over_cap_rejected(tmp_path):
    env, transport, guest = make_env(tmp_path, stdin_max_bytes=4)

    with pytest.raises(SandboxFileSizeError):
        env.execute("cat", stdin_data="12345")
    assert guest.stdin_payloads == []


def test_execute_stdin_rejects_unsupported_type(tmp_path):
    env, transport, guest = make_env(tmp_path)
    with pytest.raises(SandboxCommandError):
        env.execute("cat", stdin_data=object())


def test_execute_without_stdin_still_uses_plain_exec(tmp_path):
    """The capability layer must not move the default command path."""
    env, transport, guest = make_env(tmp_path)
    guest.add_file("/workspace/marker", b"ok")

    result = env.execute("cat /workspace/marker")

    assert result["returncode"] == 0
    assert result["output"] == "ok"
    assert guest.stdin_payloads == []
