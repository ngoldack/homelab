# Langfuse next to Phoenix: Authentik OIDC + VictoriaMetrics

## Context

Langfuse (LLM observability, same role as Phoenix) is already fully deployed in
`kubernetes/infrastructure/home/langfuse/` with external stores via installed
operators (CNPG, valkey-operator, clickhouse-operator, SeaweedFS-bundled S3), a
HelmRelease that already wires `AUTH_AUTHENTIK_*` OIDC env pointing at
`https://authentik.ngoldack.de/application/o/langfuse`, and a cluster
Kustomization entry. Three pieces are missing to satisfy the request "authentik
oidc + metrics in vm": (1) the Authentik provider + application the HelmRelease
references, (2) the edge HTTPRoute so `langfuse.ngoldack.de` resolves, and (3)
the VictoriaMetrics scrape so langfuse metrics appear in the VM stack.

## Approach

### 1. Authentik seed + OIDC secret (Auth)

The langfuse HelmRelease already sets:
`AUTH_AUTHENTIK_CLIENT_ID=langfuse-oidc`,
`AUTH_AUTHENTIK_ISSUER=https://authentik.ngoldack.de/application/o/langfuse`, and
pulls `AUTH_AUTHENTIK_CLIENT_SECRET` from secret `langfuse-secret` (key
`AUTH_AUTHENTIK_CLIENT_SECRET`). The seed must create a provider named
`langfuse-oidc` with `client_id: langfuse` and an application with slug `langfuse`
(auth callback redirect URI `https://langfuse.ngoldack.de/api/auth/callback/authentik`).

**Load-bearing client-secret consistency:** the authentik provider's
`client_secret` MUST equal the `AUTH_AUTHENTIK_CLIENT_SECRET` value already inside
`kubernetes/infrastructure/home/langfuse/langfuse-secret.sops.yaml` (SOPS-encrypted,
age key at repo root as `age.key`). Steps:
1. Decrypt the existing value: `sops -d --input-type yaml langfuse-secret.sops.yaml`
   → capture the plaintext `stringData.AUTH_AUTHENTIK_CLIENT_SECRET`.
2. Create `kubernetes/infrastructure/home/authentik/oidc-langfuse.sops.yaml`:
   a Kubernetes `Secret` (type opaque, `stringData.client_secret: <exact value from
   step 1>`), named `authentik-oidc-langfuse`, SOPS-encrypted. Mirror the exact file
   shape of `oidc-grafana.sops.yaml` (SAME 3 age recipients). Use the repo's
   `sops -e` mechanics from memory (encrypt with `--filename-override <realpath>
   --output tmp` then `mv`; do NOT `sops -e file > target`).
3. Edit `kubernetes/infrastructure/home/authentik/helmrelease.yaml`:
   add a second global volume + volumeMount for this secret so the blueprint can
   `!File` it. Do NOT reuse `/secrets` (grafana occupies it) — use a distinct path:
   ```yaml
   global:
     volumes:
       - name: seed-secrets          # existing (grafana)
         secret: { secretName: authentik-oidc-grafana }
       - name: seed-secrets-langfuse
         secret: { secretName: authentik-oidc-langfuse }
     volumeMounts:
       - name: seed-secrets
         mountPath: /secrets
         readOnly: true
       - name: seed-secrets-langfuse
         mountPath: /secrets-langfuse
         readOnly: true
   ```
4. Edit `kubernetes/infrastructure/home/authentik/seed.yaml` (ConfigMap
   `authentik-blueprints`, same single YAML document — entries must be added
   AFTER the grafana-oidc block, around line 196, BEFORE the MFA section; ordering:
   provider before the app that `!Find`s it). Add entries modeled exactly on the
   grafana-oidc + grafana-application blocks (lines 142-196), with these differences:
   - provider `model: authentik_providers_oauth2.oauth2provider`, `identifiers.name:
     langfuse-oidc`, attrs:
     - `client_id: langfuse`
     - `client_type: confidential`
     - `client_secret: !File /secrets-langfuse/client_secret`
     - `issuer_mode: per_provider`
     - `grant_types: [authorization_code, refresh_token]` (MUST be pinned — empty
       grants deny every OIDC login)
     - `redirect_uris:` → `matching_mode: strict`, url
       `https://langfuse.ngoldack.de/api/auth/callback/authentik`
     - same `property_mappings` (`sm_openid/sm_profile/sm_email/sm_groups`) and the
       three flow `!Find`s as grafana (authorization implicit-consent,
       authentication default, invalidation default)
   - application `model: authentik_core.application`, `identifiers.slug: langfuse`,
     attrs `name: langfuse`, `meta_name: Langfuse`,
     `meta_launch_url: https://langfuse.ngoldack.de/`,
     `provider: !Find [authentik_providers_oauth2.oauth2provider, [name, langfuse-oidc]]`
   - PolicyBinding for `target: !Find [authentik_core.application, [slug, langfuse]]`,
     `order: 0`, `group: !Find [authentik_core.group, [name, akadmin]]` — same shape
     as the langfuse-grafana bindings (lines 397-404), so SSO is admin-gated like
     every other app.
   - **Do NOT** add the provider/app to the `authentik_outposts.outpost` `providers`
     list (lines 133-137): langfuse uses native OIDC, NOT the outpost proxy.
5. Add `oidc-langfuse.sops.yaml` to `kubernetes/infrastructure/home/authentik/kustomization.yaml`
   resources list.
6. Validate the blueprint before commit:
   `yamllint seed.yaml` +
   `kubectl -n authentik exec deploy/authentik-server -- ak shell -c "from authentik.blueprints.v1.importer import Importer; ok,logs=Importer.from_string(open('/tmp/seed.yaml').read(),None).validate(); print('valid' if ok else logs)"`
   (feed the updated seed content). Then after commit/push/test:
   `kubectl -n authentik rollout restart deploy/authentik-server deploy/authentik-worker`
   and confirm
   `kubectl -n authentik exec deploy/authentik-server -- ak shell -c "from authentik.blueprints.models import BlueprintInstance; print(BlueprintInstance.objects.get(name='homelab-edge-ingress').status)"`
   → `successful`.

### 2. Edge HTTPRoute (route)

Create `kubernetes/infrastructure/home/langfuse/httproute-edge.yaml` mirroring
`phoenix/httproute-edge.yaml` BUT backend the langfuse web Service directly (native
OIDC — NOT the authentik-proxy outpost; the provider already does the auth handshake
in-app). Same-namespace backend → NO ReferenceGrant change needed. Route:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: langfuse-edge
  namespace: langfuse
spec:
  parentRefs:
    - name: edge
      namespace: network
      sectionName: https-apex
  hostnames:
    - langfuse.${CLUSTER_DOMAIN}
  rules:
    - backendRefs:
        - name: langfuse-web
          port: 3000
```
The langfuse web Service is `langfuse-web`, port 3000, port name `http` (verified
from chart `templates/web/service.yaml` + `values.yaml`). The edge `https-apex`
listener already accepts routes from namespaces labeled
`gateway.ngoldack.de/edge-ingress: "true"` (langfuse namespace has it); the edge
external-dns auto-publishes edge-served names (routes without
`dns.ngoldack.de/publish: lan`), and `*.ngoldack.de` wildcard cert covers it — so
NO DNS record file is needed. Do NOT add a `dns.ngoldack.de/publish` label (that is
the LAN filter). Add `httproute-edge.yaml` to
`kubernetes/infrastructure/home/langfuse/kustomization.yaml`.

### 3. VictoriaMetrics scrape (metrics)

Create `kubernetes/infrastructure/home/langfuse/vm-scrapes.yaml`, a
`VMServiceScrape` (CRD kind installed by `monitoring-crds`, used by vmagent with
`selectAllByDefault: true`). Langfuse web serves Prometheus metrics at
`/api/public/metrics` on the app port 3000 (no dedicated metrics port — verified
against chart: only `http` port 3000 exists). Mirror the exact shape of
`phoenix/vm-scrapes.yaml` (port-name endpoint + namespaceSelector pin), using
`path: /metrics` → `path: /api/public/metrics`:
```yaml
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMServiceScrape
metadata:
  name: langfuse
  namespace: langfuse
spec:
  endpoints:
    - port: http
      path: /api/public/metrics
  selector:
    matchLabels:
      app: web
  namespaceSelector:
    matchNames:
      - langfuse
```
`app: web` is the service's own selector label (from `templates/web/service.yaml`
selector `app: web`). Add `vm-scrapes.yaml` to the langfuse kustomization.yaml.

## Critical files & anchors

- `kubernetes/infrastructure/home/authentik/seed.yaml` — append after line ~196
  (graphana-oidc app block) before the MFA section; provider-before-app rule.
- `kubernetes/infrastructure/home/authentik/helmrelease.yaml` — lines 60-71
  `global.volumes`/`volumeMounts`: add langfuse secret volume at `/secrets-langfuse`.
- `kubernetes/infrastructure/home/authentik/oidc-grafana.sops.yaml` — template for
  the new `oidc-langfuse.sops.yaml` (same recipients/shape).
- `kubernetes/infrastructure/home/langfuse/helmrelease.yaml` — confirms the exact
  `AUTH_AUTHENTIK_*` values the provider/app must match (client_id `langfuse-oidc`,
  issuer `.../o/langfuse`).
- `kubernetes/infrastructure/home/phoenix/httproute-edge.yaml` + `vm-scrapes.yaml` —
  the patterns to mirror for steps 2 and 3.

## Verification

Working dir: `/Users/ngoldack/Projects/homelab`. Env: `SOPS_AGE_KEY_FILE=age.key`
(token of memory convention) for encrypt/decrypt.

1. **Secret match** — after step 1: `sops -d langfuse-secret.sops.yaml |
   grep AUTH_AUTHENTIK_CLIENT_SECRET` and `sops -d
   authentik/oidc-langfuse.sops.yaml | grep client_secret` — the two plaintext
   values MUST be byte-identical (they are the OIDC handshake secret).
2. **Render** — `kustomize build kubernetes/infrastructure/home/langfuse` and
   `kustomize build kubernetes/infrastructure/home/authentik` both succeed
   (langfuse build validates the new VMServiceScrape + HTTPRoute render).
3. **Blueprint import** — run the `Importer...validate()` check in step 1.6 on the
   updated seed; then after apply, `BlueprintInstance.status` → `successful` and the
   provider/app exist (`ak shell` query, or Authentik UI under Applications/Providers).
4. **task check** — `task check` green (repo convention before push).
5. **Live SSO** — commit+push all changes, then:
   `flux reconcile source git flux-system && flux reconcile kustomization authentik
   -n flux-system --with-source` (+ `rollout restart` per step 1.6), then
   `flux reconcile kustomization langfuse -n flux-system --with-source`.
   Browser: open `https://langfuse.ngoldack.de` → must redirect to authentik
   login → login as akadmin → redirected back into the Langfuse UI (this proves
   provider secret match + app binding + edge route end-to-end).
6. **VM metrics** — confirm the target is up:
   `vmselect`/vmagent query, e.g. port-forward the vmagent/vmsingle service and
   `curl '.../api/v1/query?query=up{service=~"langfuse.*"}'` returns a 1, and
   `curl` the scrape URL directly `/api/public/metrics` from inside the cluster
   (kubectl exec) returns `# TYPE` lines. (Exact service name for vmagent/vmsingle
   discovered at run time; any single sample for a `langfuse`-labeled `up` metric
   with value 1 proves the scrape.)

## Assumptions & contingencies

- The current `AUTH_AUTHENTIK_CLIENT_SECRET` in `langfuse-secret.sops.yaml` is the
  intended OIDC secret; we reuse it rather than rotate. If step 1 decryption fails
  (age key missing/wrong), do NOT invent a value — regenerate a fresh secret and
  write the SAME new value to both `langfuse-secret.sops.yaml`
  (`AUTH_AUTHENTIK_CLIENT_SECRET`) and `oidc-langfuse.sops.yaml` (`client_secret`),
  then reapply the langfuse HelmRelease so web/worker pick it up.
- Metrics path `/api/public/metrics` on port 3000 is the langfuse-documented
  Prometheus endpoint; if the direct scrape returns 404 (chart-version quirk),
  fall back to scraping the readiness `livenessProbe` target path seen in
  `templates/web/deployment.yaml` (its HTTP path is chart-known at run time) and
  update `path` accordingly — the `port: http` endpoint target stays.
- No VMRule/dashboard is added (repo has zero VMRule objects; vmalert ruleless by
  design). Scrape-only satisfies "metrics in vm".
- Edge external-dns auto-publishes `langfuse.ngoldack.de`; if it does not (edge DNS
  verified at run time), create the DNS record following the phoenix edge pattern
  (no `publish` label → edge instance owns it); flagged only if the SSO browser test
  fails on DNS resolution rather than auth.