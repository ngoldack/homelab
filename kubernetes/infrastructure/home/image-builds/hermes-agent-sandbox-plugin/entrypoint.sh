#!/bin/sh
# Gateway + dashboard supervisor for the agent_sandbox image.
#
# s6-overlay was bypassed (its preinit cannot run under uid 10000 / PSA
# restricted), so this wrapper runs both surfaces of the Hermes install:
#   * `hermes gateway run` — messaging gateway + OpenAI API server (8642)
#   * `hermes dashboard`  — web dashboard, and the remote backend that the
#     Hermes Desktop app connects to (9119, username/password auth, bound
#     to 0.0.0.0 so the operator's port-forward peer is admitted)
# The dashboard is backgrounded; the gateway owns the foreground signal and
# Kubernetes restartPolicy supervises this wrapper (both die together).
sleep 1
/opt/hermes/bin/hermes dashboard --host 0.0.0.0 --port 9119 --no-open &
exec /opt/hermes/bin/hermes gateway run