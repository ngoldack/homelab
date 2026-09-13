# homelab-infra-metrics — external infra → VictoriaMetrics

Push endpoint: **`http://10.30.0.202:8428`** (HTTP), graphite plaintext
**`10.30.0.202:2003`**. Both are the `vmsingle-push` LoadBalancer Service
(`push-endpoint.yaml`); `.202` is a Cilium-announced LAN VIP reachable from
every host on `10.30.0.0/24` with no VPN/Hetzner hop.

## Required vmsingle change (integration owner)

The graphite plaintext listener is OFF by default. Add this to
`monitoring/helmrelease.yaml` (do not touch anything else there) so TrueNAS'
graphite export has a listener on `:2003`:

```yaml
vmsingle:
  spec:
    extraArgs:
      graphiteListenAddr: ":2003"
```

(The equivalent single-node flag is `--graphiteListenAddr=:2003`. Influx,
graphite-HTTP and Prometheus remote_write already answer on `:8428` with no
extra config.) The push-endpoint Service already forwards `:2003`; this
extraArg is the only remaining piece.

## (a) Proxmox VE → InfluxDB

Datacenter → Metric Server, Add → **InfluxDB**, then:

```text
Server:      http://10.30.0.202:8428/influx/write?db=proxmox
Port:        8428
Organization: (leave empty — VictoriaMetrics ignores it)
Bucket/DB:    proxmox
Protocol:     HTTP
```

Proxmox emits InfluxDB **line protocol**; VictoriaMetrics accepts it natively
at `/influx/write`. No token (`--influxAuthToken` unset). The `db=proxmox`
query string namespaces metrics under `proxmox_*` (e.g. `proxmox_cpu_usage`).

## (b) Proxmox Backup Server (PBS) → InfluxDB

PBS → Administration → **Metric Server**, choose **InfluxDB**:

```text
Server:      http://10.30.0.202:8428/influx/write?db=pbs
Port:        8428
Database:    pbs
```

Same line-protocol path as Proxmox; only the `db=` value differs so the two
stay separable in VM (`pbs_*`).

## (c) TrueNAS → Graphite

System Settings → **Reporting** → Exporters → Add → **Graphite**:

```text
Reporting:   Graphite
Server Host: 10.30.0.202
Port:        2003
Prefix:      homelab-truenas
```

Metrics land as `graphite.homelab_truenas.<host>.<metric>`. Requires the
`graphiteListenAddr` extraArg above, otherwise the connection is refused.

## (d) UDM Pro → unpoller (in-cluster)

Handled by the `unpoller` HelmRelease + `vm-scrapes.yaml` here — no config on
the UDM beyond creating a local read-only admin:

1. Fill the `url`/`user`/`pass` placeholders in `unifi.sops.yaml` (decrypt,
   edit; the file comments contain the exact "Local Access Only" admin steps).
   The controller URL is `https://10.30.0.1` by default.
2. unpoller exposes `/metrics` on `:9130`; vmagent scrapes it automatically.

## OTel posture — do NOT add an OTel Collector

No OpenTelemetry Collector for metrics. VictoriaMetrics speaks InfluxDB line
protocol, Graphite (plaintext + HTTP), and Prometheus remote_write natively —
every source above already talks one of those, so a Collector would only add
an extra hop and a second config surface to keep right, for metrics VM already
accepts directly. Reserve OTel for the one thing VM does not do: LLM **trace**
observability (the Phoenix deployment later), where the OTLP span format is
the actual integration point.