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
  # SNAPSHOT_FILE is a test seam: point the gates at a captured payload instead
  # of the live cluster so the reporting logic can be exercised offline.
  if [ -n "${SNAPSHOT_FILE:-}" ]; then cat "$SNAPSHOT_FILE"; return 0; fi
  kubectl -n "$NS" get kustomizations -o json 2>/dev/null
}

suspended_names() {
  # Kustomizations deliberately paused in-cluster (spec.suspend). A suspended
  # Kustomization never applies a new revision, so the revision gate can never
  # pass and its dependents sit on "dependency ... revision is not up to date".
  # That is an operator maintenance window, not a failing merge — on 2026-09-22
  # it held this gate red for hours while `matrix` was suspended, and the log
  # gave no hint that the cause was a pause rather than a broken manifest.
  jq -r '.items[] | select(.spec.suspend == true) | .metadata.name' <<<"$1" 2>/dev/null || true
}

source_rev() {
  # SNAPSHOT_SOURCE_REV is a test seam, matching SNAPSHOT_FILE.
  if [ -n "${SNAPSHOT_SOURCE_REV:-}" ]; then printf '%s' "$SNAPSHOT_SOURCE_REV"; return 0; fi
  kubectl -n "$NS" get gitrepository flux-system -o jsonpath='{.status.artifact.revision}' 2>/dev/null || true
}

report_superseded() {
  # A run pins the revision it must see applied everywhere. If main advances
  # while the run is in flight — routine on this repo, where several merges can
  # land minutes apart — every Kustomization moves to the NEWER revision, the
  # pinned one can never be applied again, and the run burns its whole budget
  # before failing. Observed 2026-09-22: run 35714299454 asserted cd60fcd while
  # the cluster had already moved to 178abfcf (a later merge), and its log read
  # like a deploy failure. Say what actually happened.
  local payload="$1" current applied
  current=$(source_rev)
  [ -n "$current" ] || return 0
  [ "$current" = "$TARGET_REV" ] && return 0
  applied=$(revision_laggards "$payload" | cut -f2 | sort -u)
  [ -n "$applied" ] && [ "$applied" = "$current" ] || return 0
  echo "[$(now_ts)] SUPERSEDED: main advanced to $current while this run was asserting $TARGET_REV"
  echo "      Every Kustomization is on the newer revision, so nothing is broken: this run's premise"
  echo "      is stale because a later merge landed during it. The run triggered by that merge is the"
  echo "      one that verifies the deploy (and its revision contains this one). Re-run this workflow"
  echo "      to assert a specific revision explicitly."
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
  local phase="$1" payload="$2" lines sus
  if [ "$phase" = revision ]; then
    lines=$(revision_laggards "$payload")
  else
    lines=$(ready_laggards "$payload")
  fi
  [ -z "$lines" ] && return 0
  sus=$(suspended_names "$payload")
  printf '[%s] %s gate: still waiting on:\n' "$(now_ts)" "$phase"
  printf '%s\n' "$lines" | while IFS=$'\t' read -r name applied msg; do
    if [ -n "$sus" ] && printf '%s\n' "$sus" | grep -qx -- "$name"; then
      msg="$msg  [SUSPENDED in-cluster: a pause, not a failure]"
    fi
    printf '   %-32s %-22s %s\n' "$name" "$applied" "$(printf '%s' "$msg" | head -c 200)"
  done
}

report_suspended() {
  # Printed on the failure paths: name the pause and the exact way out, so the
  # next operator does not have to reconstruct this from a revision mismatch.
  local payload="$1" sus
  sus=$(suspended_names "$payload")
  [ -z "$sus" ] && return 0
  echo "[$(now_ts)] NOTE: suspended in-cluster (spec.suspend=true): $(printf '%s' "$sus" | tr '\n' ' ')"
  echo "      A suspended Kustomization never applies a new revision, so the revision gate cannot"
  echo "      pass and its dependents report 'dependency ... revision is not up to date'. That is a"
  echo "      deliberate in-cluster pause, not a defect in the merge under test. Resume it with"
  echo "      'flux resume kustomization <name> -n $NS' (or drop the suspend) and re-run this workflow."
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
      report_suspended "$payload"
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
      log_laggards "$phase" "$payload"
      report_superseded "$payload"
      report_suspended "$payload"
      echo "--- final kustomization state ---"
      flux get kustomizations -n "$NS" 2>/dev/null || kubectl -n "$NS" get kustomizations
      exit 1
    fi
    sleep "$POLL"
  done
done

echo "[$(now_ts)] all kustomizations converged to $TARGET_REV and Ready"