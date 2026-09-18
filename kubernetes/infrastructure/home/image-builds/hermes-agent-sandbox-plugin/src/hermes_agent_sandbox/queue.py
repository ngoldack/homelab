"""Session-scoped bounded FIFO admission for sandbox work (plan Unit 2.5).

WHY a queue exists at all: one Hermes session maps onto ONE Kata sandbox with
1 CPU request / 4 CPU limit (hermes-sandbox/sandbox-template.yaml). Hermes can
issue several tool calls concurrently, and every command competes for that one
guest; an unbounded fan-out both thrashes the guest and multiplies the blast
radius of a single wedged call (each one holding a gRPC stream and a thread).
The gate is therefore per ENVIRONMENT INSTANCE (one session = one sandbox), not
process-wide: the process-wide capacity gate for *sandboxes*
(``environment._CAPACITY``) is a different, coarser concern.

Semantics that callers rely on:

* **FIFO admission.** A new submission never jumps ahead of a waiting one, even
  when a slot happens to be free — otherwise a burst of short calls would starve
  a long-queued call forever.
* **Bounded.** ``max_queued`` waiters is a hard ceiling; the next submission is
  rejected immediately with :class:`SandboxQueueFullError` instead of growing an
  unbounded backlog of threads.
* **Bounded wait.** A waiter that cannot be admitted within ``wait_timeout``
  gives up with :class:`SandboxQueueTimeoutError`; the timeout measures *queue
  wait only*, never the execution of the callable itself (command deadlines are
  the transport's business and are reported as returncode 124).
* **Failure never leaks a slot.** A raising callable still releases its slot, so
  one failed command cannot permanently shrink session capacity.

Concurrency above 1 is deliberate: file path probes and REST transfers run on
this queue too, and serializing them behind a 300 s command would make a simple
``read_file`` block for minutes. The cap still bounds total guest pressure.
"""

from __future__ import annotations

import logging
import threading
import time
from collections import deque
from dataclasses import dataclass
from typing import Any, Callable, Deque, Dict, Optional, TypeVar

from .errors import SandboxCommandError, SandboxQueueFullError, SandboxQueueTimeoutError

log = logging.getLogger(__name__)

T = TypeVar("T")


@dataclass
class _Ticket:
    """A waiter's place in line; flips to admitted exactly once."""

    admitted: bool = False


@dataclass
class QueueStats:
    """Counters kept for diagnostics/tests; never part of a correctness path."""

    submitted: int = 0
    admitted: int = 0
    completed: int = 0
    failed: int = 0
    rejected_full: int = 0
    rejected_timeout: int = 0
    rejected_closed: int = 0
    high_water_queued: int = 0


class SessionCommandQueue:
    """Bounded FIFO admission gate over one session's sandbox.

    Thread-safe (the sandbox is driven from several Hermes threads); no
    background threads of its own, so an idle session costs nothing.
    """

    def __init__(
        self,
        *,
        max_concurrent: int,
        max_queued: int,
        wait_timeout: float,
        name: str = "session",
        clock: Callable[[], float] = time.monotonic,
    ):
        if max_concurrent < 1:
            raise ValueError("max_concurrent must be >= 1")
        if max_queued < 0:
            raise ValueError("max_queued must be >= 0")
        if wait_timeout <= 0:
            raise ValueError("wait_timeout must be > 0")
        self.max_concurrent = int(max_concurrent)
        self.max_queued = int(max_queued)
        self.wait_timeout = float(wait_timeout)
        self.name = name
        self._clock = clock
        self._cv = threading.Condition()
        self._running = 0
        self._waiting: Deque[_Ticket] = deque()
        self._closed = False
        self._stats = QueueStats()

    # ---------------- submission ----------------
    def submit(self, fn: Callable[[], T], *, wait_timeout: Optional[float] = None) -> T:
        """Admit and run *fn*; raises before running when full, timed out, or closed."""
        limit = self.wait_timeout if wait_timeout is None else float(wait_timeout)
        with self._cv:
            if self._closed:
                self._stats.rejected_closed += 1
                raise SandboxCommandError(f"{self.name}: command queue is closed")
            self._stats.submitted += 1
            if self._running < self.max_concurrent and not self._waiting:
                self._running += 1
                self._stats.admitted += 1
            else:
                if len(self._waiting) >= self.max_queued:
                    self._stats.rejected_full += 1
                    raise SandboxQueueFullError(
                        f"{self.name}: command queue full "
                        f"({self._running} running, {len(self._waiting)} waiting, "
                        f"max_queued={self.max_queued}); retry after a command finishes"
                    )
                ticket = _Ticket()
                self._waiting.append(ticket)
                if len(self._waiting) > self._stats.high_water_queued:
                    self._stats.high_water_queued = len(self._waiting)
                deadline = self._clock() + limit
                while not ticket.admitted:
                    if self._closed:
                        self._drop_ticket(ticket)
                        self._stats.rejected_closed += 1
                        raise SandboxCommandError(f"{self.name}: command queue is closed")
                    remaining = deadline - self._clock()
                    if remaining <= 0:
                        self._drop_ticket(ticket)
                        self._stats.rejected_timeout += 1
                        raise SandboxQueueTimeoutError(
                            f"{self.name}: no command slot within {limit:.1f}s "
                            f"({self._running} running); nothing was executed"
                        )
                    self._cv.wait(remaining)
                self._stats.admitted += 1
        try:
            result = fn()
        except BaseException:
            self._finish(failed=True)
            raise
        self._finish(failed=False)
        return result

    def _finish(self, *, failed: bool) -> None:
        """Release the caller's slot and hand it to the oldest waiter."""
        with self._cv:
            if failed:
                self._stats.failed += 1
            else:
                self._stats.completed += 1
            self._release_one()
            self._cv.notify_all()

    def _drop_ticket(self, ticket: _Ticket) -> None:
        """Remove a waiting ticket (caller holds ``self._cv``)."""
        try:
            self._waiting.remove(ticket)
        except ValueError:  # already forwarded to a runner — cannot happen under the lock
            pass

    def _release_one(self) -> None:
        """Hand the finished slot to the oldest waiter, else free it."""
        while self._waiting:
            ticket = self._waiting.popleft()
            if ticket.admitted:  # defensive: admitted tickets are never queued
                continue
            ticket.admitted = True
            return  # `_running` transfers to the new holder unchanged
        self._running -= 1

    def close(self) -> None:
        """Refuse new work and wake every waiter (teardown path).

        Waiters are released with :class:`SandboxCommandError`, not with the
        queue-full/queue-timeout errors: the session ended, and reporting a
        capacity condition would be a lie.
        """
        with self._cv:
            self._closed = True
            self._cv.notify_all()

    # ---------------- introspection ----------------
    @property
    def running(self) -> int:
        with self._cv:
            return self._running

    @property
    def queued(self) -> int:
        with self._cv:
            return len(self._waiting)

    def stats(self) -> Dict[str, Any]:
        """Snapshot of counters + current occupancy (diagnostics/tests only)."""
        with self._cv:
            return {
                "name": self.name,
                "running": self._running,
                "queued": len(self._waiting),
                "max_concurrent": self.max_concurrent,
                "max_queued": self.max_queued,
                "closed": self._closed,
                **{
                    field: getattr(self._stats, field)
                    for field in QueueStats.__dataclass_fields__
                },
            }
