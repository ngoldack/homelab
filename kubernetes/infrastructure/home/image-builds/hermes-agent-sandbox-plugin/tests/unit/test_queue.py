"""Session queue: FIFO admission, bounds, timeout, slot release (Unit 2.5)."""

from __future__ import annotations

import threading
import time

import pytest

from hermes_agent_sandbox.errors import (
    SandboxCommandError,
    SandboxQueueFullError,
    SandboxQueueTimeoutError,
)
from hermes_agent_sandbox.queue import SessionCommandQueue


def _queue(**overrides):
    kwargs = {"max_concurrent": 1, "max_queued": 1, "wait_timeout": 5.0, "name": "test"}
    kwargs.update(overrides)
    return SessionCommandQueue(**kwargs)


def test_immediate_admission_when_idle():
    queue = _queue()
    assert queue.submit(lambda: "ran") == "ran"
    assert queue.running == 0 and queue.queued == 0
    assert queue.stats()["completed"] == 1


def test_full_queue_rejects_immediately():
    queue = _queue(max_concurrent=1, max_queued=1)
    release = threading.Event()
    started = threading.Event()

    def blocker():
        started.set()
        release.wait(5)
        return "blocked"

    holder = threading.Thread(target=lambda: queue.submit(blocker))
    holder.start()
    assert started.wait(5)

    waiter = threading.Thread(target=lambda: queue.submit(lambda: "waiting"))
    waiter.start()
    deadline = time.monotonic() + 5
    while queue.queued < 1 and time.monotonic() < deadline:
        time.sleep(0.005)

    with pytest.raises(SandboxQueueFullError) as excinfo:
        queue.submit(lambda: "rejected")
    assert "max_queued=1" in str(excinfo.value)

    release.set()
    holder.join(5)
    waiter.join(5)
    assert queue.stats()["rejected_full"] == 1


def test_waiters_are_admitted_in_arrival_order():
    queue = _queue(max_concurrent=1, max_queued=8)
    order = []
    release_first = threading.Event()

    def first():
        release_first.wait(5)
        order.append("first")

    threads = [threading.Thread(target=lambda: queue.submit(first))]
    threads[0].start()
    deadline = time.monotonic() + 5
    while queue.running < 1 and time.monotonic() < deadline:
        time.sleep(0.005)

    for index in range(2, 5):
        threads.append(
            threading.Thread(target=lambda i=index: queue.submit(lambda: order.append(f"w{i}")))
        )
        threads[-1].start()
        while queue.queued < index - 1 and time.monotonic() < deadline:
            time.sleep(0.005)

    release_first.set()
    for thread in threads:
        thread.join(5)

    assert order == ["first", "w2", "w3", "w4"]


def test_waiter_times_out_without_executing():
    queue = _queue(max_concurrent=1, max_queued=4, wait_timeout=0.2)
    release = threading.Event()
    ran = []

    holder = threading.Thread(target=lambda: queue.submit(lambda: release.wait(5)))
    holder.start()
    deadline = time.monotonic() + 5
    while queue.running < 1 and time.monotonic() < deadline:
        time.sleep(0.005)

    with pytest.raises(SandboxQueueTimeoutError) as excinfo:
        queue.submit(lambda: ran.append("nope"))
    assert "nothing was executed" in str(excinfo.value)
    assert ran == []

    release.set()
    holder.join(5)
    assert queue.stats()["rejected_timeout"] == 1


def test_failed_callable_releases_its_slot():
    queue = _queue(max_concurrent=1, max_queued=2)

    with pytest.raises(ValueError):
        queue.submit(lambda: (_ for _ in ()).throw(ValueError("boom")))

    assert queue.running == 0
    assert queue.submit(lambda: "after") == "after"
    assert queue.stats()["failed"] == 1


def test_concurrency_cap_is_respected():
    queue = _queue(max_concurrent=2, max_queued=8)
    peak = {"value": 0, "current": 0}
    lock = threading.Lock()
    gate = threading.Event()

    def work():
        with lock:
            peak["current"] += 1
            peak["value"] = max(peak["value"], peak["current"])
            if peak["current"] >= 2:
                gate.set()
        time.sleep(0.05)
        with lock:
            peak["current"] -= 1
        return "done"

    threads = [threading.Thread(target=lambda: queue.submit(work)) for _ in range(3)]
    for thread in threads:
        thread.start()
    assert gate.wait(5), "two workers never ran concurrently"
    for thread in threads:
        thread.join(5)
    assert peak["value"] == 2
    assert queue.stats()["admitted"] == 3


def test_close_unblocks_waiting_submissions():
    queue = _queue(max_concurrent=1, max_queued=4, wait_timeout=30)
    release = threading.Event()
    errors = []

    holder = threading.Thread(target=lambda: queue.submit(lambda: release.wait(5)))
    holder.start()
    deadline = time.monotonic() + 5
    while queue.running < 1 and time.monotonic() < deadline:
        time.sleep(0.005)

    def waiter():
        try:
            queue.submit(lambda: "never")
        except SandboxCommandError as exc:
            errors.append(str(exc))

    waiting = threading.Thread(target=waiter)
    waiting.start()
    while queue.queued < 1 and time.monotonic() < deadline:
        time.sleep(0.005)

    queue.close()
    waiting.join(5)
    assert errors and "closed" in errors[0]
    assert queue.stats()["rejected_closed"] == 1

    with pytest.raises(SandboxCommandError):
        queue.submit(lambda: "closed")
    release.set()
    holder.join(5)


def test_environment_commands_go_through_the_queue(tmp_path):
    """A session's commands are admitted by ITS queue, with its own stats."""
    from _guest import Guest, make_env

    env, transport, guest = make_env(tmp_path, queue_max_concurrent=1, queue_max_queued=2)

    env.execute("true")
    env.execute("true")

    stats = env.queue_stats()
    assert stats["name"].startswith("sandbox:")
    assert stats["admitted"] == 2 and stats["completed"] == 2 and stats["queued"] == 0
    assert stats["max_concurrent"] == 1 and stats["max_queued"] == 2


def test_environment_queue_rejects_when_saturated(tmp_path):
    from _guest import Guest, make_env

    env, transport, guest = make_env(tmp_path, queue_max_concurrent=1, queue_max_queued=0)
    gate = threading.Event()
    started = threading.Event()

    def slow(command, timeout):
        started.set()
        gate.wait(5)
        return "", "", 0

    transport.run = slow
    thread = threading.Thread(target=lambda: env.execute("true"))
    thread.start()
    assert started.wait(5)

    with pytest.raises(SandboxQueueFullError):
        env.execute("true")
    gate.set()
    thread.join(5)
