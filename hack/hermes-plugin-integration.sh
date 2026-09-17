#!/usr/bin/env bash
# Live integration run for the agent_sandbox Hermes plugin.
#
# Requires (all wired by the repo's normal workflows):
#   * kubeconfig-home.yaml in the repo root  (task kubeconfig:home:export)
#   * the sandbox stack deployed (controller, router, hermes-go warm pool)
#   * a Python env with pytest + k8s-agent-sandbox, e.g.
#       cd kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin
#       uv venv /tmp/hermes-plugin-venv && uv pip install -e '.[test]'
#     (or point PYTEST_BIN at any interpreter that has pytest + k8s-agent-sandbox)
#
# Usage:
#   AGENT_SANDBOX_SEED=<hex or base64 seed> hack/hermes-plugin-integration.sh
#
# The seed is the 32-byte Ed25519 signing seed whose public key is in
# agent-sandbox/router-auth-keys.yaml (kid hermes-1). It is also the value of
# AGENT_SANDBOX_ROUTER_TOKEN in the hermes Secret. Both hex and base64 input
# are accepted, but the decoded value must be exactly 32 bytes; the script
# exits 1 with the extraction recipe otherwise. The Secret stores the token
# base64-encoded under `data:`, so the natural extraction is:
#
#   sops -d kubernetes/infrastructure/home/hermes/secret.sops.yaml \
#     | grep AGENT_SANDBOX_ROUTER_TOKEN | awk '{print $2}' \
#     | base64 -d | xxd -p
#
# The script port-forwards the Router service, materializes the seed as a
# scratch token file, and runs pytest -m integration -rs from the plugin
# package. It never `exec`s pytest: every exit path removes the token file
# and the port-forward.
#
# Exec assertions need an in-cluster caller (the sandbox ingress policy admits
# only agent-sandbox-system + hermes on the gRPC port), so they run only with
# AGENT_SANDBOX_IN_CLUSTER=1 — from a hermes pod, e.g.
#   kubectl -n hermes exec hermes-0 -- ... run the plugin tests in-namespace
# AGENT_SANDBOX_IN_CLUSTER=0 is the deliberate non-exec subset (exec tests
# skip, and the skips are printed). Any other value is rejected.

set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$(pwd)"
PLUGIN_DIR="kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin"
ROUTER_NS="agent-sandbox-system"
LOCAL_PORT="${AGENT_SANDBOX_LOCAL_PORT:-8080}"

IN_CLUSTER="${AGENT_SANDBOX_IN_CLUSTER:-1}"
case "$IN_CLUSTER" in
  1) ;;
  0) echo "AGENT_SANDBOX_IN_CLUSTER=0: exec tests will skip (non-exec subset)" >&2 ;;
  *) echo "AGENT_SANDBOX_IN_CLUSTER must be 1 (exec tests) or 0 (non-exec subset), got '$IN_CLUSTER'" >&2
     echo "exec tests need an in-cluster caller; run inside the hermes namespace, e.g." >&2
     echo "  kubectl -n hermes exec hermes-0 -- env AGENT_SANDBOX_IN_CLUSTER=1 ..." >&2
     exit 1 ;;
esac
export AGENT_SANDBOX_IN_CLUSTER="$IN_CLUSTER"

[ -f kubeconfig-home.yaml ] || { echo "kubeconfig-home.yaml missing (task kubeconfig:home:export)" >&2; exit 1; }
SEED_RAW="${AGENT_SANDBOX_SEED:-}"
[ -n "$SEED_RAW" ] || { echo "AGENT_SANDBOX_SEED required (hex or base64 Ed25519 seed)" >&2; exit 1; }
SEED="$(printf '%s' "$SEED_RAW" | tr -d '[:space:]')"

PYTEST_PY="${PYTEST_BIN:-/tmp/hermes-plugin-venv/bin/python}"
if [ ! -x "$PYTEST_PY" ]; then
  echo "pytest interpreter '$PYTEST_PY' is missing or not executable" >&2
  echo "create it: cd $PLUGIN_DIR && uv venv /tmp/hermes-plugin-venv && uv pip install -e '.[test]'" >&2
  echo "or set PYTEST_BIN to an interpreter that has pytest + k8s-agent-sandbox" >&2
  exit 1
fi

TOK_FILE="$(mktemp -t hermes-router-token.XXXXXX)"
PF_PID=""
cleanup() {
  rm -f "$TOK_FILE"
  if [ -n "$PF_PID" ]; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Decode the seed: hex first (the canonical form), then base64 (the Secret's
# `data:` form). Either way it must be exactly 32 bytes.
printf '%s' "$SEED" | xxd -r -p > "$TOK_FILE" 2>/dev/null || : > "$TOK_FILE"
SEED_BYTES="$(wc -c < "$TOK_FILE" | tr -d '[:space:]')"
if [ "$SEED_BYTES" != "32" ]; then
  if printf '%s' "$SEED" | base64 -d > "$TOK_FILE" 2>/dev/null; then
    :
  else
    printf '%s' "$SEED" | base64 -D > "$TOK_FILE" 2>/dev/null || : > "$TOK_FILE"
  fi
  SEED_BYTES="$(wc -c < "$TOK_FILE" | tr -d '[:space:]')"
fi
if [ "$SEED_BYTES" != "32" ]; then
  echo "AGENT_SANDBOX_SEED decoded to $SEED_BYTES bytes, not 32 (hex and base64 both tried)" >&2
  echo "the hermes Secret stores AGENT_SANDBOX_ROUTER_TOKEN base64-encoded under data:;" >&2
  echo "extract the hex form with:" >&2
  echo "  sops -d kubernetes/infrastructure/home/hermes/secret.sops.yaml | grep AGENT_SANDBOX_ROUTER_TOKEN | awk '{print \$2}' | base64 -d | xxd -p" >&2
  exit 1
fi

echo "Starting port-forward to sandbox-router-svc:$LOCAL_PORT ..."
kubectl --kubeconfig kubeconfig-home.yaml -n "$ROUTER_NS" \
  port-forward svc/sandbox-router-svc "$LOCAL_PORT":8080 >/dev/null 2>&1 &
PF_PID=$!

# Probe the endpoint the plugin itself uses (provider._probe_router): GET
# /healthz. 401/403 mean the router is up and enforcing auth.
ROUTER_READY=""
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
    "http://127.0.0.1:$LOCAL_PORT/healthz" 2>/dev/null || true)
  case "$code" in
    200|401|403) echo "router reachable (GET /healthz: HTTP $code)"; ROUTER_READY=1; break ;;
  esac
  sleep 0.5
done
if [ -z "$ROUTER_READY" ]; then
  echo "sandbox-router never answered GET http://127.0.0.1:$LOCAL_PORT/healthz (30 tries, last HTTP '$code')" >&2
  echo "  * did the port-forward bind? kubectl -n $ROUTER_NS get svc/sandbox-router-svc"
  echo "  * is the router up?           kubectl -n $ROUTER_NS get pods -l app.kubernetes.io/name=sandbox-router"
  echo "  * is $LOCAL_PORT already in use? set AGENT_SANDBOX_LOCAL_PORT" >&2
  exit 1
fi

export KUBECONFIG="$ROOT/kubeconfig-home.yaml"
export AGENT_SANDBOX_ROUTER_URL="http://127.0.0.1:$LOCAL_PORT"
export AGENT_SANDBOX_ROUTER_TOKEN_FILE="$TOK_FILE"

cd "$ROOT/$PLUGIN_DIR"
set +e
"$PYTEST_PY" -m pytest -m integration -rs tests/integration -v "$@"
PYTEST_STATUS=$?
set -e
echo "pytest exited with status $PYTEST_STATUS"
exit "$PYTEST_STATUS"
