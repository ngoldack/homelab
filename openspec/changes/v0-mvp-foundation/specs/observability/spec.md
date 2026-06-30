## ADDED Requirements

### Requirement: Metrics on VictoriaMetrics
The system SHALL collect cluster and host metrics and store them in VictoriaMetrics.

#### Scenario: Metrics are queryable
- **WHEN** the monitoring stack is running
- **THEN** node and pod metrics are scraped and queryable from VictoriaMetrics

### Requirement: Logs on VictoriaLogs
The system SHALL ship all container logs from every node to VictoriaLogs (not Loki) via a node-local collector.

#### Scenario: Logs from every node reach VictoriaLogs
- **WHEN** the log collector DaemonSet is deployed
- **THEN** it runs on every node including tainted ones (control-plane, GPU)
- **AND** container logs from those nodes are queryable in VictoriaLogs (LogsQL)

### Requirement: Traces on VictoriaTraces
The system SHALL accept OTLP traces and store them in VictoriaTraces (not Tempo).

#### Scenario: Traces are stored and viewable
- **WHEN** a workload emits OTLP traces
- **THEN** they are ingested by VictoriaTraces and viewable in Grafana

### Requirement: Grafana dashboards over the VictoriaMetrics stack
The system SHALL provide Grafana with VictoriaMetrics, VictoriaLogs, and VictoriaTraces datasources, plus an aggregated "all container logs" view and a cluster-overview metrics dashboard.

#### Scenario: Operator can view metrics, logs, and traces
- **WHEN** an operator opens Grafana
- **THEN** the VictoriaMetrics, VictoriaLogs, and VictoriaTraces datasources are provisioned
- **AND** an "all container logs" view and a cluster-overview dashboard are available
