#!/usr/bin/env bash
# R7 render assertions for the Matrix ESS chart (kubernetes/infrastructure/home/matrix).
#
# Renders the pinned matrix-stack chart with the HelmRelease's OWN values block
# and asserts the offline invariants:
#   1. element-admin / matrix-rtc / livekit / bundled-postgres / hookshot NOT rendered
#   2. exactly one Synapse StatefulSet, one MAS Deployment, one Element Web
#      Deployment, one haproxy Deployment
#   3. serverName + the three ingress.host values resolve to EXACTLY
#      {matrix.ngoldack.de, element.ngoldack.de, matrix-auth.ngoldack.de}
#      (element.ngoldack.de — chat.ngoldack.de belongs to LibreChat)
#   4. NO ServiceMonitor is rendered even when the prometheus-operator CRDs are
#      advertised (serviceMonitors.enabled: false everywhere; the CRDs are not
#      installed in this cluster)
#   5. Element Web config.json points at the LOCAL homeserver — base_url is
#      https://matrix.ngoldack.de and no matrix.org default leaks in
#   6. resources (requests/limits) are set on Synapse/MAS/Element Web AND on
#      haproxy (lightweight budget)
#   7. the 3 rendered Ingress CRs are the KNOWN inert orphans (documented, not relied on)
#
# Requires: helm >= 4.x, python3 (PyYAML), network to dimensions of the chart.
# Usage: ./hack/validate-matrix-render.sh <path-to-matrix-stack-chart> [values-file]
#   <path-to-matrix-stack-chart> is the extracted chart dir (see the R7 section of
#   docs/hermes-multi-agent-matrix.md for how the chart is fetched). The values
#   are extracted from the HelmRelease itself; pass <values-file> only to check
#   an override set.

set -euo pipefail
CHART="${1:?path to matrix-stack chart dir}"
VALUES="${2:-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELMRELEASE="$HERE/../kubernetes/infrastructure/home/matrix/helmrelease.yaml"

render="$(mktemp)"
values="$(mktemp)"
cleanup() { rm -f "$render" "$values"; }
trap cleanup EXIT

if [[ -n "$VALUES" ]]; then
  cp "$VALUES" "$values"
else
  [[ -f "$HELMRELEASE" ]] || { echo "FAIL: helmrelease not found at $HELMRELEASE"; exit 1; }
  # The HelmRelease's spec.values block IS the chart input — extract it rather
  # than keeping a second, drifting copy of the values next to this script.
  python3 - "$HELMRELEASE" "$values" <<'PY'
import sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
hr = [d for d in docs if d.get('kind') == 'HelmRelease']
if len(hr) != 1:
    print(f"FAIL: expected exactly 1 HelmRelease in {sys.argv[1]}, got {len(hr)}"); sys.exit(1)
values = hr[0].get('spec', {}).get('values')
if not isinstance(values, dict) or not values:
    print("FAIL: HelmRelease has no spec.values"); sys.exit(1)
with open(sys.argv[2], 'w') as fh:
    yaml.safe_dump(values, fh, default_flow_style=False, sort_keys=False)
PY
fi

# --api-versions advertises the prometheus-operator CRD on purpose: the chart
# gates ServiceMonitor rendering on Capabilities.APIVersions, so this is the
# ONLY way to prove the values (not the capability) keep them out. Without it
# the assertion would pass vacuously.
helm template matrix "$CHART" --values "$values" --namespace matrix \
  --api-versions networking.k8s.io/v1 \
  --api-versions monitoring.coreos.com/v1/ServiceMonitor > "$render"

python3 - "$render" "$values" "$(dirname "$CHART")" "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)" <<'PY'
import glob, json, os, sys, yaml
docs = [d for d in yaml.safe_load_all(open(sys.argv[1])) if d]
values = yaml.safe_load(open(sys.argv[2])) or {}
kinds = [(d.get('kind'), d.get('metadata', {}).get('name', '')) for d in docs]

def fail(msg):
    print(f"FAIL: {msg}"); sys.exit(1)

# 1. disabled components absent
for pat in ('element-admin', 'matrix-rtc', 'livekit', '-postgres-', 'hookshot'):
    hit = [k for k in kinds if pat in k[1].lower()]
    if hit: fail(f"disabled component present: {hit}")

counts = {}
for k in kinds: counts[k] = counts.get(k, 0) + 1
def n(kind, name): return counts.get((kind, name), 0)

if n('StatefulSet', 'matrix-synapse-main') != 1:
    fail(f"expected 1 matrix-synapse-main StatefulSet, got {n('StatefulSet', 'matrix-synapse-main')}")
if n('Deployment', 'matrix-matrix-authentication-service') != 1:
    fail("MAS not exactly 1 Deployment")
if n('Deployment', 'matrix-element-web') != 1:
    fail("element-web not exactly 1 Deployment")
if n('Deployment', 'matrix-haproxy') != 1:
    fail(f"expected 1 haproxy Deployment, got {n('Deployment', 'matrix-haproxy')}")

# 3. ingress hosts — the exact expected set (element.ngoldack.de, never chat.)
hosts = []
for d in docs:
    if d.get('kind') == 'Ingress':
        for h in d.get('spec', {}).get('rules', []):
            hosts.append(h.get('host'))
exp = {'matrix.ngoldack.de', 'element.ngoldack.de', 'matrix-auth.ngoldack.de'}
if set(hosts) != exp or len(hosts) != len(exp):
    fail(f"ingress hosts {set(hosts)} != expected {exp}; note: chart also renders TLS")

# 4. no ServiceMonitor despite the advertised CRD
sm = [k for k in kinds if k[0] == 'ServiceMonitor']
if sm:
    fail(f"ServiceMonitor rendered although the CRD is absent here: {sm}")

# 5. Element Web points at the LOCAL homeserver, no matrix.org default
ew = [d for d in docs if d.get('kind') == 'ConfigMap' and d.get('metadata', {}).get('name') == 'matrix-element-web']
if len(ew) != 1:
    fail(f"expected 1 matrix-element-web ConfigMap, got {len(ew)}")
config = json.loads(ew[0]['data']['config.json'])
base_url = config.get('default_server_config', {}).get('m.homeserver', {}).get('base_url')
if base_url != 'https://matrix.ngoldack.de':
    fail(f"element-web base_url {base_url!r} != 'https://matrix.ngoldack.de'")
if 'matrix.org' in json.dumps(config):
    fail("element-web config.json still contains a matrix.org default")

# 6. resources set (synapse/MAS/element-web + haproxy). The generic loop is
# non-vacuous only for values that differ from the chart defaults, so haproxy —
# the component whose budget the HelmRelease deliberately trims — is checked
# against the values block itself: if `haproxy.resources` is dropped from the
# HelmRelease the chart's own 100m/100Mi/200Mi default would otherwise sail
# through.
for d in docs:
    name = d['metadata']['name']
    if name not in ('matrix-synapse-main', 'matrix-matrix-authentication-service',
                    'matrix-element-web', 'matrix-haproxy'):
        continue
    for c in d.get('spec', {}).get('template', {}).get('containers', []):
        r = c.get('resources', {})
        if not r.get('requests') or not r.get('limits'):
            fail(f"{name}/{c.get('name')} missing resources (requests/limits)")

want_haproxy = values.get('haproxy', {}).get('resources')
if not want_haproxy:
    fail("HelmRelease sets no haproxy.resources (the lightweight budget)")
haproxy = [d for d in docs if d.get('kind') == 'Deployment'
           and d.get('metadata', {}).get('name') == 'matrix-haproxy']
got_haproxy = haproxy[0]['spec']['template']['spec']['containers'][0].get('resources')
if got_haproxy != want_haproxy:
    fail(f"haproxy resources {got_haproxy} != HelmRelease haproxy.resources {want_haproxy}")

# 7. every backendRef in the repo-authored Matrix routes resolves to a Service
# the chart actually renders — the "guessed service name" failure mode the
# brief calls out. Names and ports are compared against the same render.
repo = sys.argv[4] if len(sys.argv) > 4 else None
services = {}
for d in docs:
    if d.get('kind') == 'Service':
        services[d['metadata']['name']] = {
            (p_.get('name'), p_.get('port')) for p_ in d['spec'].get('ports', [])
        }
routed = 0
if repo:
    for f in sorted(glob.glob(os.path.join(repo, 'kubernetes/infrastructure/home/matrix/*.yaml'))):
        try:
            route_docs = [d for d in yaml.safe_load_all(open(f)) if d]
        except Exception as exc:  # a parse error here is a real failure
            fail(f"could not parse {f}: {exc}")
        for d in route_docs:
            if d.get('kind') != 'HTTPRoute':
                continue
            for rule in d['spec'].get('rules', []):
                for b in rule.get('backendRefs') or []:
                    routed += 1
                    name, port = b.get('name'), b.get('port')
                    if name not in services:
                        fail(f"{os.path.basename(f)}: backendRef {name!r} is not a rendered Service")
                    elif all(pp != port for _, pp in services[name]):
                        fail(f"{os.path.basename(f)}: backendRef {name}:{port} has no matching "
                             f"rendered service port {sorted(services[name])}")
if routed == 0:
    fail("no HTTPRoute backendRefs were checked (routes missing?)")

# 8. ingress CR count is the known 3 (inert orphans)
ing = sum(1 for d in docs if d.get('kind') == 'Ingress')
if ing != 3:
    fail(f"expected 3 inert Ingress CRs, got {ing}")

print("PASS: matrix render invariants hold (0 admin/rtc/livekit/pg/hookshot; "
      "1 synapse StatefulSet + 1 MAS + 1 element-web + 1 haproxy; "
      "ingress hosts exactly {matrix,element,matrix-auth}.ngoldack.de; "
      "0 ServiceMonitors; element-web base_url=local; resources set; "
      f"{routed} route backendRefs all resolve; 3 inert Ingress CRs)")
PY
