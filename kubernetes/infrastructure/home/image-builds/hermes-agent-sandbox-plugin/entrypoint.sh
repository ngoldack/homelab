#!/bin/sh
# Entrypoint for the agent_sandbox gateway image.
#
# WHY this wrapper exists: s6-overlay's preinit cannot run as uid 10000 under
# PSA restricted, so the image bypasses s6 and starts the gateway binary
# directly. The gateway container therefore also owns the web dashboard, which
# is backgrounded under a supervision loop so a dashboard crash cannot leave a
# Ready pod with a dead Desktop backend; the container's readiness probe checks
# both listeners (8642 API, 9119 dashboard).
#
# The command line is fixed here on purpose: Kubernetes passes no args and
# anything an operator adds to the pod spec would be silently ignored, so the
# StatefulSet deliberately declares none.
set -eu

# Supervise the dashboard: restart it if it exits, and keep the exit status of
# a failed start from killing the container (the gateway is the critical path).
#
# HERMES_DASHBOARD_ENABLED=false skips it entirely. Set that for profiles that
# have no dashboard surface: the dashboard REFUSES to bind on 0.0.0.0 unless an
# auth provider is registered, so without the dashboard auth env it exits
# instantly and this loop restarts it every ~2s — measured at ~0.5 CPU per pod
# and a log line every couple of seconds, for a listener nothing can reach.
if [ "${HERMES_DASHBOARD_ENABLED:-true}" = "true" ]; then
  while :; do
    /opt/hermes/bin/hermes dashboard --host 0.0.0.0 --port 9119 --no-open || true
    sleep 2
  done &
else
  echo "HERMES_DASHBOARD_ENABLED=false — dashboard not started"
fi

exec /opt/hermes/bin/hermes gateway run
