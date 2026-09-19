#!/usr/bin/env bash
# flux-wait-kustomizations.sh — wait for a Flux reconcile with VISIBILITY.
#
# The old reconcile workflow used bare `kubectl wait --for=jsonpath=...`
# steps: they print nothing until the predicate passes or the timeout fires,
# so a stuck dependency or a dry-run failure just hangs the workflow for
# 15 minutes with zero signal. This script polls instead and prints, every
# poll, WHICH kustomizations have not yet converged and the latest message on
# each one's Ready condition — the message is where Flux says what it is
# waiting for ("DependencyNotReady: dependency 'x' is not ready",
# "dry-run failed: field is immutable", ...).
#
# Two phases, matching the old workflow's two gates:
#   1. revision gate: every Kustomization's status.lastAppliedRevision must
#      equal the target revision (a bare Ready check can pass on a revision
#      that has not been applied anywhere yet).
#   2. ready gate: every Kustomization's Ready condition is True.
#
# Usage: flux-wait-kustomizations.sh <target-revision> [max-wait-seconds]
#   <target-revision>   e.g. main@sha1:261f7618ca66800f73ca477bc5219a60316107aa
#   max-wait            default 540 (9 min per phase; keep 2 phases < the
#                       workflow's 20-min job timeout)
#
# Requires: kubectl + jq (both present in the flux-cli runner image), and a
# kubeconfig pointing at the cluster (the workflow writes it earlier).
set -euo pipefail

TARGET_REV="${1:?target revision required, e.g. main@sha1:...}"
MAX_WAIT="${2:-540}"
POLL=6
NS=flux-system
PHASES=(revision ready)
now_ts() { date -u +%H:%M:%S; }

json_snapshot() {
  kubectl -n "$NS" get kustomizations -o json 2>/dev/null
}

revision_laggards() {
  # name <TAB> applied <TAB> first Ready-condition message (or no-status marker)
  jq -r --arg rev "$TARGET_REV" '
    [ .items[] | select(.status.lastAppliedRevision? != $rev) ]
    | map([
        .metadata.name,
        (.status.lastAppliedRevision? // "NOT-APPLIED"),
        ((.status.conditions[]? | select(.type == "Ready") | .message) // "no Ready condition yet")
      ])
    | .[] | @tsv' <<<"$1" 2>/dev/null || true
}

ready_laggards() {
  # name <TAB> ready <TAB> message
  jq -r '
    [ .items[] | select(((.status.conditions[]? | select(.type == "Ready") | .status) // "False") != "True") ]
    | map([
        .metadata.name,
        ((.status.conditions[]? | select(.type == "Ready") | .status) // "Unknown"),
        ((.status.conditions[]? | select(.type == "Ready") | .message) // "no Ready condition yet")
      ])
    | .[] | @tsv' <<<"$1" 2>/dev/null || true
}

phase_done() {
  local phase="$1" payload="$2" lines
  if [ "$phase" = revision ]; then
    lines=$(revision_laggards "$payload")
  else
    lines=$(ready_laggards "$payload")
  fi
  [ -z "$lines" ]
}

log_laggards() {
  local phase="$1" payload="$2" lines
  if [ "$phase" = revision ]; then
    lines=$(revision_laggards "$payload")
  else
    lines=$(ready_laggards "$payload")
  fi
  [ -z "$lines" ] && return 0
  printf '[%s] %s gate: still waiting on:\n' "$(now_ts)" "$phase"
  printf '%s\n' "$lines" | while IFS=$'\t' read -r name applied msg; do
    printf '   %-32s %-22s %s\n' "$name" "$applied" "$(printf '%s' "$msg" | head -c 200)"
  done
}

# Permanent vs transient: a dry-run failure is a declarative error in the
# manifest itself (schema-invalid, immutable field, illegal value) — Flux will
# NOT recover from it by itself and no amount of polling fixes it. Exit at
# once so the workflow fails on the first sighting instead of burning the
# whole wait budget on a known-lost cause. Transient states (dependency not
# ready, drift detection running) still wait.
fatal_error() {
  local payload="$1" msg
  msg=$(printf '%s' "$payload" | jq -r '
    [ .items[].status.conditions[]? | select(.message? | contains("dry-run failed")) | .message ] | .[]' \
    | head -1)
  [ -n "$msg" ]
}

for phase in "${PHASES[@]}"; do
  start=$SECONDS
  echo "[$(now_ts)] phase: $phase gate (target $TARGET_REV, max ${MAX_WAIT}s)"
  while true; do
    payload=$(json_snapshot) || { echo "[$(now_ts)] ERROR: kubectl get kustomizations failed"; exit 1; }
    if fatal_error "$payload"; then
      echo "[$(now_ts)] FATAL: a Kustomization reports a dry-run failure (declarative error, will not recover by itself)"
      log_laggards "$phase" "$payload"
      echo "--- final kustomization state ---"
      flux get kustomizations -n "$NS" 2>/dev/null || kubectl -n "$NS" get kustomizations
      exit 1
    fi
    if phase_done "$phase" "$payload"; then
      echo "[$(now_ts)] $phase gate satisfied"
      break
    fi
    log_laggards "$phase" "$payload"
    if [ $((SECONDS - start)) -ge "$MAX_WAIT" ]; then
      echo "[$(now_ts)] ERROR: $phase gate timed out after ${MAX_WAIT}s"
      echo "--- final kustomization state ---"
      flux get kustomizations -n "$NS" 2>/dev/null || kubectl -n "$NS" get kustomizations
      exit 1
    fi
    sleep "$POLL"
  done
done

echo "[$(now_ts)] all kustomizations converged to $TARGET_REV and Ready"