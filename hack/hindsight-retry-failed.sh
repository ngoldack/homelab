#!/usr/bin/env bash
# hindsight-retry-failed.sh — requeue Hindsight's terminal failed operations.
#
# The Hindsight dashboard's "operations (Failed)" count is the residue of a
# drained-but-not-finished ingest: an operation that fails its retries is
# terminal ('failed') and NOTHING requeues it — the worker only polls 'pending'.
# This script walks that residue through the documented API:
#
#   GET  /v1/default/banks/{bank_id}/operations?status=failed&limit=100&offset=N
#   POST /v1/default/banks/{bank_id}/operations/{operation_id}/retry
#
# The endpoint (verified against the deployed image — the public docs describe
# 0.10.1, this deployment is chart 0.9.2) resets status='pending',
# error_message/completed_at/next_retry_at/worker_id/claimed_at to NULL and
# retry_count to 0. For a payload-less `batch_retain` PARENT it instead re-queues
# that parent's failed children and revives the parent, so the ORDER matters:
# children first, then parents (the reverse makes the API answer 409 for every
# child the parent's cascade already requeued).
#
# Idempotent and re-runnable: it snapshots whatever is failed at that moment.
# Re-run it after the LLM leg recovers rather than retrying into a dark window.
#
# Prereq: the LLM leg the workers use is actually serving. The provider is
# Synthetic-only (OpenRouter removed 2026-09-22) and synthetic-proxy refuses a
# key for 5 minutes after a generic 429, so requeueing into a refusal window
# just burns each operation's fresh retry budget. Gate on a clean window:
#
#   kubectl --kubeconfig kubeconfig-home.yaml -n agentgateway \
#     exec deploy/synthetic-proxy -- wget -qO- localhost:8080/metrics \
#     | grep -E 'requests_total|upstream_429_total'
#
# sampled twice ~10 minutes apart: `upstream_429_total{reason="rate_limited"}`
# unchanged, `requests_total{verdict="healthy"}` growing.
#
# Usage: hack/hindsight-retry-failed.sh [--dry-run] [--api-url URL]
#                                       [--bank ID]... [--batch N] [--pause S] [--max N]
#   --dry-run      list what would be requeued, POST nothing
#   --api-url      default https://hindsight.ngoldack.de (LAN; the record is
#                  owned by the LAN external-dns instance). On a host without
#                  that DNS, port-forward instead:
#                    kubectl --kubeconfig kubeconfig-home.yaml -n hindsight \
#                      port-forward svc/hindsight-api 8888:8888
#                  then pass --api-url http://localhost:8888
#   --bank         restrict to one bank (repeatable); default: every bank the API lists
#   --batch        POSTs between pauses (default 25)
#   --pause        seconds to pause between batches (default 2)
#   --max          cap the operations requeued per bank (default: all)
#
# Requires: kubectl, curl, jq, and kubeconfig-home.yaml (the tenant key is read
# from the hindsight-env secret; it is never printed).
set -euo pipefail

API_DEFAULT="https://hindsight.ngoldack.de"
API="$API_DEFAULT"
BANK_FILTER=""
BATCH=25
PAUSE=2
MAX=0
DRY_RUN=0

KUBECONFIG_FILE="${KUBECONFIG:-kubeconfig-home.yaml}"
K="kubectl --kubeconfig $KUBECONFIG_FILE"

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; }
say() { printf '[retry-failed] %s\n' "$*"; }
fail() { printf '[retry-failed] FAIL: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --api-url) API="${2:?--api-url needs a value}"; shift ;;
    --api-url=*) API="${1#--api-url=}" ;;
    --bank) BANK_FILTER="$BANK_FILTER ${2:?--bank needs a value}"; shift ;;
    --bank=*) BANK_FILTER="$BANK_FILTER ${1#--bank=}" ;;
    --batch) BATCH="${2:?--batch needs a value}"; shift ;;
    --batch=*) BATCH="${1#--batch=}" ;;
    --pause) PAUSE="${2:?--pause needs a value}"; shift ;;
    --pause=*) PAUSE="${1#--pause=}" ;;
    --max) MAX="${2:?--max needs a value}"; shift ;;
    --max=*) MAX="${1#--max=}" ;;
    -h|--help) usage; exit 0 ;;
    *) printf '[retry-failed] unknown argument: %s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

command -v kubectl >/dev/null || fail "kubectl not on PATH"
command -v curl >/dev/null || fail "curl not on PATH"
command -v jq >/dev/null || fail "jq not on PATH"
test -f "$KUBECONFIG_FILE" || fail "kubeconfig $KUBECONFIG_FILE not found (run: task kubeconfig:home:export)"

TMPDIR_OUT="$(mktemp -d)"
trap 'rm -rf "$TMPDIR_OUT"' EXIT

KEY="$($K -n hindsight get secret hindsight-env -o jsonpath='{.data.HINDSIGHT_API_TENANT_API_KEY}' | base64 -d)"
[ -n "$KEY" ] || fail "could not read HINDSIGHT_API_TENANT_API_KEY from secret hindsight-env"

HTTP_CODE=""
HTTP_BODY=""

# http_call METHOD PATH — sets HTTP_CODE and HTTP_BODY (a file under TMPDIR_OUT).
http_call() {
  local method="$1" path="$2"
  HTTP_BODY="$TMPDIR_OUT/body"
  HTTP_CODE="$(curl -sS -m 60 -o "$HTTP_BODY" -w '%{http_code}' -X "$method" \
    -H "Authorization: Bearer $KEY" "$API$path")" || HTTP_CODE=000
  case "$HTTP_CODE" in
    401|403)
      fail "HTTP $HTTP_CODE from $API$path — check the tenant key (task hindsight:api:key)" ;;
  esac
}

banks_to_process() {
  if [ -n "$BANK_FILTER" ]; then
    printf '%s\n' $BANK_FILTER
    return
  fi
  http_call GET "/v1/default/banks?limit=200"
  [ "$HTTP_CODE" = "200" ] || fail "listing banks failed (HTTP $HTTP_CODE): $(head -c 300 "$HTTP_BODY")"
  jq -r '.banks[].bank_id' "$HTTP_BODY"
}

# snapshot_failed BANK — collects every failed operation id (and task_type) into
# one file BEFORE any POST: the retry endpoint shrinks the very set being paged,
# so paging while mutating would silently skip rows.
snapshot_failed() {
  local bank="$1" off=0 total=1 pages=0
  : > "$TMPDIR_OUT/snap"
  while [ "$off" -lt "$total" ]; do
    http_call GET "/v1/default/banks/$bank/operations?status=failed&limit=100&offset=$off"
    [ "$HTTP_CODE" = "200" ] || fail "listing $bank operations failed (HTTP $HTTP_CODE): $(head -c 300 "$HTTP_BODY")"
    [ "$pages" -eq 0 ] && total="$(jq -r '.total' "$HTTP_BODY")"
    jq -r '.operations[] | "\(.id)\t\(.task_type)"' "$HTTP_BODY" >> "$TMPDIR_OUT/snap"
    off=$((off + 100))
    pages=$((pages + 1))
    [ "$pages" -gt 200 ] && fail "paging $bank did not converge (>20000 rows)"
  done
  printf '%s\n' "${total:-0}" > "$TMPDIR_OUT/snap.total"
}

requeue_bank() {
  local bank="$1"
  local before requeued=0 skipped=0 errors=0 n=0 id type code detail remaining

  snapshot_failed "$bank"
  before="$(cat "$TMPDIR_OUT/snap.total")"
  # Children first, parents after: see the header note on the batch_retain cascade.
  awk -F'\t' '$2 != "batch_retain"' "$TMPDIR_OUT/snap" > "$TMPDIR_OUT/ordered"
  awk -F'\t' '$2 == "batch_retain"' "$TMPDIR_OUT/snap" >> "$TMPDIR_OUT/ordered"

  local planned
  planned="$(wc -l < "$TMPDIR_OUT/ordered" | tr -d ' ')"
  if [ "$MAX" -gt 0 ] && [ "$planned" -gt "$MAX" ]; then
    head -n "$MAX" "$TMPDIR_OUT/ordered" > "$TMPDIR_OUT/ordered.capped"
    mv "$TMPDIR_OUT/ordered.capped" "$TMPDIR_OUT/ordered"
    planned="$MAX"
  fi

  say "bank=$bank failed_before=$before planned=$planned (dry_run=$DRY_RUN)"
  if [ "$DRY_RUN" = 1 ]; then
    say "  dry run: first 5 -> $(head -n 5 "$TMPDIR_OUT/ordered" | tr '\t' ':' | tr '\n' ' ')"
    say "  dry run: last 5  -> $(tail -n 5 "$TMPDIR_OUT/ordered" | tr '\t' ':' | tr '\n' ' ')"
    return 0
  fi

  while IFS=$'\t' read -r id type; do
    [ -n "$id" ] || continue
    http_call POST "/v1/default/banks/$bank/operations/$id/retry"
    code="$HTTP_CODE"
    case "$code" in
      200) requeued=$((requeued + 1)) ;;
      409)
        skipped=$((skipped + 1))
        detail="$(jq -r '.detail // .message // "409"' "$HTTP_BODY" 2>/dev/null | head -c 160)"
        say "  skip $id ($type): $detail"
        ;;
      *)
        errors=$((errors + 1))
        say "  ERROR $id ($type) HTTP $code: $(head -c 160 "$HTTP_BODY")"
        ;;
    esac
    n=$((n + 1))
    [ "$PAUSE" -gt 0 ] && [ $((n % BATCH)) -eq 0 ] && [ "$n" -lt "$planned" ] && sleep "$PAUSE"
  done < "$TMPDIR_OUT/ordered"

  http_call GET "/v1/default/banks/$bank/operations?status=failed&limit=1"
  remaining="$(jq -r '.total' "$HTTP_BODY" 2>/dev/null || printf '?')"
  say "bank=$bank requeued=$requeued skipped=$skipped errors=$errors failed_after=$remaining"

  TOTAL_REQUEUED=$((TOTAL_REQUEUED + requeued))
  TOTAL_SKIPPED=$((TOTAL_SKIPPED + skipped))
  TOTAL_ERRORS=$((TOTAL_ERRORS + errors))
}

TOTAL_REQUEUED=0
TOTAL_SKIPPED=0
TOTAL_ERRORS=0

say "api=$API"
for bank in $(banks_to_process); do
  requeue_bank "$bank"
done

say "requeued=$TOTAL_REQUEUED skipped=$TOTAL_SKIPPED errors=$TOTAL_ERRORS"
[ "$TOTAL_ERRORS" -gt 0 ] && exit 1
exit 0
