#!/usr/bin/env bash
# Verify the per-agent LLM chain wiring for the multi-agent Hermes deployment.
#
# The contract (docs/hermes-multi-agent-matrix.md):
#   dave         -> general-purpose
#   lindner      -> general-purpose
#   chad         -> coding-medium   (default)
#   chad + tier  -> coding-small / coding-large   (per-Kanban-card override)
#
# OFFLINE (default): asserts the manifests agree with that contract —
#   * each profile's default provider base_url names the expected /v1/chain/<chain>
#   * chad registers the coding-small and coding-large providers, because the
#     dispatcher passes `-m <model> --provider <provider>` for a card that pins
#     a tier (hermes_cli/kanban_db_dispatch.py `_worker_argv`)
#   * the agentgateway HTTPRoute exposes /v1/chain/<chain> and the chain backends
#     it points at exist
#
# LIVE (--live): for each agent pod, prints the profile that is actually running
# and the provider its profile config resolves to, then makes one real request
# through the pod's own API. Chain attribution is NOT asserted here: which leg
# of a chain served a call is only visible in the gateway's own telemetry, so
# the script prints the exact command to look at instead of guessing.
#
# Usage: hack/verify-agent-chains.sh [--live] [--namespace NS]
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENTS_DIR="$ROOT/kubernetes/infrastructure/home/hermes-agents"
GW_DIR="$ROOT/kubernetes/infrastructure/home/agentgateway"
NS="hermes"
LIVE=0
for arg in "$@"; do
  case "$arg" in
    --live) LIVE=1 ;;
    --namespace) shift ;;
    --namespace=*) NS="${arg#--namespace=}" ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

fail=0
ok()   { printf 'PASS  %s\n' "$1"; }
bad()  { printf 'FAIL  %s\n' "$1" >&2; fail=1; }

# ---------------------------------------------------------------- offline ----
command -v python3 >/dev/null || { echo "python3 required" >&2; exit 2; }

offline_out="$(python3 - "$AGENTS_DIR" "$GW_DIR" <<'PY'
import sys, yaml, pathlib

agents_dir, gw_dir = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
expected = {
    "dave": "general-purpose",
    "lindner": "general-purpose",
    "chad": "coding-medium",
    "marius": "general-purpose",
    "orchestrator": "general-purpose",
}
tiers = {"chad": ("coding-small", "coding-large")}
problems, notes = [], []

cm = None
for d in yaml.safe_load_all((agents_dir / "configmap.yaml").read_text()):
    if d and d.get("kind") == "ConfigMap":
        cm = d["data"]
if cm is None:
    problems.append("hermes-agents-config ConfigMap not found")
    print("\n".join("FAIL  " + p for p in problems)); sys.exit(1)

for agent, chain in expected.items():
    raw = cm.get(f"{agent}.yaml")
    if not raw:
        problems.append(f"configmap key {agent}.yaml missing")
        continue
    cfg = yaml.safe_load(raw)
    provs = cfg.get("providers") or {}
    default = (cfg.get("providers") or {}).get("agentgateway") or {}
    url = str(default.get("base_url", ""))
    if f"/v1/chain/{chain}" not in url:
        problems.append(f"{agent}: default provider base_url is not /v1/chain/{chain} ({url!r})")
    else:
        notes.append(f"{agent} -> /v1/chain/{chain}")
    for tier in tiers.get(agent, ()):
        turl = str(((provs.get(tier) or {}).get("base_url")) or "")
        if f"/v1/chain/{tier}" not in turl:
            problems.append(f"{agent}: tier provider {tier} does not point at /v1/chain/{tier} ({turl!r})")
        else:
            notes.append(f"{agent} (card override) -> /v1/chain/{tier}")

# gateway routes + the backends they reference
routes = yaml.safe_load_all((gw_dir / "routes.yaml").read_text())
route_urls = set()
for d in routes:
    if d and d.get("kind") == "HTTPRoute":
        for rule in d["spec"].get("rules", []):
            for m in rule.get("matches") or []:
                v = (m.get("path") or {}).get("value")
                if v:
                    route_urls.add(v)
backends = set()
for d in yaml.safe_load_all((gw_dir / "agent-chains.yaml").read_text()):
    if d and d.get("kind") == "AgentgatewayBackend":
        backends.add(d["metadata"]["name"])
for chain in ("general-purpose", "coding-small", "coding-medium", "coding-large"):
    if f"/v1/chain/{chain}" not in route_urls:
        problems.append(f"agentgateway route /v1/chain/{chain} missing from routes.yaml")
    elif f"{chain}-chain" not in backends:
        problems.append(f"route /v1/chain/{chain} has no {chain}-chain AgentgatewayBackend")
    else:
        notes.append(f"route /v1/chain/{chain} -> backend {chain}-chain")

for n in notes:
    print("NOTE  " + n)
for p in problems:
    print("FAIL  " + p)
sys.exit(1 if problems else 0)
PY
)" || true
if printf '%s\n' "$offline_out" | grep -q '^FAIL'; then
  printf '%s\n' "$offline_out" | grep '^FAIL' >&2
  fail=1
else
  printf '%s\n' "$offline_out" | grep '^NOTE'
  ok "offline chain wiring (manifests)"
fi

# ------------------------------------------------------- pod structure ----
# A profile copied from a sibling keeps the sibling's numbers and lists: a wrong
# probe port leaves the pod never-Ready, a missing boot-config key fails the seed
# initContainer under `set -eu` (blocking the roll of EVERY pod), and an env var
# renamed on one side of a reference is an unbound variable in that script. All
# three are silent until a rollout, so they are asserted here.
if ! struct_out="$(python3 "$ROOT/hack/verify-agent-structure.py" "$AGENTS_DIR")"; then
  printf '%s\n' "$struct_out" | grep '^FAIL' >&2 || printf '%s\n' "$struct_out" >&2
  fail=1
else
  ok "pod structure (boot-config projections, probe ports, token suffix, seed script)"
fi

# ------------------------------------------------------------------- live ----
if [ "$LIVE" -eq 1 ]; then
  command -v kubectl >/dev/null || { echo "--live needs kubectl" >&2; exit 2; }
  for agent in dave chad lindner marius; do
    pod="$(kubectl -n "$NS" get pod -l "app.kubernetes.io/instance=$agent" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [ -z "$pod" ]; then
      bad "$agent: no pod found in namespace $NS"
      continue
    fi
    echo "--- $agent ($pod)"
    kubectl -n "$NS" exec "$pod" -c "$agent" -- sh -c '
      set -eu
      echo "HERMES_HOME=$HERMES_HOME"
      echo "HERMES_PROFILE=$HERMES_PROFILE"
      # the provider the profile config resolves to (never print the key itself)
      sed -n "/^providers:/,/^[a-z]/p" "$HERMES_HOME/config.yaml" | grep -E "agentgateway:|base_url:" | head -4
      curl -fsS -o /dev/null -w "api /v1/models: HTTP %{http_code}\n" \
        -H "Authorization: Bearer $API_SERVER_KEY" "http://127.0.0.1:${API_SERVER_PORT}/v1/models"
    ' || bad "$agent: in-pod check failed"
    ok "$agent: profile resolved and API answering"
  done
  cat <<'EOF'

Chain attribution (which leg served a call) is NOT asserted here: it lives in the
gateway's own telemetry. Look at the chain backend's metrics/logs for the window
of your request, e.g.:

  kubectl -n agentgateway logs -l app.kubernetes.io/name=agentgateway --since=5m | grep -i 'chain'
  # or the Gateway API route status for the path you called:
  kubectl -n agentgateway get httproute local-llm -o jsonpath='{.status.parents[*].conditions[*].type}{"\n"}'

A synthetic tier that answers a 5xx (the proxy's "hold this key off" signal)
evicts itself for 5 minutes (policy-agent-chains-health.yaml). The chains have
had no fallback group since 2026-09-22, so the caller sees that 503 rather than
a second provider serving the call -- an eviction is not a failover any more.
EOF
fi

if [ "$fail" -ne 0 ]; then
  echo "FAIL: chain verification failed" >&2
  exit 1
fi
echo "PASS: agent chain wiring verified${LIVE:+ (offline + live)}"
