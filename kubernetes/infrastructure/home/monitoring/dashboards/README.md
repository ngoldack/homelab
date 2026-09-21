# Vendored Grafana dashboards

These are committed as files rather than fetched at reconcile time, and that
is the whole point.

The victoria-metrics-k8s-stack chart can supply dashboards itself via
`defaultDashboards.enabled`, but it does so with a sync-job that downloads
JSON over the network on every install and upgrade from unpinned branch tips
(`.../master/dashboards/...`). That makes the cluster non-reproducible — two
applies a month apart can yield different dashboards from identical git state
— and it fails the whole HelmRelease if GitHub is unreachable, because the
fetch is a Helm hook. So `defaultDashboards` is off and these live here.

Sources, all pinned:
  victoriametrics.json  VictoriaMetrics/VictoriaMetrics @ v1.151.0 (matches the
                        appVersion of the chart we run)
  vmagent.json          same tag
  vmalert.json          same tag
  node-exporter-full    grafana.com dashboard 1860, revision 45
  k8s-views-global      dotdc/grafana-dashboards-kubernetes @ v3.0.6
                        (k8s-views-global, uid k8s_views_global)
  k8s-views-pods        dotdc/grafana-dashboards-kubernetes @ v3.0.6
                        (k8s-views-pods, uid k8s_views_pods)
  cnpg.json             grafana.com dashboard 20417 (CloudNativePG), revision 4
  external-dns.json     grafana.com dashboard 15038 (External DNS), revision 3
  cert-manager.json     grafana.com dashboard 11001 (cert-manager), revision 1
  cilium-agent.json     grafana.com dashboard 15513 (Cilium Agent Metrics), rev 1
  hubble.json    Cilium v1.20.1 dashboard (chart-shipped @ install/kubernetes/cilium/files/hubble/dashboards/hubble-dashboard.json), from helm chart cilium v1.20.1
  cilium-operator.json  grafana.com dashboard 15514 (Cilium Operator), rev 1
  valkey.json           grafana.com dashboard 763 (Redis Exporter 1.x), rev 6
  kyverno.json          kyverno/kyverno @ v1.19.1 (chart 3.9.1) —
                        charts/kyverno/charts/grafana/dashboard/kyverno-dashboard.json
                        (uid Rg8lWBG7k). Coverage caveat recorded in
                        kustomization.yaml: this build exports 6 of the 14
                        kyverno_* families the dashboard queries, so the
                        per-policy-type panels stay empty.
  crowdsec.json         crowdsecurity/grafana-dashboards @ 0d34f686
                        (dashboards_v5/Crowdsec Overview.json, uid hjmZdB4nk).
                        Caveat: v1.8.1 here exports cs_info,
                        cs_filesource/node_hits/parser_hits only —
                        cs_alerts/cs_active_decisions/cs_buckets do not exist,
                        so those panels stay empty.
  flux.json             fluxcd/flux2-monitoring-example control-plane.json
                        (official Flux Control Plane dashboard, pinned at its
                        main-branch state when vendored)
  immich.json           hand-authored (no official dashboard) — OTel HTTP
                        server metrics from IMMICH_TELEMETRY_INCLUDE
  zot.json              hand-authored (no official dashboard) — zot_* metrics
  llamacpp.json         hand-authored (no official dashboard) — llamacpp:*
                        metrics from llama.cpp server --metrics
  authentik.json        hand-authored (no official dashboard) — django-
                        prometheus process metrics on the :9300 metrics server
  synthetic-proxy.json  hand-authored (no official dashboard) — the
                        synthetic_proxy_* metrics from image-builds/
                        synthetic-proxy, the per-key quota/health gate in
                        front of api.synthetic.new: requests by verdict,
                        upstream responses by code, the request-latency
                        histogram, refusals as a percentage of traffic,
                        quota-check outcomes, keys tracked and lifetime
                        requests. Every expression was run against the live
                        VictoriaMetrics query API before commit and the
                        numbers observed at that time are recorded in each
                        panel's description. Two coverage caveats recorded
                        there: synthetic_proxy_probes_total does not exist
                        until the reachability probe runs (it fires only when
                        the quota endpoint call fails), so that panel is
                        empty in steady state by design; and the
                        verdict="quota_exhausted"/"rate_limited" and code="429"
                        series appear only once the proxy has seen such an
                        answer, so they can be absent rather than zero.
  survivability.json    hand-authored (no official dashboard), uid
                        homelab-survivability, schemaVersion 39 — the cluster
                        survivability set: backup ages (talos-backup etcd
                        CronJob + CNPG/barman last-available-backup per
                        cluster), minimum certificate validity, node/Flux/
                        GitOps health, monitoring + alert-delivery self-health
                        (up, alertmanager notifications/failures, vmalert rule
                        errors), CNPG WAL archive backlog, and node /var
                        filesystem pressure. Every panel expression was run
                        against the live VictoriaMetrics query API before
                        commit; the numbers observed at that time are recorded
                        in each panel's description, and two panels at the
                        bottom document what could NOT be built (Flux 2.9.x
                        exports neither gotk_reconcile_condition nor
                        gotk_resource_info, and the unpoller target is down, so
                        no TrueNAS pool series exist).

Processing applied when vendoring, which must be repeated if these are
refreshed (the hand-authored dashboards above follow the same rules — for
survivability.json the expressions, not the JSON, are the reviewed artefact,
so any edit must re-run each expression against VictoriaMetrics):
  - `__inputs` / `__requires` stripped. grafana.com exports use these to prompt
    for a datasource at import time; the Grafana sidecar performs no such
    prompt, so a dashboard keeping them imports with every panel unbound.
  - `${DS_PROMETHEUS}` rewritten to `VictoriaMetrics`, the datasource name and
    uid this stack provisions (type prometheus, isDefault). Any remaining
    `datasource` references (string or object form) are normalized to
    `{"type":"prometheus","uid":"VictoriaMetrics"}`, and templating variables
    of `type: datasource` are dropped (they are import prompts, like
    `__inputs`). EXCEPTION: `annotations.list[]` entries keep their built-in
    Grafana datasource `{"type":"datasource","uid":"grafana"}` (the "Annotations
    & Alerts"/tag queries run against the internal datasource, not Prometheus;
    binding them to VictoriaMetrics makes every annotation query error on load).
  - `id` removed so Grafana assigns its own; a baked-in id collides on import.

They are surfaced by the Grafana sidecar, which watches for ConfigMaps
labelled `grafana_dashboard: "1"` — see kustomization.yaml, which generates one
ConfigMap per file with that label. Adding a dashboard is: drop the JSON here,
add it to the generator.
