#!/bin/sh
# Entrypoint for the agent_sandbox image. The gateway container runs
# `args: ["gateway", "run"]` and the web-UI container passes its own argv, so
# this wrapper honours argv (Kubernetes supervises each container separately,
# with its own probes).
#
# s6-overlay stays bypassed (its preinit cannot run under uid 10000 / PSA
# restricted); with no args the gateway is the historical default.
set -eu
if [ "$#" -gt 0 ]; then
  exec /opt/hermes/bin/hermes "$@"
fi
exec /opt/hermes/bin/hermes gateway run
