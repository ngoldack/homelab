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
  cilium-operator.json  grafana.com dashboard 15514 (Cilium Operator), rev 1
  valkey.json           grafana.com dashboard 763 (Redis Exporter 1.x), rev 6
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

Processing applied when vendoring, which must be repeated if these are
refreshed:
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
