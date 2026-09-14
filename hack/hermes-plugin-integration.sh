#!/usr/bin/env bash
# Live integration run for the agent_sandbox Hermes plugin.
#
# Requires (all wired by the repo's normal workflows):
#   * kubeconfig-home.yaml in the repo root  (task kubeconfig:home:export)
#   * the sandbox stack deployed (controller, router, hermes-go warm pool)
#   * a Python env with pytest + k8s-agent-sandbox (uv venv recommended)
#
# Usage:
#   AGENT_SANDBOX_SEED=<hex seed> hack/hermes-plugin-integration.sh
#
# The seed is the 32-byte Ed25519 signing seed whose public key is in
# agent-sandbox/router-auth-keys.yaml (kid hermes-1). It is also the value
# of AGENT_SANDBOX_ROUTER_TOKEN in the hermes Secret. Hex or raw accepted.
#
# The script port-forwards the Router service, materializes the seed as a
# scratch token file, and runs pytest -m integration from the plugin package.

set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$(pwd)"
PLUGIN_DIR="kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin"
ROUTER_NS="agent-sandbox-system"
LOCAL_PORT="${AGENT_SANDBOX_LOCAL_PORT:-8080}"

[ -f kubeconfig-home.yaml ] || { echo "kubeconfig-home.yaml missing (task kubeconfig:home:export)" >&2; exit 1; }
SEED_HEX="${AGENT_SANDBOX_SEED:-}"
[ -n "$SEED_HEX" ] || { echo "AGENT_SANDBOX_SEED required (hex Ed25519 seed)" >&2; exit 1; }

TOK_FILE="$(mktemp -t hermes-router-token.XXXXXX)"
trap 'rm -f "$TOK_FILE"; kill ${PF_PID:-} 2>/dev/null || true' EXIT
printf '%s' "$SEED_HEX" | xxd -r -p > "$TOK_FILE"

echo "Starting port-forward to sandbox-router-svc:$LOCAL_PORT ..."
kubectl --kubeconfig kubeconfig-home.yaml -n "$ROUTER_NS" \
  port-forward svc/sandbox-router-svc "$LOCAL_PORT":8080 >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 -X POST \
    "http://127.0.0.1:$LOCAL_PORT/execute" 2>/dev/null || true)
  case "$code" in
    400|401|403) echo "router reachable (auth gate: HTTP $code)"; break ;;
  esac
  sleep 0.5
done

export KUBECONFIG="$ROOT/kubeconfig-home.yaml"
export AGENT_SANDBOX_ROUTER_URL="http://127.0.0.1:$LOCAL_PORT"
export AGENT_SANDBOX_ROUTER_TOKEN_FILE="$TOK_FILE"

cd "$ROOT/$PLUGIN_DIR"
exec "${PYTEST_BIN:-/tmp/hermes-plugin-venv/bin/python}" -m pytest -m integration tests/integration -v "$@"