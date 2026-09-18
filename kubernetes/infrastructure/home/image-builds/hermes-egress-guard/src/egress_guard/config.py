"""Environment plumbing shared by both roles.

Configuration is fail-fast: a missing required value or an unparseable integer
aborts startup with a non-zero exit, which surfaces as a CrashLoopBackOff
instead of a silently misconfigured guard (the plan's "wrong key must fail
loudly" rule applied to our own config).
"""

from __future__ import annotations

import os
from typing import Optional


def env_str(name: str, default: Optional[str] = None) -> Optional[str]:
    value = os.environ.get(name)
    if value is None or value == "":
        return default
    return value


def env_required(name: str) -> str:
    value = env_str(name)
    if value is None:
        raise SystemExit(f"{name} is required")
    return value


def env_int(name: str, default: int) -> int:
    raw = env_str(name)
    if raw is None:
        return default
    try:
        return int(raw)
    except ValueError:
        raise SystemExit(f"{name} must be an integer, got {raw!r}") from None


def env_float(name: str, default: float) -> float:
    raw = env_str(name)
    if raw is None:
        return default
    try:
        return float(raw)
    except ValueError:
        raise SystemExit(f"{name} must be a number, got {raw!r}") from None
