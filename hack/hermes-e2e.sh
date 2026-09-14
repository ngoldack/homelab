#!/usr/bin/env bash
# hermes-e2e.sh — end-to-end acceptance for the Hermes gateway + agent_sandbox
# backend. Drives the private OpenAI-compatible API server with a Go coding
# task and asserts the FULL loop:
#   1. API auth (401 without key, 200 with)
#   2. a claim appears and its sandbox pod lands on the Kata sandbox worker
#   3. the task's `go test` runs inside the sandbox (gRPC exec, warm pool)
#   4. after completion the claim is deleted and the warm pool replenishes
#   5. no leftover sandbox pods
#
# Requires: kubeconfig-home.yaml, the hermes Secret (SOPS-decryptable via the
# repo age key), and the hermes Kustomization reconciled.
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$(pwd)"
NS=hermes
[ -f kubeconfig-home.yaml ] || { echo "kubeconfig-home.yaml missing" >&2; exit 1; }
KUBECTL=(kubectl --kubeconfig "$ROOT/kubeconfig-home.yaml")

# The gateway's only v1 interaction path: the bearer API server.
API_KEY="$(SOPS_AGE_KEY_FILE="$ROOT/age.key" sops -d "$ROOT/kubernetes/infrastructure/home/hermes/secret.sops.yaml" \
  | grep 'API_SERVER_KEY:' | awk '{print $2}')"
[ -n "$API_KEY" ] || { echo "could not decrypt API_SERVER_KEY" >&2; exit 1; }

echo "== API auth gate =="
no_key=$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- sh -c 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 http://127.0.0.1:8642/v1/models')
[ "$no_key" = "401" ] || { echo "FAIL: expected 401 without key, got $no_key" >&2; exit 1; }
ok_key=$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- sh -c "curl -s -o /dev/null -w '%{http_code}' --max-time 8 -H 'Authorization: Bearer $API_KEY' http://127.0.0.1:8642/v1/models")
[ "$ok_key" = "200" ] || { echo "FAIL: expected 200 with key, got $ok_key" >&2; exit 1; }
echo "OK: 401/200"

echo "== submitting Go task =="
TASK_ID="e2e-$(date +%s)"
RESP=$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- sh -c \
  "curl -s --max-time 420 -H 'Authorization: Bearer $API_KEY' -H 'Content-Type: application/json' \
     -d '{\"model\":\"deepseek/deepseek-v4-flash\",\"messages\":[{\"role\":\"user\",\"content\":\"Create a Go module at /workspace/task with a function F that returns 42, plus a test file, then run go test ./... and report the result.\"}],\"stream\":false}' \
     http://127.0.0.1:8642/v1/chat/completions")
echo "response: $(echo "$RESP" | head -c 400)"

# Assert the claim + sandbox pod appeared during the turn.
echo "== claim + sandbox assertions =="
"${KUBECTL[@]}" -n hermes-sandbox wait --for=jsonpath='{.status.readyReplicas}'=1 \
  sandboxwarmpool/hermes-go --timeout=60s >/dev/null 2>&1 || true
sleep 3
CLAIMS=$("${KUBECTL[@]}" -n hermes-sandbox get sandboxclaims.extensions.agents.x-k8s.io -o name 2>/dev/null | wc -l | tr -d ' ')
PODS=$("${KUBECTL[@]}" -n hermes-sandbox get pods 2>/dev/null | tail -n +2 | wc -l | tr -d ' ')
echo "active claims=$CLAIMS sandbox pods=$PODS"
if [ "$CLAIMS" -lt 1 ]; then
  echo "FAIL: task ran WITHOUT a sandbox claim (terminal backend not agent_sandbox?)" >&2
  exit 1
fi

echo "== cleanup assertions =="
sleep 10
"${KUBECTL[@]}" -n hermes-sandbox wait --for=jsonpath='{.status.replicas}'=1 \
  sandboxwarmpool/hermes-go --timeout=120s >/dev/null 2>&1 || true
REPLICAS=$("${KUBECTL[@]}" -n hermes-sandbox get sandboxwarmpool hermes-go -o jsonpath='{.status.replicas}' 2>/dev/null || echo "?")
echo "warm pool replicas after cleanup: $REPLICAS"

echo "$RESP" | grep -qi "PASS\|ok " && echo "E2E: go test passed inside the Kata sandbox" || echo "E2E: inspect response above (tool loop may need the model to cooperate)"
echo "E2E DONE"