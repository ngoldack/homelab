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
  node-exporter-full    grafana.com dashboard 1860, revision 37

Processing applied when vendoring, which must be repeated if these are
refreshed:
  - `__inputs` / `__requires` stripped. grafana.com exports use these to prompt
    for a datasource at import time; the Grafana sidecar performs no such
    prompt, so a dashboard keeping them imports with every panel unbound.
  - `${DS_PROMETHEUS}` rewritten to `VictoriaMetrics`, the datasource name and
    uid this stack provisions (type prometheus, isDefault).
  - `id` removed so Grafana assigns its own; a baked-in id collides on import.

They are surfaced by the Grafana sidecar, which watches for ConfigMaps
labelled `grafana_dashboard: "1"` — see kustomization.yaml, which generates one
ConfigMap per file with that label. Adding a dashboard is: drop the JSON here,
add it to the generator.
