"""Background processes: native stream, bounded logs, TERM/KILL stop (2.5)."""

from __future__ import annotations

import time

import pytest

from hermes_agent_sandbox.errors import SandboxProcessError

from _guest import Guest, GuestTransport, make_env


def _scripted(transport: GuestTransport, command: str, events):
    transport.process_scripts[command] = list(events)
    return transport


def test_start_process_returns_handle_and_logs_output(tmp_path):
    env, transport, guest = make_env(tmp_path)
    command = "cd /workspace && printf hello"
    _scripted(transport, command, [("stdout", b"hello"), ("stderr", b"warn\n"), ("exit", 0)])

    handle = env.start_process("printf hello")

    assert handle["id"] and handle["remote_process_id"]
    assert handle["state"] in ("running", "exited")
    _wait_for(lambda: not env.process_logs(handle["id"])["running"])
    logs = env.process_logs(handle["id"])
    assert logs["output"] == "hellowarn\n"
    assert logs["stdout"] == "hello" and logs["stderr"] == "warn\n"
    assert logs["returncode"] == 0
    assert transport.started_commands == [command]


def test_process_logs_tail_is_bounded_and_marked(tmp_path):
    env, transport, guest = make_env(
        tmp_path, process_log_buffer_bytes=64, process_log_tail_bytes=16
    )
    transport.process_scripts["cd /workspace && noisemaker"] = [
        ("stdout", b"x" * 200),
        ("exit", 0),
    ]

    handle = env.start_process("noisemaker")
    _wait_for(lambda: not env.process_logs(handle["id"])["running"])

    logs = env.process_logs(handle["id"])
    assert logs["truncated"] is True
    assert "earlier output dropped" in logs["output"]
    # The ring bounds memory: never more retained than the configured buffer.
    assert len(logs["stdout"]) <= 64 + len("... [earlier output dropped]\n")


def test_process_logs_tail_bytes_argument(tmp_path):
    env, transport, guest = make_env(tmp_path)
    transport.process_scripts["cd /workspace && seq"] = [("stdout", b"0123456789"), ("exit", 0)]

    handle = env.start_process("seq")
    _wait_for(lambda: not env.process_logs(handle["id"])["running"])

    logs = env.process_logs(handle["id"], tail_bytes=4)
    assert logs["stdout"] == "6789"


def test_unknown_process_id_is_an_error(tmp_path):
    env, transport, guest = make_env(tmp_path)
    with pytest.raises(SandboxProcessError) as excinfo:
        env.process_logs("nope")
    assert "unknown background process" in str(excinfo.value)


def test_stop_process_sends_term_then_finishes(tmp_path):
    env, transport, guest = make_env(tmp_path)
    # A command that never exits on its own: the fake emits an exit event only
    # once a signal arrives (see GuestTransport.signal_process).
    transport.process_scripts["cd /workspace && sleep forever"] = [("stdout", b"started\n")]

    handle = env.start_process("sleep forever")
    assert env.process_logs(handle["id"])["running"] is True

    result = env.stop_process(handle["id"])

    assert result["running"] is False
    assert transport.guest.signals == [(handle["remote_process_id"], "TERM")]
    assert result["returncode"] == 143


def test_stop_process_escalates_to_kill(tmp_path):
    """A process that ignores TERM must be KILLed, then cancelled."""
    env, transport, guest = make_env(tmp_path, process_stop_grace_seconds=1)

    class Deaf(GuestTransport):
        def signal_process(self, process_id, signal_name, timeout=30):
            self.guest.signals.append((process_id, signal_name))
            # Never let the process exit: force the escalation path.

    transport = Deaf(guest)
    transport.process_scripts["cd /workspace && stubborn"] = [("stdout", b"alive\n")]
    env, transport, guest = make_env(tmp_path, transport=transport, process_stop_grace_seconds=1)

    handle = env.start_process("stubborn")
    result = env.stop_process(handle["id"])

    assert [name for _, name in guest.signals] == ["TERM", "KILL"]
    assert result["running"] is False
    assert result["state"] == "stopped"
    assert transport.cancelled == 1


def test_stopping_a_finished_process_is_a_noop(tmp_path):
    env, transport, guest = make_env(tmp_path)
    transport.process_scripts["cd /workspace && quick"] = [("exit", 0)]
    handle = env.start_process("quick")
    _wait_for(lambda: not env.process_logs(handle["id"])["running"])

    result = env.stop_process(handle["id"])

    assert result["returncode"] == 0
    assert guest.signals == []


def test_concurrent_process_cap_enforced(tmp_path):
    env, transport, guest = make_env(tmp_path, process_max_concurrent=1)
    # `one` never exits on its own, so the session is genuinely at its cap.
    transport.process_scripts["cd /workspace && one"] = [("stdout", b"x")]
    transport.process_scripts["cd /workspace && two"] = [("exit", 0)]

    handle = env.start_process("one")
    try:
        with pytest.raises(SandboxProcessError) as excinfo:
            env.start_process("two")
        assert "already running" in str(excinfo.value)
    finally:
        env.stop_process(handle["id"])


def test_stream_failure_marks_process_failed(tmp_path):
    env, transport, guest = make_env(tmp_path)

    class Exploding(GuestTransport):
        def start_process(self, command, start_timeout):
            started = super().start_process(command, start_timeout)

            def events():
                # A generator so the failure surfaces on iteration (where the
                # plugin's reader thread sees it), not on the call itself.
                if False:
                    yield None
                raise RuntimeError("stream reset")

            started.events = events
            return started

    transport = Exploding(guest)
    env, transport, guest = make_env(tmp_path, transport=transport)

    handle = env.start_process("anything")
    _wait_for(lambda: env.process_logs(handle["id"])["state"] == "failed")
    logs = env.process_logs(handle["id"])
    assert "stream reset" in (logs["error"] or "")


def test_list_processes_reports_each_handle(tmp_path):
    env, transport, guest = make_env(tmp_path)
    transport.process_scripts["cd /workspace && a"] = [("exit", 0)]
    transport.process_scripts["cd /workspace && b"] = [("exit", 0)]

    first = env.start_process("a")
    second = env.start_process("b")

    listed = env.list_processes()
    assert [entry["id"] for entry in listed] == [first["id"], second["id"]]


def test_cleanup_stops_running_processes(tmp_path):
    env, transport, guest = make_env(tmp_path)
    transport.process_scripts["cd /workspace && daemon"] = [("stdout", b"up\n")]

    handle = env.start_process("daemon")
    assert env.process_logs(handle["id"])["running"] is True

    env.cleanup()

    assert guest.signals and guest.signals[0][1] == "TERM"
    assert guest.signals[0][0] == handle["remote_process_id"]


def test_start_process_uses_requested_cwd(tmp_path):
    env, transport, guest = make_env(tmp_path)
    guest.add_dir("/workspace/sub")
    transport.process_scripts["cd /workspace/sub && work"] = [("exit", 0)]

    env.start_process("work", cwd="/workspace/sub")

    assert transport.started_commands == ["cd /workspace/sub && work"]


def test_start_process_rejects_escaping_cwd(tmp_path):
    from hermes_agent_sandbox.errors import CwdNotAllowedError

    env, transport, guest = make_env(tmp_path)
    with pytest.raises(CwdNotAllowedError):
        env.start_process("work", cwd="/etc")


def _wait_for(predicate, timeout: float = 5.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.005)
    raise AssertionError("condition never became true")
