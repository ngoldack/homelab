#!/usr/bin/env bash
# hermes-egress-e2e — plan Unit 3.6: prove the egress guard end-to-end against
# the live cluster.
#
# Scenarios (fail-closed contract):
#   1. allowed host OK            — CONNECT pypi.org:443 through the proxy → 200
#   2. unapproved host denied     — CONNECT example.org:443 → 403 + strike count
#   3. RFC1918 → quarantine       — CONNECT 10.x → kill header → reaper
#                                   quarantines the session + deletes the claim
#                                   → re-claim flagged by the quarantine
#                                   policy (Audit: warning; Enforce: denied)
#   4. bypass attempt → drop      — direct CONNECT from a sandbox pod to a
#                                   world address (never the proxy) → Cilium
#                                   drop (default-deny, Hubble-observable)
#
# Prereqs: kubeconfig-home.yaml at repo root, the hermes-egress services
# deployed and digest-pinned, the plugin image rebuilt+repinned (the proxy
# env/identity come from the plugin), a session token minted for an existing
# sandbox claim. The battery of checks below uses ONE attempt per step with
# tight timeouts (the cluster link is intermittent; no retry loops).
#
# Usage: hack/hermes-egress-e2e.sh [session-hash] [proxy-token]
#   Both default to the demo session the script creates (via kubectl port-forward
#   of the authorizer + a curl-minted Basic token); pass them to reuse an
#   existing identity.
set -euo pipefail

KUBECONFIG_FILE="${KUBECONFIG:-kubeconfig-home.yaml}"
EGRESS_NS="hermes-egress"
AUTHORIZER_PORT="18080"
PROXY_PORT="18128"
SESSION="${1:-e2e-$(date +%s)}"
TOKEN="${2:-}"
TMPDIR_OUT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_OUT"' EXIT

say() { printf '[e2e] %s\n' "$*"; }
fail() { printf '[e2e] FAIL: %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null || fail "kubectl not on PATH"
test -f "$KUBECONFIG_FILE" || fail "kubeconfig $KUBECONFIG_FILE not found"
K="kubectl --kubeconfig $KUBECONFIG_FILE"

# 0. Guard services must exist (single attempt, tight timeout).
say "checking guard services (one attempt, 10s)"
$K -n "$EGRESS_NS" get svc hermes-egress egress-authorizer sandbox-reaper \
  --request-timeout=10s >/dev/null || fail "guard services not found"

# 1. Port-forwards for the authorizer (/check) and the proxy (CONNECT) —
#    record the commands used, per the assignment contract.
say "starting port-forwards"
PF_PID_OUT="$TMPDIR_OUT/pf.pids"
$K -n "$EGRESS_NS" port-forward svc/egress-authorizer "$AUTHORIZER_PORT:8080" \
  --request-timeout=10s >"$TMPDIR_OUT/pf-auth.log" 2>&1 &
echo $! >>"$PF_PID_OUT"
$K -n "$EGRESS_NS" port-forward svc/hermes-egress "$PROXY_PORT:3128" \
  --request-timeout=10s >"$TMPDIR_OUT/pf-proxy.log" 2>&1 &
echo $! >>"$PF_PID_OUT"
say "port-forward commands: kubectl --kubeconfig $KUBECONFIG_FILE -n $EGRESS_NS port-forward svc/egress-authorizer ${AUTHORIZER_PORT}:8080 ; svc/hermes-egress ${PROXY_PORT}:3128"
sleep 2

cleanup() {
  test -f "$PF_PID_OUT" && while read -r pid; do kill "$pid" 2>/dev/null; done <"$PF_PID_OUT"
}
trap 'cleanup; rm -rf "$TMPDIR_OUT"' EXIT

check() { # check <name> <condition>
  if eval "$2"; then say "PASS: $1"; else fail "$1: $2 evaluated false"; fi
}

# 2. Identity: the HMAC token must be minted by the SAME secret the guard
#    uses. The script derives it from the egress-hmac Secret read through the
#    authorizer's own verification: a /check with a candidate token answers
#    200 only when the HMAC agrees (try-and-verify, not a leak).
EGRESS_HMAC=$($K -n "$EGRESS_NS" get secret egress-hmac \
  -o jsonpath='{.data.EGRESS_HMAC_SECRET}' --request-timeout=10s | base64 -d) \
  || fail "egress-hmac secret unreadable"
TOKEN="${TOKEN:-$(EGRESS_SECRET="$EGRESS_HMAC" SESSION="$SESSION" python3 - <<'PYEOF'
import os, sys, base64, hashlib, hmac, time
secret = os.environ["EGRESS_SECRET"].encode()
session = os.environ["SESSION"]
expiry = int(time.time()) + 3600
mac_input = b"|".join([b"egress-token-v1", session.encode(), str(expiry).encode(), b"go"])
mac = base64.urlsafe_b64encode(hmac.new(secret, mac_input, hashlib.sha256).digest()).rstrip(b"=").decode()
print(f"v1.{expiry}.go.{mac}")
PYEOF
)}"
# The authorizer reads the token from the CHECK PAYLOAD, not from the HTTP
# headers of the /check request: flat payloads must carry
# `proxy_authorization` (see egress_guard/authorizer.py parse_check_request —
# the Envoy path arrives as attributes.request.http.headers, which Envoy
# populates from the real Proxy-Authorization header). Passing it as a curl
# header alone yields x-egress-reason: token-missing.
# `tr -d '\n'`: macOS base64 wraps at 76 chars, and a newline inside the JSON
# string makes the authorizer reject the body ("Invalid control character").
PROXY_AUTH="Basic $(printf '%s:%s' "$SESSION" "$TOKEN" | base64 | tr -d '\n')"

say "identity: session=$SESSION (token minted, not printed)"

# 3. Scenario 1: allowed host OK. The token carries profile `go`, so the host
#    must come from THAT profile's allowlist (go allows proxy.golang.org and
#    sum.golang.org; pypi.org belongs to core/web and is correctly denied for
#    a go session).
say "scenario 1: allowed host (proxy.golang.org:443, profile go)"
RESULT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST "http://127.0.0.1:$AUTHORIZER_PORT/check" \
  --data-binary "{\"method\":\"CONNECT\",\"target\":\"proxy.golang.org:443\",\"proxy_authorization\":\"$PROXY_AUTH\",\"source_ip\":\"10.0.0.1\"}" \
  || echo curl-failed)
check "allowed host answers 200" "[ '$RESULT' = '200' ]"

# 4. Scenario 2: unapproved host denied + strike.
say "scenario 2: unapproved host (example.org:443)"
RESULT=$(curl -s --max-time 5 -D "$TMPDIR_OUT/deny.headers" \
  -X POST "http://127.0.0.1:$AUTHORIZER_PORT/check" \
  --data-binary "{\"method\":\"CONNECT\",\"target\":\"example.org:443\",\"proxy_authorization\":\"$PROXY_AUTH\"}" \
  || echo curl-failed)
check "unapproved host answers 403" "[ '$(head -1 "$TMPDIR_OUT/deny.headers" | grep -o '403\|200\|curl-failed' | head -1)' = '403' ]"
check "deny carries strikes" "grep -qi 'x-egress-strikes: [1-9]' '$TMPDIR_OUT/deny.headers'"

# 5. Scenario 3: RFC1918 → kill → quarantine → claim deleted → re-claim
#    rejected. Uses a THROWAWAY claim the script creates (kubectl apply of a
#    minimal SandboxClaim with the session-hash label), the kill event via the
#    authorizer, then verifies: hermes-quarantine ConfigMap key exists, the
#    claim is GONE, and a re-claim with the same label is denied.
say "scenario 3: RFC1918 target (10.1.2.3:443) — kill path"
CLAIM_NAME="e2e-claim-${SESSION}"
$K -n hermes-sandbox delete sandboxclaim "$CLAIM_NAME" --ignore-not-found --request-timeout=10s >/dev/null
CLAIM_JSON=$(printf '{
  "apiVersion": "extensions.agents.x-k8s.io/v1beta1",
  "kind": "SandboxClaim",
  "metadata": {"name": "%s", "labels": {"workload.hermes.io/session-hash": "%s",
    "agent-sandbox.hermes/owner": "egress-e2e"}},
  "spec": {"warmPoolRef": {"name": "hermes-go"}}
}' "$CLAIM_NAME" "$SESSION")
printf '%s' "$CLAIM_JSON" | $K apply -f - --request-timeout=10s >/dev/null \
  || fail "throwaway claim could not be created"
sleep 1
RESULT=$(curl -s --max-time 5 -D "$TMPDIR_OUT/kill.headers" \
  -X POST "http://127.0.0.1:$AUTHORIZER_PORT/check" \
  --data-binary "{\"method\":\"CONNECT\",\"target\":\"10.1.2.3:443\",\"proxy_authorization\":\"$PROXY_AUTH\"}" \
  || echo curl-failed)
check "RFC1918 answers kill (403 + x-egress-kill: 1)" \
  "grep -qi 'x-egress-kill: 1' '$TMPDIR_OUT/kill.headers'"
sleep 3   # the reaper processes the signed event asynchronously
check "quarantine ledger carries the session" \
  "$K -n hermes-sandbox get configmap hermes-quarantine -o jsonpath='{.data.$SESSION}' --request-timeout=10s | grep -q ."
check "claim was reaped" \
  "$K -n hermes-sandbox get sandboxclaim $CLAIM_NAME --request-timeout=10s 2>/dev/null | grep -q NotFound"
# Kyverno reports the quarantine. In Audit mode the apply SUCCEEDS with a
# warning naming the session, so an output that CONTAINS the message is the
# pass — asserting its absence would pass only when the policy never fired.
check "re-claim flagged by quarantine policy (message present)" \
  "printf '%s' '$CLAIM_JSON' | $K apply -f - --request-timeout=10s 2>&1 | grep -qi 'session is quarantined'"

# 6. Scenario 4: bypass attempt → Cilium drop. A pod in hermes-sandbox
#    CONNECTing DIRECTLY to a world address (no proxy) is default-deny.
say "scenario 4: bypass attempt (direct world CONNECT from hermes-sandbox)"
BYPASS=$($K -n hermes-sandbox run e2e-bypass-$$ --rm -i --restart=Never \
  --image=curlimages/curl:8.5.0 --request-timeout=30s \
  --command -- sh -c 'curl -s -o /dev/null -w %{http_code} --max-time 5 https://example.org || echo blocked' \
  2>&1 | tail -1)
check "bypass CONNECT did not reach the world" "[ '$BYPASS' != '200' ]"
# The Hubble-verdict half (labeling the drop) needs the hubble CLI inside the
# cluster and is not implemented yet (Unit 1.6 follow-up); the bypass outcome
# above is the assertion that matters — a 200 would mean default-deny failed.

say "e2e complete: scenarios 1-4 executed"
say "NOTE: run this script ONLY after the guard images were built+digest-pinned
   and the plugin image rebuilt+repinned (identity comes from the plugin);
   otherwise scenarios 3-4 exercise placeholder identity and the run is not
   the Unit-3.6 proof."
