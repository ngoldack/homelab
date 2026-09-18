"""Background processes on the sandboxd streaming transport (plan Unit 2.5).

TRANSPORT DECISION. These use the runtime's NATIVE long-running exec — the
server-streaming ``ProcessService.Start`` RPC — not ``nohup`` + a workspace log
file. Evidence that it exists in the pinned runtime (sandboxd v1.0.2 @
9a85153590e54cb980f3241f9e7a9228449412c9):

* proto surface: ``rpc Start(StartRequest) returns (stream StartResponse)`` with
  ``InitEvent{process_id}`` → repeated ``stdout``/``stderr`` bytes →
  ``ExitEvent{exit_code}`` (k8s-agent-sandbox 1.0.2 process_pb2; verified by
  descriptor inspection), plus ``SendSignal{process_id, signal}``.
* server implementation: ``packages/sandboxd/pkg/server/process.go`` —
  ``Start`` sets ``cmd.WaitDelay``, streams chunks as they are read and records
  the exit code; ``SendSignal`` signals the process GROUP (Setpgid in
  ``buildCommand``), so a stopped process takes its children with it.
* ``packages/sandboxd/USER_GUIDE.md`` documents exactly this: "Process
  execution is served over gRPC because ``Start`` is a long-lived
  server-streaming RPC: stdout/stderr flow continuously until the process
  exits."

Why native beats the file fallback here: real exit codes, real signals, no
workspace pollution (a ``nohup`` log file would land in /workspace and show up
in checkpoints and to the model), and no pid-file staleness. The cost is one
reader thread and a bounded log buffer per process, both capped by config.

DRAINING IS MANDATORY. sandboxd's ``streamOutput`` sends each chunk on the gRPC
stream synchronously, so a client that stops reading eventually blocks the
remote process on a full pipe. Every process therefore owns a reader thread
that drains the stream into a bounded ring buffer.

SCRATCH NAMESPACE NOTE: nothing here writes into /workspace — that is the whole
point of choosing the native transport. ``files.SCRATCH_DIR`` is used only for
stdin payloads (file-mode fallback) and artifact tarballs.
"""

from __future__ import annotations

import logging
import secrets
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Deque, Dict, Iterator, List, Optional, Tuple

from .errors import SandboxProcessError
from .transport import START_EVENT_EXIT, START_EVENT_STDERR, START_EVENT_STDOUT

log = logging.getLogger(__name__)

# Marker prepended when a log read had to drop earlier output (the ring keeps
# the TAIL, which is what a person tailing a process actually wants).
LOG_TRUNCATION_MARKER = "... [earlier output dropped]\n"

# State values reported by process_logs/list_processes.
STATE_RUNNING = "running"
STATE_EXITED = "exited"
STATE_FAILED = "failed"
STATE_STOPPED = "stopped"


class _Ring:
    """Byte-bounded tail buffer (append + read-last-N), thread-safe by contract.

    Appends and reads happen on different threads (reader thread vs. the tool
    call that asks for logs), so the buffer is guarded by its own lock rather
    than by the registry lock — a log read must never block a stream drain.
    """

    def __init__(self, limit: int):
        self.limit = int(limit)
        self._chunks: Deque[bytes] = deque()
        self._size = 0
        self._dropped = False
        self._lock = threading.Lock()

    def append(self, chunk: bytes) -> None:
        if not chunk:
            return
        with self._lock:
            self._chunks.append(chunk)
            self._size += len(chunk)
            while self._size > self.limit and len(self._chunks) > 1:
                self._size -= len(self._chunks.popleft())
                self._dropped = True
            if self._size > self.limit:
                # A single chunk larger than the whole buffer: keep its tail.
                chunk = self._chunks.pop()
                self._chunks.append(chunk[-self.limit :])
                self._size = len(self._chunks[0])
                self._dropped = True

    def tail(self, max_bytes: int) -> Tuple[str, bool]:
        """Return the last *max_bytes* decoded as text, plus a truncation flag.

        Two different omissions are reported differently on purpose: the marker
        means the RING dropped bytes (history is genuinely gone), while a
        smaller-than-buffer ``max_bytes`` only sets the flag — the caller asked
        for a tail, so those bytes are not missing, they were not requested.
        """
        with self._lock:
            data = b"".join(self._chunks)
            ring_dropped = self._dropped
            truncated = ring_dropped or len(data) > max_bytes
        if len(data) > max_bytes:
            data = data[-max_bytes:]
        text = data.decode("utf-8", errors="replace")
        if ring_dropped:
            text = LOG_TRUNCATION_MARKER + text
        return text, truncated


@dataclass
class BackgroundProcess:
    """One background process plus everything the plugin knows about it."""

    id: str
    remote_process_id: int
    command: str
    started_at: float
    stdout: _Ring
    stderr: _Ring
    state: str = STATE_RUNNING
    exit_code: Optional[int] = None
    error: Optional[str] = None
    finished_at: Optional[float] = None
    # True once stop() decided this process must go away: a stream that then
    # ends without an exit event is "stopped", not "failed".
    _stopping: bool = field(default=False, repr=False)
    _started: Any = field(default=None, repr=False)
    _thread: Optional[threading.Thread] = field(default=None, repr=False)
    _done: threading.Event = field(default_factory=threading.Event, repr=False)

    @property
    def running(self) -> bool:
        return self.state == STATE_RUNNING

    def logs(self, tail_bytes: int) -> Dict[str, Any]:
        """Return a snapshot of the process' output (tail-bounded)."""
        out, out_dropped = self.stdout.tail(tail_bytes)
        err, err_dropped = self.stderr.tail(tail_bytes)
        # Merge like execute() does, so a caller can use one field for display
        # and still reach the split streams when it needs the distinction.
        if out and err:
            output = out + err
        else:
            output = out or err
        return {
            "id": self.id,
            "remote_process_id": self.remote_process_id,
            "command": self.command,
            "state": self.state,
            "running": self.running,
            "returncode": self.exit_code,
            "error": self.error,
            "output": output,
            "stdout": out,
            "stderr": err,
            "truncated": out_dropped or err_dropped,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
        }

    def stop(self, transport: Any, *, grace_seconds: float, signal_timeout: int = 30) -> Dict[str, Any]:
        """TERM → grace → KILL → cancel, then report the final state.

        The escalation matters on a Kata guest: a process ignoring SIGTERM (or a
        child that inherited the pipe) would otherwise keep the claim occupied.
        Cancelling the stream is the last resort: sandboxd kills the child when
        the RPC context ends, so the process cannot outlive its stream.
        """
        if not self.running:
            return self.logs(0)
        self._stopping = True
        for signal_name, wait in (("TERM", grace_seconds), ("KILL", grace_seconds)):
            try:
                transport.signal_process(self.remote_process_id, signal_name, signal_timeout)
            except SandboxProcessError as exc:
                # Already gone (NOT_FOUND) is the happy race, not a failure.
                log.debug("signal %s to process %s failed: %s", signal_name, self.id, exc)
                break
            if self._done.wait(wait):
                return self.logs(0)
        try:
            self._started.cancel()
        except Exception as exc:  # noqa: BLE001 - cancel is best-effort
            log.debug("stream cancel for process %s failed: %s", self.id, exc)
        self._done.wait(grace_seconds)
        if self.running:
            self._mark(STATE_STOPPED, error="stop escalated to stream cancel")
        return self.logs(0)

    def _mark(self, state: str, *, exit_code: Optional[int] = None, error: Optional[str] = None) -> None:
        self.state = state
        if exit_code is not None:
            self.exit_code = exit_code
        if error is not None:
            self.error = error
        if self.finished_at is None:
            self.finished_at = time.time()
        self._done.set()

    def as_dict(self) -> Dict[str, Any]:
        return {
            "id": self.id,
            "remote_process_id": self.remote_process_id,
            "command": self.command,
            "state": self.state,
            "running": self.running,
            "returncode": self.exit_code,
            "error": self.error,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
        }


class ProcessRegistry:
    """Session-scoped registry of background processes (one per environment)."""

    def __init__(
        self,
        transport: Any,
        *,
        max_processes: int,
        log_buffer_bytes: int,
        log_tail_bytes: int,
        stop_grace_seconds: float,
        clock: Any = time.time,
    ):
        self._transport = transport
        self.max_processes = int(max_processes)
        self.log_buffer_bytes = int(log_buffer_bytes)
        self.log_tail_bytes = int(log_tail_bytes)
        self.stop_grace_seconds = float(stop_grace_seconds)
        self._clock = clock
        self._processes: Dict[str, BackgroundProcess] = {}
        self._lock = threading.Lock()

    # ---------------- lifecycle ----------------
    def start(self, command: str, *, start_timeout: int) -> BackgroundProcess:
        """Start *command* in the sandbox and drain its stream in the background."""
        with self._lock:
            running = sum(1 for proc in self._processes.values() if proc.running)
            if running >= self.max_processes:
                raise SandboxProcessError(
                    f"{running} background processes already running "
                    f"(AGENT_SANDBOX_PROCESS_MAX_CONCURRENT={self.max_processes}); "
                    "stop one or wait for it to exit"
                )
        started = self._transport.start_process(command, start_timeout)
        handle = BackgroundProcess(
            id=secrets.token_hex(6),
            remote_process_id=started.process_id,
            command=command,
            started_at=self._clock(),
            stdout=_Ring(self.log_buffer_bytes),
            stderr=_Ring(self.log_buffer_bytes),
            _started=started,
        )
        thread = threading.Thread(
            target=self._drain,
            args=(handle, started.events()),
            name=f"agent-sandbox-proc-{handle.id}",
            daemon=True,
        )
        handle._thread = thread
        with self._lock:
            self._processes[handle.id] = handle
        thread.start()
        log.info(
            "started background process %s (remote pid %s): %s",
            handle.id, handle.remote_process_id, command,
        )
        return handle

    def _drain(self, handle: BackgroundProcess, events: Iterator[Tuple[str, Any]]) -> None:
        """Reader thread: consume the stream until exit (or a stream failure).

        This is the loop that makes the remote process able to run at all: the
        server blocks on ``stream.Send`` when the client stops reading.
        """
        try:
            for kind, value in events:
                if kind == START_EVENT_STDOUT:
                    handle.stdout.append(value)
                elif kind == START_EVENT_STDERR:
                    handle.stderr.append(value)
                elif kind == START_EVENT_EXIT:
                    handle._mark(STATE_EXITED, exit_code=int(value))
                    return
        except Exception as exc:  # noqa: BLE001 - reported through the handle
            if handle._done.is_set():
                return
            if handle._stopping:
                # A cancelled stream is how a stop escalation ends the reader;
                # that is the intended outcome, not a failure.
                handle._mark(STATE_STOPPED)
                return
            log.warning("background process %s stream failed: %s", handle.id, exc)
            handle._mark(STATE_FAILED, error=f"{type(exc).__name__}: {exc}")
            return
        if not handle._done.is_set():
            if handle._stopping:
                handle._mark(STATE_STOPPED)
            else:
                handle._mark(STATE_FAILED, error="stream ended without an exit event")

    def get(self, process_id: str) -> BackgroundProcess:
        """Look up one process by its plugin-side id."""
        with self._lock:
            handle = self._processes.get(process_id)
        if handle is None:
            known = ", ".join(sorted(self._processes)) or "none"
            raise SandboxProcessError(
                f"unknown background process {process_id!r} (known: {known})"
            )
        return handle

    def list(self) -> List[Dict[str, Any]]:
        with self._lock:
            handles = list(self._processes.values())
        return [handle.as_dict() for handle in sorted(handles, key=lambda h: h.started_at)]

    def stop(self, process_id: str) -> Dict[str, Any]:
        handle = self.get(process_id)
        return handle.stop(
            self._transport,
            grace_seconds=self.stop_grace_seconds,
        )

    def stop_all(self) -> List[str]:
        """Stop every running process (sandbox teardown path); returns their ids.

        Called from ``AgentSandboxEnvironment.cleanup``: deleting the claim
        already kills the guest, but stopping first makes the sequence explicit
        and lets a still-live stream produce its final output before the channel
        goes away.
        """
        with self._lock:
            handles = list(self._processes.values())
        stopped: List[str] = []
        for handle in handles:
            if handle.running:
                try:
                    handle.stop(self._transport, grace_seconds=self.stop_grace_seconds)
                    stopped.append(handle.id)
                except Exception as exc:  # noqa: BLE001 - teardown must not raise
                    log.warning("stopping background process %s failed: %s", handle.id, exc)
        return stopped

    def forget_finished(self) -> int:  # pragma: no cover - housekeeping helper
        """Drop finished handles; returns how many were removed."""
        with self._lock:
            finished = [pid for pid, handle in self._processes.items() if not handle.running]
            for pid in finished:
                del self._processes[pid]
        return len(finished)
