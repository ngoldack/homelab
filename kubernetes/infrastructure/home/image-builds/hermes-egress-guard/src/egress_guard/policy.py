"""Deterministic egress policy engine (plan Unit 3.2).

The policy is JSON loaded from ``EGRESS_POLICY_FILE`` (mounted from the
``egress-authorizer-policy`` ConfigMap, key ``policy.json``). Nothing about a
profile is embedded in this code: an unknown profile is a denial, never an
implicit allow, so a ConfigMap edit is the only way to widen egress.

Decision order (deterministic, no DNS, no clock-dependent branching beyond the
strike window):

1. unauthenticated -> deny ``unauthenticated`` (no strike: there is no session
   to count against).
2. instant-kill destinations -> kill ``kill-destination``. Only IP-literal
   targets are evaluated; a hostname is never resolved here (the authorizer
   makes no network calls, and DNS-rebinding defence is Cilium's job).
3. unknown profile -> deny ``profile-unknown`` (no strike: a policy mistake,
   not sandbox behaviour).
4. host/port not allowlisted -> deny ``not-allowlisted`` (strike; the strike
   that reaches the threshold escalates the same response to a kill).
5. byte budget exceeded -> deny ``budget-exhausted`` (strike, same escalation).

Strike accounting: ``EGRESS_STRIKE_THRESHOLD`` denies inside
``EGRESS_STRIKE_WINDOW_S`` seconds => quarantine. Strikes are per session hash
and in-memory only, bounded by ``EGRESS_MAX_SESSIONS`` (LRU by last use), so a
restart forgets strikes but never the quarantine ConfigMap.
"""

from __future__ import annotations

import ipaddress
import json
import threading
import time
from collections import deque
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, Mapping, Optional, Tuple

ALLOW = "allow"
DENY = "deny"
KILL = "kill"

# Instant-kill baseline, independent of the policy file: RFC1918, loopback,
# link-local (including the 169.254.169.254 cloud metadata address), CGNAT,
# unspecified/multicast/reserved space and their IPv6 counterparts. These are
# never reachable through the guard even if a policy file were to allowlist
# them, so a compromised/misconfigured policy cannot re-open the management
# plane. Cluster-specific ranges (pod/service CIDR, node subnet, management
# networks) are added by the policy file's ``kill_cidrs``.
BUILTIN_KILL_CIDRS: Tuple[str, ...] = (
    "0.0.0.0/8",
    "10.0.0.0/8",
    "100.64.0.0/10",
    "127.0.0.0/8",
    "169.254.0.0/16",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "224.0.0.0/4",
    "240.0.0.0/4",
    "::/128",
    "::1/128",
    "fc00::/7",
    "fe80::/10",
    "ff00::/8",
)

# Always-kill addresses by value, on top of the ranges above. 169.254.169.254 is
# already inside 169.254.0.0/16; it is listed explicitly because it is the
# metadata endpoint the plan calls out by name (and because an operator reading
# the policy should see it).
BUILTIN_KILL_EXACT: Tuple[str, ...] = ("169.254.169.254",)


class PolicyError(ValueError):
    """The mounted policy file is unparseable or violates the schema."""


@dataclass(frozen=True)
class AllowEntry:
    """One allowlist entry: exact host or ``.suffix`` domain, plus ports."""

    host: str
    ports: frozenset[int]

    def matches(self, host: str, port: int) -> bool:
        if port not in self.ports:
            return False
        normalized = host.lower().rstrip(".")
        if self.host.startswith("."):
            bare = self.host[1:]
            return normalized == bare or normalized.endswith(self.host)
        return normalized == self.host


@dataclass(frozen=True)
class ProfilePolicy:
    name: str
    allow: Tuple[AllowEntry, ...]
    budget_bytes: Optional[int] = None

    def allows(self, host: str, port: int) -> bool:
        return any(entry.matches(host, port) for entry in self.allow)


def _parse_ip(host: str) -> Optional[ipaddress._BaseAddress]:
    candidate = host.strip()
    if candidate.startswith("[") and candidate.endswith("]"):
        candidate = candidate[1:-1]
    try:
        return ipaddress.ip_address(candidate)
    except ValueError:
        return None


@dataclass(frozen=True)
class Policy:
    version: str
    profiles: Mapping[str, ProfilePolicy]
    kill_cidrs: Tuple[Any, ...] = ()
    kill_exact: frozenset = frozenset()

    def kill_reason(self, host: str) -> Optional[str]:
        """Return ``kill-destination`` when *host* is an IP literal we never
        reach out to, else ``None``. Hostnames are not resolved."""
        address = _parse_ip(host)
        if address is None:
            return None
        candidates = [address]
        mapped = getattr(address, "ipv4_mapped", None)
        if mapped is not None:
            candidates.append(mapped)
        for candidate in candidates:
            if candidate in self.kill_exact:
                return "kill-destination"
            for network in self.kill_cidrs:
                if candidate.version == network.version and candidate in network:
                    return "kill-destination"
        return None


def _reject_unknown(obj: Mapping[str, Any], allowed: set[str], where: str) -> None:
    unknown = set(obj) - allowed
    if unknown:
        raise PolicyError(
            f"{where}: unknown key(s) {sorted(unknown)}; allowed: {sorted(allowed)}"
        )


def _as_int(value: Any, where: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise PolicyError(f"{where}: expected an integer, got {value!r}")
    return value


def load_policy(path: str) -> Policy:
    """Read and validate the policy file. Raises :class:`PolicyError`."""
    with open(path, "r", encoding="utf-8") as handle:
        raw = json.load(handle)
    if not isinstance(raw, dict):
        raise PolicyError("policy: expected a JSON object at the top level")
    _reject_unknown(raw, {"version", "profiles", "kill_cidrs", "kill_exact"}, "policy")

    version = raw.get("version")
    if not isinstance(version, str) or not version.strip():
        raise PolicyError("policy.version: expected a non-empty string")

    raw_profiles = raw.get("profiles")
    if not isinstance(raw_profiles, dict) or not raw_profiles:
        raise PolicyError("policy.profiles: expected a non-empty object")

    profiles: Dict[str, ProfilePolicy] = {}
    for name, body in raw_profiles.items():
        where = f"policy.profiles.{name}"
        if not isinstance(body, dict):
            raise PolicyError(f"{where}: expected an object")
        _reject_unknown(body, {"allow", "budget_bytes"}, where)
        raw_allow = body.get("allow", [])
        if not isinstance(raw_allow, list):
            raise PolicyError(f"{where}.allow: expected a list")
        entries = []
        for index, entry in enumerate(raw_allow):
            entry_where = f"{where}.allow[{index}]"
            if not isinstance(entry, dict):
                raise PolicyError(f"{entry_where}: expected an object")
            _reject_unknown(entry, {"host", "ports"}, entry_where)
            host = entry.get("host")
            if not isinstance(host, str) or not host.strip():
                raise PolicyError(f"{entry_where}.host: expected a non-empty string")
            host = host.strip().lower()
            if host.endswith(".."):
                raise PolicyError(f"{entry_where}.host: malformed host {host!r}")
            raw_ports = entry.get("ports")
            if not isinstance(raw_ports, list) or not raw_ports:
                raise PolicyError(f"{entry_where}.ports: expected a non-empty list")
            ports = frozenset(
                _parse_port(port, f"{entry_where}.ports", name) for port in raw_ports
            )
            entries.append(AllowEntry(host=host, ports=ports))
        budget = body.get("budget_bytes")
        if budget is not None:
            budget = _as_int(budget, f"{where}.budget_bytes")
            if budget <= 0:
                raise PolicyError(f"{where}.budget_bytes: must be positive")
        profiles[name] = ProfilePolicy(name=name, allow=tuple(entries), budget_bytes=budget)

    networks = [ipaddress.ip_network(cidr, strict=False) for cidr in _cidr_list(raw, "kill_cidrs")]
    exact = frozenset(
        _parse_address(value, "kill_exact") for value in _cidr_list(raw, "kill_exact")
    )
    builtin_networks = [ipaddress.ip_network(cidr) for cidr in BUILTIN_KILL_CIDRS]
    builtin_exact = frozenset(
        _parse_address(value, "builtin-kill-exact") for value in BUILTIN_KILL_EXACT
    )
    return Policy(
        version=version,
        profiles=profiles,
        kill_cidrs=tuple(builtin_networks) + tuple(networks),
        kill_exact=frozenset(builtin_exact | exact),
    )


def _cidr_list(raw: Mapping[str, Any], key: str) -> list:
    values = raw.get(key, [])
    if not isinstance(values, list):
        raise PolicyError(f"policy.{key}: expected a list")
    return values


def _parse_port(value: Any, where: str, profile: str) -> int:
    port = _as_int(value, where)
    if not 1 <= port <= 65535:
        raise PolicyError(f"{where}: port {port} out of range for profile {profile!r}")
    return port


def _parse_address(value: Any, where: str):
    if not isinstance(value, str):
        raise PolicyError(f"policy.{where}: expected a string, got {value!r}")
    try:
        return ipaddress.ip_address(value)
    except ValueError:
        raise PolicyError(f"policy.{where}: not an IP address: {value!r}") from None


@dataclass
class Identity:
    """A verified session identity (post token verification)."""

    session_hash: str
    profile: str


@dataclass(frozen=True)
class Decision:
    kind: str
    reason: str
    strikes: int
    status: int


@dataclass
class _SessionState:
    denies: deque = field(default_factory=deque)
    bytes_used: int = 0
    last_seen: float = 0.0


class EgressEngine:
    """Stateless policy + in-memory per-session strike/byte accounting."""

    def __init__(
        self,
        policy: Policy,
        *,
        strike_threshold: int = 3,
        strike_window_s: float = 60.0,
        max_sessions: int = 4096,
        clock: Callable[[], float] = time.time,
    ) -> None:
        if strike_threshold < 1:
            raise ValueError("strike_threshold must be >= 1")
        self.policy = policy
        self.strike_threshold = strike_threshold
        self.strike_window_s = float(strike_window_s)
        self.max_sessions = max_sessions
        self._clock = clock
        self._sessions: Dict[str, _SessionState] = {}
        self._lock = threading.Lock()

    def evaluate(
        self,
        identity: Optional[Identity],
        host: str,
        port: int,
        *,
        byte_hint: int = 0,
        now: Optional[float] = None,
    ) -> Decision:
        moment = self._clock() if now is None else now
        if identity is None:
            return Decision(DENY, "unauthenticated", 0, 403)
        with self._lock:
            state = self._state(identity.session_hash, moment)
            kill = self.policy.kill_reason(host)
            if kill is not None:
                return Decision(KILL, kill, self._note_deny(state, moment), 403)
            profile = self.policy.profiles.get(identity.profile)
            if profile is None:
                return Decision(DENY, "profile-unknown", self._window(state, moment), 403)
            if not profile.allows(host, port):
                return self._deny(state, moment, "not-allowlisted")
            if profile.budget_bytes is not None and byte_hint > 0:
                state.bytes_used += int(byte_hint)
                if state.bytes_used > profile.budget_bytes:
                    return self._deny(state, moment, "budget-exhausted")
            return Decision(ALLOW, "allowed", self._window(state, moment), 200)

    def _deny(self, state: _SessionState, moment: float, reason: str) -> Decision:
        strikes = self._note_deny(state, moment)
        if strikes >= self.strike_threshold:
            return Decision(KILL, "strikes", strikes, 403)
        return Decision(DENY, reason, strikes, 403)

    def _window(self, state: _SessionState, moment: float) -> int:
        while state.denies and state.denies[0] < moment - self.strike_window_s:
            state.denies.popleft()
        return len(state.denies)

    def _note_deny(self, state: _SessionState, moment: float) -> int:
        self._window(state, moment)
        state.denies.append(moment)
        return len(state.denies)

    def _state(self, session_hash: str, moment: float) -> _SessionState:
        state = self._sessions.get(session_hash)
        if state is None:
            if len(self._sessions) >= self.max_sessions:
                oldest = min(self._sessions, key=lambda key: self._sessions[key].last_seen)
                del self._sessions[oldest]
            state = _SessionState()
            self._sessions[session_hash] = state
        state.last_seen = moment
        return state


def sessions_seen(engine: EgressEngine) -> int:  # pragma: no cover - diagnostics
    with engine._lock:
        return len(engine._sessions)
