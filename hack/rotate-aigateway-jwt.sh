#!/usr/bin/env bash
# Rotates the shared aigateway-oidc client_credentials JWT for every in-cluster
# consumer that cannot mint its own tokens (hermes OPENAI_API_KEY, hindsight
# HINDSIGHT_API_LLM_API_KEY). Authentik's provider caps access tokens at 30
# days (seed.yaml: access_token_validity hours=720), so without this cron the
# two services stop authenticating against the agentgateway at expiry —
# both at once, since they share the token.
#
# Schedule: bi-weekly (every 2 weeks). LaunchAgent installed by
# `hack/rotate-aigateway-jwt.sh install` runs it every 14 days at 03:00 local
# time. A 14-day cadence halves the maximum staleness vs. the 30-day TTL and
# keeps rotation far away from the cliff.
#
# Flow per run:
#   1. Mint a fresh token: client_credentials grant against authentik using
#      client_id aigateway-oidc + the client secret from the sops-managed
#      authentik secret (git-decrypted with the worktree's age key).
#   2. Verify the token is a real JWT and actually accepted by the gateway
#      (a completion POST through /v1/synthetic-small) BEFORE touching git.
#      A bad token must never reach the secrets.
#   3. sops-set the new value into both secret files in the worktree.
#   4. Commit and push to main (push = deploy), wait for Flux to apply the
#      new secret in-cluster, then rollout-restart the consumer controllers —
#      pods read env at start and nothing else reloads them.
#
# Requirements: the worktree's age.key must decrypt the sops files.
set -euo pipefail

REPO_DIR="/Users/ngoldack/.omp/wt/connectionproblems-09de0f9"
cd "$REPO_DIR"
export SOPS_AGE_KEY_FILE="$PWD/age.key"
KUBECONFIG="$PWD/kubeconfig-home.yaml"
export KUBECONFIG

AIGATEWAY_CLIENT_SECRET_FILE="kubernetes/infrastructure/home/authentik/oidc-aigateway.sops.yaml"
TOKEN_URL="https://authentik.ngoldack.de/application/o/token/"
GATEWAY_BASE="http://agentgateway.agentgateway.svc.cluster.local:80"
HERMES_SECRET="kubernetes/infrastructure/home/hermes/secret.sops.yaml"
HINDSIGHT_SECRET="kubernetes/infrastructure/home/hindsight/secret.sops.yaml"

# Namespace/name pairs of the consumers' controllers. A secret-only change
# moves nothing: pods read env at start, and no reloader/checksum mechanism
# exists in this repo — so after Flux applies the new secret, the pods MUST
# be restarted to pick it up. The script polls for the applied secret before
# restarting (a restart issued earlier would just capture the old token).
HERMES_ROLLOUT_TARGET="statefulset.apps/hermes"
HINDSIGHT_ROLLOUT_TARGETS="deployment.apps/hindsight-api statefulset.apps/hindsight-worker"


# ---- launchd (macOS) -------------------------------------------------------
# `hack/rotate-aigateway-jwt.sh install` installs a LaunchAgent running this
# script every 14 days at 03:00. Must come before the rotation flow so that
# `install` exits without running a rotation.
# `hack/rotate-aigateway-jwt.sh install` installs a LaunchAgent running this
# script every 14 days at 03:00 and kicks off the first run.
install_agent() {
  local label="de.ngoldack.rotate-aigateway-jwt"
  local plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array>
    <string>/bin/bash</string><string>$PWD/$(basename "$0")</string>
  </array>
  <key>StartCalendarInterval</key><dict>
    <key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer>
  </dict>
  <key>StartInterval</key><integer>$((14*24*3600))</integer>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/$label.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/$label.log</string>
</dict></plist>
PLIST
  launchctl unload "$plist" 2>/dev/null || true
  launchctl load "$plist"
  echo "installed $plist (every 14 days at 03:00)"
}

case "${1:-}" in
  install) install_agent; exit 0 ;;
esac

log() { echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"; }


# ---- 1. Client secret ------------------------------------------------------
# Source of truth is the in-cluster secret the seed created — decrypting the
# sops file needs multi-line YAML parsing (the sed one-liner grabs the whole
# document), and kubectl is simpler and authoritative.
log "reading aigateway client secret from the cluster"
CLIENT_SECRET=$(kubectl -n authentik get secret authentik-oidc-aigateway -o jsonpath='{.data.client_secret}' | base64 -d)
[ -n "$CLIENT_SECRET" ] || { log "ERROR: empty client secret"; exit 1; }

# ---- 2. Mint a token --------------------------------------------------------
# Runs from the Mac; authentik's edge vhost is reachable from here. If the
# Mac ever loses edge access, the mint can move in-cluster (cronjob) — the
# verify step below would need to run from a pod either way.
log "minting aigateway-oidc client_credentials token"
JWT=$(curl -sS --max-time 30 -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" \
  -d "client_id=aigateway-oidc" \
  -d "client_secret=$CLIENT_SECRET" \
  -d "scope=openid" | python3 -c "import sys,json; print(json.load(sys.stdin)['access_token'])") || {
  log "ERROR: token mint failed"; exit 1; }

echo "$JWT" | grep -qE '^eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$' || {
  log "ERROR: minted token is not a JWT"; exit 1; }

# ---- 3. Verify against the gateway BEFORE touching git ---------------------
# In-cluster probe (the Mac cannot reach the LB IP); a hindsight pod serves
# as the vantage point — same Cilium path hermes/hindsight actually use.
log "verifying token against the gateway from a hindsight pod"
STATUS=$(kubectl -n hindsight exec pod/hindsight-worker-0 -- env PROBE_TOKEN="$JWT" python3 -c "
import os, urllib.request, json
req = urllib.request.Request(
    '$GATEWAY_BASE/v1/synthetic-small/chat/completions',
    data=json.dumps({'messages':[{'role':'user','content':'hi'}],'max_tokens':1}).encode(),
    headers={'Authorization': 'Bearer ' + os.environ['PROBE_TOKEN'],
             'Content-Type': 'application/json'})
try:
    r = urllib.request.urlopen(req, timeout=60)
    print(r.status)
except Exception as e:
    print('ERROR', getattr(e, 'code', ''), type(e).__name__, e)
") || true
echo "$STATUS" | grep -q "^200$" || { log "ERROR: gateway verification failed: $STATUS"; exit 1; }
log "gateway verification passed (HTTP 200)"

# ---- 4. Roll into both sops secrets ----------------------------------------
log "writing token into $HERMES_SECRET and $HINDSIGHT_SECRET"
sops set "$HERMES_SECRET" '["stringData"]["OPENAI_API_KEY"]' "\"$JWT\""
sops set "$HINDSIGHT_SECRET" '["stringData"]["HINDSIGHT_API_LLM_API_KEY"]' "\"$JWT\""
# ---- 5. Commit and push (push = deploy) ------------------------------------
# Only the two secret files — the worktree may carry unrelated changes.
git add "$HERMES_SECRET" "$HINDSIGHT_SECRET"
if git diff --cached --quiet; then
  log "no changes to commit (token unchanged?)"
  exit 0
fi
git commit -m "chore(secrets): rotate shared aigateway-oidc JWT (2-week rotation)"
git push origin HEAD:main

# ---- 6. Wait for Flux to apply, then restart the consumers -----------------
# A restart issued before the secret lands would just capture the old token.
# Compare the FULL token, not a prefix: every JWT from the same provider
# shares the eyJhbG header, so a 6-char match also matches the stale value.
log "waiting for Flux to apply the new secrets"
for i in $(seq 1 60); do
  IN_HERMES=$(kubectl -n hermes get secret hermes-secret -o jsonpath='{.data.OPENAI_API_KEY}' | base64 -d)
  IN_HINDSIGHT=$(kubectl -n hindsight get secret hindsight-env -o jsonpath='{.data.HINDSIGHT_API_LLM_API_KEY}' | base64 -d)
  [ "$IN_HERMES" = "$JWT" ] && [ "$IN_HINDSIGHT" = "$JWT" ] && break
  [ "$i" = 60 ] && { log "ERROR: secrets not updated after 10m"; exit 1; }
  sleep 10
done
log "secrets applied; restarting consumers"
kubectl -n hermes rollout restart "$HERMES_ROLLOUT_TARGET"
for target in $HINDSIGHT_ROLLOUT_TARGETS; do
  kubectl -n hindsight rollout restart "$target"
done
kubectl -n hermes rollout status "$HERMES_ROLLOUT_TARGET" --timeout=300s || log "WARN: hermes rollout did not complete in 5m"
for target in $HINDSIGHT_ROLLOUT_TARGETS; do
  kubectl -n hindsight rollout status "$target" --timeout=300s || log "WARN: $target rollout did not complete in 5m"
done
log "rotation complete"
