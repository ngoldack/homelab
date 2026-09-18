"""Deterministic egress guard for Hermes sandbox traffic (plan Units 3.2/3.3).

One image, one package, two services selected by the module path passed as
argv (``egress_guard.authorizer`` | ``egress_guard.reaper``):

* ``authorizer`` — Envoy ext_authz decision point for the CONNECT-capable
  forward proxy (``POST /check``); deterministic rules engine, no network
  calls except the signed event POST to the reaper.
* ``reaper`` — consumes those signed events (``POST /events``), writes the
  ``hermes-quarantine`` ConfigMap and deletes the session's SandboxClaims;
  exports Prometheus text metrics.

Stdlib only: the image installs no dependencies, so every runtime import here
must resolve from CPython 3.12 itself.
"""

__version__ = "0.1.0"
