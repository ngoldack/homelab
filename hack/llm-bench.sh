#!/usr/bin/env bash
#
# llm-bench.sh — small multi-prompt HTTP benchmark for llama.cpp servers.
#
# Measures prompt-processing (pp) and generation (tg) throughput from the
# server's own /completion `timings` object, so it exercises the real serving
# path (the ik_llama / mainline images ship no llama-bench binary). Deterministic:
# temperature 0, pinned seed, 3 repeats, cache off.
#
# Dependencies: curl, jq, awk (no cluster tools). A single positional arg is
# the base URL of the llama.cpp server (default http://127.0.0.1:8080).
# Optional env:
#   BENCH_TAG   arbitrary label appended to the CSV row (e.g. a pass name)
#   BENCH_N     repeats per cell (default 3)
#   BENCH_CTX   at-depth cell target context in tokens (default 8000)
#
# Output: a markdown comparison-table row on stdout, and an appended CSV row
# in hack/bench-results.csv (tag,date,prose,code,reasoning,depth,cat_mean,cat_sd,pp).
# Exits non-zero on any HTTP/parse failure so a broken field path fails the
# pass instead of being papered over.

set -euo pipefail

BENCH_URL="${1:-http://127.0.0.1:8080}"
BENCH_TAG="${BENCH_TAG:-unset}"
REPEATS="${BENCH_N:-3}"
DEPTH_TOKENS="${BENCH_CTX:-8000}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMPTS_DIR="$DIR/bench-prompts"
CSV="$DIR/bench-results.csv"

# Guard: an explicit BENCH_N=0 (or garbage) must fail loudly, not emit zeros.
if ! [[ "$REPEATS" =~ ^[0-9]+$ ]] || [ "$REPEATS" -lt 1 ] || [ "$REPEATS" -gt 20 ]; then
  echo "FATAL: BENCH_N must be an integer in [1,20], got '${REPEATS}'" >&2
  exit 1
fi
if ! [[ "$DEPTH_TOKENS" =~ ^[0-9]+$ ]] || [ "$DEPTH_TOKENS" -lt 1000 ]; then
  echo "FATAL: BENCH_CTX must be an integer >= 1000, got '${DEPTH_TOKENS}'" >&2
  exit 1
fi
if [ ! -d "$PROMPTS_DIR" ]; then
  echo "FATAL: prompts dir not found at $PROMPTS_DIR" >&2
  exit 1
fi

# --- JSON-string-escape a prompt file for embedding in a request body ---
json_prompt() {
  jq -Rs . < "$1"
}

# One /completion call. Args: <prompt-file> <n_predict> [cache_prompt]
# Prints tab-separated "pp\ttg" from timings on success, else non-zero.
do_completion() {
  local file="$1" n_pred="$2" cache="${3:-false}"
  local prompt body json line
  prompt="$(json_prompt "$file")"
  body="{\"prompt\":$prompt,\"n_predict\":$n_pred,\"temperature\":0,\"seed\":42,\"cache_prompt\":$cache,\"stream\":false}"
  json="$(curl -sS --max-time 600 -X POST -H 'Content-Type: application/json' \
    -d "$body" "$BENCH_URL/completion")" \
    || { echo "FATAL: curl to $BENCH_URL/completion failed" >&2; return 1; }
  line="$(printf '%s' "$json" | jq -r '[.timings.prompt_per_second,
                                         .timings.predicted_per_second] | @tsv')" \
    || { echo "FATAL: missing timings fields in response" >&2; return 1; }
  # A silent 400 or a null-parsed body yields an empty/non-numeric line; that
  # would otherwise flow through as a 0.00 cell and corrupt the row.
  ok="$(printf '%s' "$line" | awk -F'\t' 'NF==2 && $1+0>0 && $2+0>0 {print "1"}')"
  if [ "$ok" != "1" ]; then
    echo "FATAL: invalid timings line '$line' (empty / non-numeric / zero)" >&2
    echo "  -> server returned: $(printf '%s' "$json" | head -c 300)" >&2
    return 1
  fi
  printf '%s\n' "$line"
}

# mean + std of newline-separated float column; prints "mean	std".
mean_std() {
  awk '
    { s += $1; ss += $1*$1; n++ }
    END {
      if (n == 0) { print "0\t0"; exit }
      m = s / n
      var = (ss - n*m*m) / (n > 1 ? n - 1 : 1)
      if (var < 0) var = 0
      printf "%.2f\t%.2f\n", m, sqrt(var)
    }'
}

now="$(date -u +%FT%TZ)"

# --- Warm-up: small completion, discard result (kills cold-start variance) ---
do_completion "$PROMPTS_DIR/prose.txt" 16 >/dev/null 2>&1 \
  || { echo "FATAL: warm-up request failed" >&2; exit 1; }

# --- Prompt cells: three categories x REPEATS ---
CELLS=(prose code reasoning)
declare -a TGM TGS CATPP
for idx in 0 1 2; do
  cell="${CELLS[$idx]}"
  vals=""
  for ((r=0; r<REPEATS; r++)); do
    line="$(do_completion "$PROMPTS_DIR/$cell.txt" 128 false)" \
      || { echo "FATAL: $cell repeat $r failed" >&2; exit 1; }
    vals+="${line}"$'\n'
    CATPP[$idx]="$(printf '%s\n' "$line" | cut -f1)"
  done
  read -r m s <<< "$(printf '%s' "$vals" | awk '/./{print $2}' | mean_std)"
  TGM[$idx]="$m"; TGS[$idx]="$s"
done

# Category mean/std across the three cell means.
cat_mean_std="$(printf '%s\n' "${TGM[0]}" "${TGM[1]}" "${TGM[2]}" | mean_std)"
read -r CAT_MEAN CAT_SD <<< "$cat_mean_std"
prose_pp="${CATPP[0]}"

# --- At-depth cell: reasoning prompt repeated to ~DEPTH_TOKENS ---
tmpdepth="$(mktemp)"
cp "$PROMPTS_DIR/reasoning.txt" "$tmpdepth"
src_size="$(wc -c < "$PROMPTS_DIR/reasoning.txt")"
# ~4 chars/token heuristic; repeat a whole copy, then byte-trim to token cap.
tgt_bytes=$((DEPTH_TOKENS * 4))
while [ "$(wc -c < "$tmpdepth")" -lt "$tgt_bytes" ] && [ "$src_size" -gt 0 ]; do
  cat "$PROMPTS_DIR/reasoning.txt" >> "$tmpdepth"
done
# Use dd (not sed-depend truncate) on a copy to avoid cutting mid-build state.
head -c "$tgt_bytes" "$tmpdepth" > "${tmpdepth}.trim"
mv "${tmpdepth}.trim" "$tmpdepth"

dvals=""
for ((r=0; r<REPEATS; r++)); do
  line="$(do_completion "$tmpdepth" 128 false)" \
    || { echo "FATAL: depth repeat $r failed" >&2; exit 1; }
  dvals+="$(printf '%s\n' "$line" | cut -f2)"$'\n'
done
rm -f "$tmpdepth"
DEPTH_MS="$(printf '%s' "$dvals" | awk '/./{print $1}' | mean_std)"
read -r DEPTH_MEAN DEPTH_SD <<< "$DEPTH_MS"

# --- Emit: markdown row + CSV row ---
echo "| $BENCH_TAG | prose ${TGM[0]}±${TGS[0]} | code ${TGM[1]}±${TGS[1]} | reas ${TGM[2]}±${TGS[2]} | depth ${DEPTH_MEAN}±${DEPTH_SD} | cat ${CAT_MEAN}±${CAT_SD} | pp ${prose_pp} |"
printf '%s,%s,"%s","%s","%s","%s","%s","%s","%s"\n' \
  "$BENCH_TAG" "$now" \
  "${TGM[0]}±${TGS[0]}" "${TGM[1]}±${TGS[1]}" \
  "${TGM[2]}±${TGS[2]}" "${DEPTH_MEAN}±${DEPTH_SD}" \
  "${CAT_MEAN}±${CAT_SD}" "$CAT_SD" "$prose_pp" >> "$CSV"

exit 0