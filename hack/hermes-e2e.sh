#!/usr/bin/env bash
# hermes-e2e.sh — end-to-end acceptance for the Hermes gateway + agent_sandbox
# backend. Drives the private OpenAI-compatible API server with a Go coding
# task and asserts the FULL loop:
#   1. API auth gate: 401 without the bearer key, 200 with it
#   2. pre-flight: hermes-sandbox holds no stray SandboxClaims before the run
#   3. the chat completion returns HTTP 200 (model id resolves, API path works)
#   4. a SandboxClaim labeled agent-sandbox.hermes/owner=<gateway id> appears
#      DURING the turn (a local terminal backend would leave no claim)
#   5. the adopted sandbox pod runs runtimeClassName=kata on the node labeled
#      workload.hermes.io/sandbox=true
#   6. in-guest evidence: the response carries the literal sentinel
#      E2E_GUEST_MARKER and a kernel release other than the host kernel
#      (a host kernel means the command ran outside the Kata guest)
#   7. cleanup: the warm pool returns to status.replicas=1, no non-pool sandbox
#      pods and no gateway-owned claims are left behind
#   8. backend identity: the agent_sandbox plugin is loaded and
#      terminal.backend == agent_sandbox
#
# Requires: kubeconfig-home.yaml, the hermes Secret (SOPS-decryptable via the
# repo age key), and the hermes Kustomization reconciled.
# Env overrides: HERMES_E2E_MODEL (default: local), HERMES_E2E_OWNER (default:
# AGENT_SANDBOX_GATEWAY_ID of pod hermes-0, else hermes-0),
# HERMES_E2E_POLL_TRIES / HERMES_E2E_POLL_SLEEP (poll bound; default 60 x 2 s),
# HERMES_E2E_HOST_KERNEL (default: 6.18.34-talos).
set -euo pipefail
cd "$(dirname "$0")/.."

ROOT="$(pwd)"
NS=hermes
SANDBOX_NS=hermes-sandbox
POOL=hermes-go
CLAIM=sandboxclaim
NODE_SELECTOR=workload.hermes.io/sandbox=true
MODEL="${HERMES_E2E_MODEL:-local}"
SENTINEL=E2E_GUEST_MARKER
HOST_KERNEL="${HERMES_E2E_HOST_KERNEL:-6.18.34-talos}"
POLL_TRIES="${HERMES_E2E_POLL_TRIES:-60}"
POLL_SLEEP="${HERMES_E2E_POLL_SLEEP:-2}"
KUBECTL=(kubectl --kubeconfig "$ROOT/kubeconfig-home.yaml")
TMP_DIR="$(mktemp -d)"
REQUEST_FILE="$TMP_DIR/request.json"
RESPONSE_FILE="$TMP_DIR/response.txt"
CURL_RC_FILE="$TMP_DIR/curl.rc"
CURL_PID=""

# Claim ownership: the plugin tags owned claims with
# agent-sandbox.hermes/owner = AGENT_SANDBOX_GATEWAY_ID (pod hostname when the
# env var is unset).
OWNER_LABEL="agent-sandbox.hermes/owner=hermes-0"

cleanup_claims() {
  rc=$?
  trap - EXIT INT TERM
  set +e
  if [ -n "$CURL_PID" ]; then kill "$CURL_PID" 2>/dev/null; fi
  "${KUBECTL[@]}" -n "$SANDBOX_NS" delete "$CLAIM" -l "$OWNER_LABEL" --ignore-not-found >/dev/null 2>&1
  rm -rf "$TMP_DIR"
  exit "$rc"
}
trap cleanup_claims EXIT INT TERM

dump_sandbox_state() {
  set +e
  echo "--- sandboxwarmpool/$POOL ---" >&2
  "${KUBECTL[@]}" -n "$SANDBOX_NS" get sandboxwarmpool "$POOL" -o yaml >&2
  echo "--- sandboxclaims ---" >&2
  "${KUBECTL[@]}" -n "$SANDBOX_NS" get "$CLAIM" -o wide >&2
  echo "--- pods ---" >&2
  "${KUBECTL[@]}" -n "$SANDBOX_NS" get pods -o wide >&2
}

[ -f kubeconfig-home.yaml ] || { echo "kubeconfig-home.yaml missing" >&2; exit 1; }

# The gateway's only v1 interaction path: the bearer API server.
API_KEY="$(SOPS_AGE_KEY_FILE="$ROOT/age.key" sops -d "$ROOT/kubernetes/infrastructure/home/hermes/secret.sops.yaml" \
  | grep 'API_SERVER_KEY:' | awk '{print $2}')"
[ -n "$API_KEY" ] || { echo "could not decrypt API_SERVER_KEY" >&2; exit 1; }

if [ -z "${HERMES_E2E_OWNER:-}" ]; then
  owner_env=""
  if ! owner_env="$("${KUBECTL[@]}" -n "$NS" get pod hermes-0 \
      -o jsonpath='{.spec.containers[?(@.name=="hermes")].env[?(@.name=="AGENT_SANDBOX_GATEWAY_ID")].value}' 2>/dev/null)"; then
    owner_env=""
  fi
  if [ -n "$owner_env" ]; then OWNER_LABEL="agent-sandbox.hermes/owner=$owner_env"; fi
fi

echo "== pre-flight: no stray sandbox claims =="
STRAYS=""
if ! STRAYS="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get "$CLAIM" -o name)"; then
  echo "FAIL: could not list $CLAIM in $SANDBOX_NS" >&2
  exit 1
fi
if [ -n "$STRAYS" ]; then
  echo "FAIL: $SANDBOX_NS already holds sandbox claims:" >&2
  printf '%s\n' "$STRAYS" >&2
  echo "  delete them before rerunning: kubectl -n $SANDBOX_NS delete $CLAIM --all" >&2
  exit 1
fi
echo "OK: no claims in $SANDBOX_NS"

echo "== API auth gate =="
no_key=$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- sh -c 'curl -s -o /dev/null -w "%{http_code}" --max-time 5 http://127.0.0.1:8642/v1/models')
[ "$no_key" = "401" ] || { echo "FAIL: expected 401 without key, got $no_key" >&2; exit 1; }
ok_key=$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- env "API_KEY=$API_KEY" sh -c \
  'curl -s -o /dev/null -w "%{http_code}" --max-time 8 -H "Authorization: Bearer $API_KEY" http://127.0.0.1:8642/v1/models')
[ "$ok_key" = "200" ] || { echo "FAIL: expected 200 with key, got $ok_key" >&2; exit 1; }
echo "OK: 401/200"

echo "== submitting Go task (model: $MODEL) =="
PROMPT="Create a Go module at /workspace/task with a function F that returns 42 and a test for it, then run go test ./... inside that directory. After that run uname -r and report its exact output followed by the literal sentinel E2E_GUEST_MARKER, verbatim, on the same line."
printf '{"model":"%s","messages":[{"role":"user","content":"%s"}],"stream":false}' "$MODEL" "$PROMPT" > "$REQUEST_FILE"

(
  set +e
  "${KUBECTL[@]}" -n "$NS" exec -i hermes-0 -- env "API_KEY=$API_KEY" sh -c \
    'curl -s --max-time 420 -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" -d @- -w "\n%{http_code}" http://127.0.0.1:8642/v1/chat/completions' \
    < "$REQUEST_FILE" > "$RESPONSE_FILE"
  echo $? > "$CURL_RC_FILE"
) &
CURL_PID=$!

echo "== claim + placement evidence (polling during the turn) =="
CLAIM_HIT=""
SANDBOX_POD=""
SANDBOX_RUNTIME=""
SANDBOX_NODE=""
for ((i = 0; i < POLL_TRIES; i++)); do
  claim_names=""
  if ! claim_names="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get "$CLAIM" -l "$OWNER_LABEL" -o name 2>/dev/null)"; then
    claim_names=""
  fi
  if [ -n "$claim_names" ]; then
    CLAIM_HIT="${claim_names%%$'\n'*}"
    sandbox_name=""
    if ! sandbox_name="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get "$CLAIM" "${CLAIM_HIT##*/}" \
        -o jsonpath='{.status.sandbox.name}' 2>/dev/null)"; then
      sandbox_name=""
    fi
    if [ -n "$sandbox_name" ]; then
      sandbox_selector=""
      if ! sandbox_selector="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get sandbox "$sandbox_name" \
          -o jsonpath='{.status.selector}' 2>/dev/null)"; then
        sandbox_selector=""
      fi
      if [ -n "$sandbox_selector" ]; then
        pod_info=""
        if ! pod_info="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get pods -l "$sandbox_selector" \
            -o jsonpath='{.items[0].metadata.name}{"|"}{.items[0].spec.runtimeClassName}{"|"}{.items[0].spec.nodeName}' 2>/dev/null)"; then
          pod_info=""
        fi
        if [ -n "$pod_info" ]; then
          IFS='|' read -r SANDBOX_POD SANDBOX_RUNTIME SANDBOX_NODE <<< "$pod_info"
          if [ -n "$SANDBOX_POD" ]; then break; fi
        fi
      fi
    fi
  fi
  if [ -f "$CURL_RC_FILE" ] && [ -z "$CLAIM_HIT" ]; then
    break
  fi
  sleep "$POLL_SLEEP"
done

if [ -z "$CLAIM_HIT" ]; then
  echo "FAIL: no sandbox claim labeled $OWNER_LABEL appeared during the turn." >&2
  echo "      The gateway most likely ran the task locally (terminal.backend is not" >&2
  echo "      agent_sandbox) or the plugin never created the claim." >&2
  dump_sandbox_state
  exit 1
fi
if [ -z "$SANDBOX_POD" ]; then
  echo "FAIL: claim $CLAIM_HIT appeared but no adopted sandbox pod could be resolved within ${POLL_TRIES}x${POLL_SLEEP}s" >&2
  dump_sandbox_state
  exit 1
fi
echo "OK: claim $CLAIM_HIT -> pod $SANDBOX_POD"

wait "$CURL_PID" || {
  echo "FAIL: chat completion transport failed (kubectl exec/curl)" >&2
  if [ -s "$RESPONSE_FILE" ]; then cat "$RESPONSE_FILE" >&2; fi
  exit 1
}
CURL_RC=""
if ! CURL_RC="$(cat "$CURL_RC_FILE" 2>/dev/null)"; then CURL_RC=""; fi
if [ "$CURL_RC" != "0" ]; then
  echo "FAIL: curl exited $CURL_RC while calling the chat completions API" >&2
  if [ -s "$RESPONSE_FILE" ]; then cat "$RESPONSE_FILE" >&2; fi
  exit 1
fi

RESP_RAW="$(cat "$RESPONSE_FILE")"
if [[ "$RESP_RAW" == *$'\n'* ]]; then
  STATUS="${RESP_RAW##*$'\n'}"
  BODY="${RESP_RAW%$'\n'*}"
else
  STATUS=""
  BODY="$RESP_RAW"
fi
STATUS="$(printf '%s' "$STATUS" | tr -d '[:space:]')"
if [ "$STATUS" != "200" ]; then
  echo "FAIL: chat completion returned HTTP ${STATUS:-<none>} (model: $MODEL)" >&2
  printf '%s\n' "$BODY" >&2
  exit 1
fi
echo "response (first 400 bytes): $(printf '%s' "$BODY" | head -c 400)"

echo "== placement =="
if [ "$SANDBOX_RUNTIME" != "kata" ]; then
  echo "FAIL: adopted sandbox pod $SANDBOX_POD has runtimeClassName='$SANDBOX_RUNTIME' (expected 'kata')" >&2
  exit 1
fi
EXPECTED_NODES=""
if ! EXPECTED_NODES="$("${KUBECTL[@]}" get nodes -l "$NODE_SELECTOR" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')"; then
  echo "FAIL: could not list nodes labeled $NODE_SELECTOR" >&2
  exit 1
fi
EXPECTED_NODES="$(printf '%s' "$EXPECTED_NODES" | tr '\n' ' ')"
EXPECTED_NODES="${EXPECTED_NODES% }"
if [ -z "$EXPECTED_NODES" ]; then
  echo "FAIL: no node carries the label $NODE_SELECTOR — sandbox placement is broken" >&2
  exit 1
fi
case " $EXPECTED_NODES " in
  *" $SANDBOX_NODE "*) ;;
  *)
    echo "FAIL: adopted sandbox pod $SANDBOX_POD runs on node '$SANDBOX_NODE' (expected one of: $EXPECTED_NODES)" >&2
    exit 1
    ;;
esac
echo "OK: $SANDBOX_POD runtimeClassName=kata on $SANDBOX_NODE"

echo "== in-guest evidence =="
if ! printf '%s' "$BODY" | grep -qF "$SENTINEL"; then
  echo "FAIL: response does not contain the sentinel $SENTINEL" >&2
  printf '%s\n' "$BODY" >&2
  exit 1
fi
GUEST_KERNEL="$(printf '%s' "$BODY" | awk -v host="$HOST_KERNEL" -v sent="$SENTINEL" '
  { s = $0
    while (match(s, /[0-9]+\.[0-9]+\.[0-9]+[-A-Za-z0-9._+]*/)) {
      k = substr(s, RSTART, RLENGTH); s = substr(s, RSTART + RLENGTH)
      if (k != host) {
        if (index($0, sent) > 0) { print k; found = 1; exit }
        if (fallback == "") { fallback = k }
      }
    }
  }
  END { if (!found && fallback != "") { print fallback } }')"
if [ -z "$GUEST_KERNEL" ]; then
  echo "FAIL: response reports no kernel release other than $HOST_KERNEL — the command ran outside the Kata guest" >&2
  printf '%s\n' "$BODY" >&2
  exit 1
fi
echo "OK: sentinel present, guest kernel $GUEST_KERNEL (host: $HOST_KERNEL)"

echo "== cleanup =="
pool_replicas=""
for ((i = 0; i < POLL_TRIES; i++)); do
  if ! pool_replicas="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get sandboxwarmpool "$POOL" \
      -o jsonpath='{.status.replicas}' 2>/dev/null)"; then
    pool_replicas=""
  fi
  if [ "$pool_replicas" = "1" ]; then break; fi
  sleep "$POLL_SLEEP"
done
if [ "$pool_replicas" != "1" ]; then
  echo "FAIL: sandboxwarmpool/$POOL did not return to status.replicas=1 (last seen: '${pool_replicas:-<none>}')" >&2
  dump_sandbox_state
  exit 1
fi

stray_pods=""
for ((i = 0; i < POLL_TRIES; i++)); do
  pod_list=""
  if ! pod_list="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get pods -o name 2>/dev/null)"; then
    pod_list=""
  fi
  stray_pods=""
  while IFS= read -r pod; do
    case "$pod" in
      ""|"pod/$POOL"-*) ;;
      *) stray_pods="$stray_pods $pod" ;;
    esac
  done <<< "$pod_list"
  if [ -z "$stray_pods" ]; then break; fi
  sleep "$POLL_SLEEP"
done
if [ -n "$stray_pods" ]; then
  echo "FAIL: non-pool sandbox pods left behind:$stray_pods" >&2
  dump_sandbox_state
  exit 1
fi

leftover_claims=""
for ((i = 0; i < POLL_TRIES; i++)); do
  if ! leftover_claims="$("${KUBECTL[@]}" -n "$SANDBOX_NS" get "$CLAIM" -l "$OWNER_LABEL" -o name 2>/dev/null)"; then
    leftover_claims=""
  fi
  if [ -z "$leftover_claims" ]; then break; fi
  sleep "$POLL_SLEEP"
done
if [ -n "$leftover_claims" ]; then
  echo "FAIL: sandbox claims left behind:" >&2
  printf '%s\n' "$leftover_claims" >&2
  dump_sandbox_state
  exit 1
fi
echo "OK: pool replenished, no stray pods, no owned claims"

echo "== backend identity =="
plugins_out=""
if ! plugins_out="$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- hermes plugins list 2>&1)"; then
  echo "FAIL: 'hermes plugins list' failed:" >&2
  printf '%s\n' "$plugins_out" >&2
  exit 1
fi
if ! printf '%s\n' "$plugins_out" | grep -q 'agent_sandbox'; then
  echo "FAIL: the agent_sandbox plugin is not listed by 'hermes plugins list':" >&2
  printf '%s\n' "$plugins_out" >&2
  exit 1
fi
backend_out=""
if ! backend_out="$("${KUBECTL[@]}" -n "$NS" exec hermes-0 -- hermes config get terminal.backend 2>&1)"; then
  echo "FAIL: 'hermes config get terminal.backend' failed:" >&2
  printf '%s\n' "$backend_out" >&2
  exit 1
fi
backend_trimmed="$(printf '%s' "$backend_out" | tr -d '[:space:]')"
if [ "$backend_trimmed" != "agent_sandbox" ]; then
  echo "FAIL: terminal.backend is '$backend_out' (expected agent_sandbox)" >&2
  exit 1
fi
echo "OK: agent_sandbox plugin loaded, terminal.backend=agent_sandbox"

echo "E2E PASS"
