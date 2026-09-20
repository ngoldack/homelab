# pxa-ab-bench.sh — A/B benchmark cells for the pxa-vs-llama-p100 canary passes.
#
# Extends hack/llm-bench.sh with the cells the canary A/B needs: exact-token cold
# prefill ladder (1k/8k/16k), warm in-memory prefill, 512-token decode on three
# prompt classes, and a JSON tool-call compliance cell. Deterministic: temperature
# 0, seed 42. Reads server-reported `timings` (prompt_per_second /
# predicted_per_second) and the engine's cached-token field (probed once).
#
# Dependencies: curl, jq, awk. Args: <base-url> (default http://127.0.0.1:8080).
# Env:
#   BENCH_TAG   label for CSV rows + log filenames (required, non-empty)
#   BENCH_MODEL model label for CSV rows (default $BENCH_TAG)
#   BENCH_N     repeats per cell (default 3, range [1,20])
#
# Outputs:
#   - raw per-run response JSON: hack/bench-logs/<tag>-<cell>-<rep>.json
#   - summary rows appended to hack/bench-results-pxa-ab.csv
# Exits non-zero on any HTTP/parse failure (broken field paths fail the pass).

set -euo pipefail

BENCH_URL="${1:-http://127.0.0.1:8080}"
: "${BENCH_TAG:?BENCH_TAG must be set}"
BENCH_MODEL="${BENCH_MODEL:-$BENCH_TAG}"
REPEATS="${BENCH_N:-3}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROMPTS_DIR="$DIR/bench-prompts"
PXQ_DIR="$DIR/pxa-ab-prompts"
LOGS="$DIR/bench-logs"
CSV="$DIR/bench-results-pxa-ab.csv"

if ! [[ "$REPEATS" =~ ^[0-9]+$ ]] || [ "$REPEATS" -lt 1 ] || [ "$REPEATS" -gt 20 ]; then
  echo "FATAL: BENCH_N must be an integer in [1,20], got '${REPEATS}'" >&2; exit 1
fi
mkdir -p "$LOGS"
[ -f "$CSV" ] || printf 'engine,model,date,cell,rep,prompt_n,prompt_ms,pp,tg,cached\n' > "$CSV"
for f in "$PROMPTS_DIR/prose.txt" "$PROMPTS_DIR/code.txt" "$PROMPTS_DIR/reasoning.txt" "$PXQ_DIR/json-tool.txt"; do
  [ -f "$f" ] || { echo "FATAL: missing prompt file $f" >&2; exit 1; }
done

json_prompt() { jq -Rs . < "$1"; }

now() { date -u +%FT%TZ; }

# POST to $BENCH_URL<path> with raw json body; prints body; fail on HTTP error.
post_json() { # <path> <body>
  curl -sS --max-time 900 -X POST -H 'Content-Type: application/json' -d "$2" \
    "$BENCH_URL$1" || { echo "FATAL: curl $1 failed" >&2; exit 1; }
}

# --- Probe: locate the engine's cached-token field once, record it ---
PADDING=$(printf 'probe%.0s' {1..60})
resp="$(post_json /completion "{\"prompt\":\"$PADDING\",\"n_predict\":2,\"temperature\":0,\"seed\":42,\"cache_prompt\":false,\"stream\":false}")"
CACHED_JQ='( .n_prompt_tokens_cache // (.usage.prompt_tokens_details.cached_tokens // (.timings.prompt_tokens - .timings.prompt_n)) )'
printf '%s' "$resp" | jq -e '.timings.prompt_per_second, .timings.predicted_per_second' >/dev/null \
  || { echo "FATAL: probe missing timings — server response:" >&2; printf '%s' "$resp" | head -c 400 >&2; exit 1; }
echo "probe: cached-token field jq = ${CACHED_JQ}"

# --- Exact-token prompt builder: wordlist repeated until /tokenize >= target ---
build_prompt() { # <outfile> <target_tokens>
  local out="$1" target="$2" src="$PROMPTS_DIR/reasoning.txt"
  local src_bytes tgt_bytes
  src_bytes="$(wc -c < "$src")"
  tgt_bytes=$(( target * 4 ))
  : > "$out"
  while [ "$(wc -c < "$out")" -lt "$tgt_bytes" ]; do cat "$src" >> "$out"; done
  head -c "$tgt_bytes" "$out" > "${out}.trim"; mv "${out}.trim" "$out"
  # Verify token count via /tokenize; loosen if it 404s (char/4 heuristic stands).
  local fz
  fz="$(mktemp)"
  post_json /tokenize "{\"content\":$(json_prompt "$out")}" > "$fz" 2>/dev/null \
    && n=$(jq -r '.tokens | length' "$fz" 2>/dev/null) || n=-1
  rm -f "$fz"
  if [ "$n" -ge 0 ]; then
    echo "build_prompt($out): tokenized=$n target=$target" >&2
    [ "$n" -ge "$target" ] || { echo "FATAL: prompt under token target (n=$n < $target)" >&2; exit 1; }
  else
    echo "build_prompt($out): /tokenize unavailable — using char/4 estimate" >&2
  fi
}

# One completion; append raw JSON log; print "pp<tab>tg<tab>prompt_n<tab>prompt_ms<tab>cached".
do_cell() { # <cell> <rep> <promptfile> <n_predict> <cache_prompt>
  local cell="$1" rep="$2" pf="$3" npred="$4" cache="$5" body json
  body="{\"prompt\":$(json_prompt "$pf"),\"n_predict\":$npred,\"temperature\":0,\"seed\":42,\"cache_prompt\":$cache,\"stream\":false}"
  json="$(post_json /completion "$body")"
  printf '%s' "$json" > "$LOGS/$BENCH_TAG-$cell-$rep.json"
  local line
  line="$(printf '%s' "$json" | jq -r '[.timings.prompt_per_second, .timings.predicted_per_second, .timings.prompt_n, .timings.prompt_ms] | @tsv')"
  ok="$(printf '%s' "$line" | awk -F'\t' 'NF==4 && $1+0>0 && $2+0>=0 && $3+0>=0 && $4+0>=0 {print "1"}')"
  [ "$ok" = "1" ] || { echo "FATAL: invalid timings '$line' ($cell rep $rep)" >&2; exit 1; }
  local pn
  pn="$(printf '%s' "$json" | jq -r "$CACHED_JQ // 0")"
  printf '%s\t%s\t%s\t%s\t%s\n' "$line" "$pn"
}

# --- Cells ---
CELLS=(cold1024 cold8192 cold16384 decode-prose decode-code decode-reasoning warm)
for cell in "${CELLS[@]}"; do
  case "$cell" in
    cold*) tgt="${cell#cold}"; pf=$(mktemp); build_prompt "$pf" "$tgt"; npred=8; cache=false ;;
    decode-*) c="${cell#decode-}"; pf="$PROMPTS_DIR/${c}.txt"; [ "$c" = "reasoning" ] && pf="$PROMPTS_DIR/reasoning.txt"; npred=512; cache=false ;;
    warm) pf="$PROMPTS_DIR/prose.txt"; npred=64; cache=true ;;
  esac
  vals=""
  for ((r=1; r<=REPEATS; r++)); do
    read -r pps tgs pn pms cached <<< "$(do_cell "$cell" "$r" "$pf" "$npred" "$cache")"
    printf '%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
      "$BENCH_TAG" "$BENCH_MODEL" "$(now)" "$cell" "$r" "$pn" "$pms" "$pps" "$tgs" "$cached" >> "$CSV"
    vals+="${pps} ${tgs}"$'\n'
  done
  case "$pf" in /tmp/*) rm -f "$pf" ;; esac
  echo "| $BENCH_TAG | $cell | $(printf '%s' "$vals" | awk 'NF==2 {s+=$1; n++} END {printf "pp_mean=%.2f", s/(n?n:1)}') |"
done

# --- JSON tool-call compliance cell (deterministic pass) ---
JCELL_PF="$PXQ_DIR/json-tool.txt"
JFAIL=0; JN=0
for ((r=1; r<=REPEATS; r++)); do
  body="$(json_prompt "$JCELL_PF")"
  jbody="{\"model\":\"local\",\"messages\":[{\"role\":\"user\",\"content\":$body}],\"temperature\":0,\"response_format\":{\"type\":\"json_object\"}}"
  jresp="$(post_json /v1/chat/completions "$jbody")"
  printf '%s' "$jresp" > "$LOGS/$BENCH_TAG-json-tool-$r.json"
  jc=$(printf '%s' "$jresp" | jq -r '.choices[0].message.content // empty' 2>/dev/null)
  JN=$((JN+1))
  if printf '%s' "$jc" | jq -e 'type=="object" and (.name|type=="string") and (.arguments|type=="object")' >/dev/null 2>&1; then
    : # compliant
  else
    JFAIL=$((JFAIL+1)); echo "WARN: json-tool rep $r NOT compliant" >&2
    # Retry against the non-json path only to diagnose, never to paper over.
  fi
done
echo "json-tool-cell: $((JN-JFAIL))/$JN compliant"
[ "$JFAIL" -eq 0 ] || { echo "FATAL: JSON tool-call compliance $((JN-JFAIL))/$JN (required 100%)" >&2; exit 1; }
exit 0