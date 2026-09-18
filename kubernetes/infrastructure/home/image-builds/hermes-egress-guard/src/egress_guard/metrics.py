"""Thread-safe Prometheus text-format counters (stdlib only).

Only counters exist here on purpose: the alerting rules in
``monitoring/vmrules`` (plan Unit 1.4) consume ``hermes_egress_denied_total``
and friends, and counter semantics need no exposition-format exotica. The
renderer is deterministic (sorted series) so tests can assert on exact lines.
"""

from __future__ import annotations

import threading
from typing import Dict, Iterable, Mapping, Tuple

_Labels = Tuple[Tuple[str, str], ...]


def _escape(value: str) -> str:
    # Prometheus text format: backslash, double quote and newline must be
    # escaped inside label values.
    return value.replace("\\", "\\\\").replace('"', '\\"').replace("\n", "\\n")


class Metrics:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._help: Dict[str, str] = {}
        self._values: Dict[Tuple[str, _Labels], float] = {}

    def declare(self, name: str, help_text: str) -> None:
        """Register a counter's HELP text; unlabelled counters render as 0."""
        with self._lock:
            self._help.setdefault(name, help_text)

    def inc(self, name: str, labels: Mapping[str, str] | None = None, amount: float = 1) -> None:
        key = (name, tuple(sorted((str(k), str(v)) for k, v in (labels or {}).items())))
        with self._lock:
            self._values[key] = self._values.get(key, 0) + amount

    def value(self, name: str, labels: Mapping[str, str] | None = None) -> float:
        key = (name, tuple(sorted((str(k), str(v)) for k, v in (labels or {}).items())))
        with self._lock:
            return self._values.get(key, 0)

    def render(self) -> str:
        with self._lock:
            values = dict(self._values)
            help_text = dict(self._help)
        for name in help_text:
            values.setdefault((name, ()), 0)
        lines: list[str] = []
        current: str | None = None
        for (name, labels), value in sorted(values.items()):
            if name != current:
                if name in help_text:
                    lines.append(f"# HELP {name} {help_text[name]}")
                lines.append(f"# TYPE {name} counter")
                current = name
            rendered = str(int(value)) if float(value).is_integer() else repr(float(value))
            if labels:
                label_text = ",".join(f'{k}="{_escape(v)}"' for k, v in labels)
                lines.append(f"{name}{{{label_text}}} {rendered}")
            else:
                lines.append(f"{name} {rendered}")
        return "\n".join(lines) + "\n"


def iter_names(metrics: Metrics) -> Iterable[str]:  # pragma: no cover - debugging helper
    return sorted({name for name, _ in metrics.render().split("\n") if name.startswith("hermes_")})
