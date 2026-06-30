## ADDED Requirements

### Requirement: Metrics collection and storage
The system SHALL collect cluster and host metrics and store them in VictoriaMetrics.

#### Scenario: Metrics are queryable
- **WHEN** the monitoring stack is running
- **THEN** node and pod metrics are scraped and queryable from VictoriaMetrics

### Requirement: Aggregated log collection
The system SHALL ship all container logs from every node to Loki via a node-local collector.

#### Scenario: Logs from every node reach Loki
- **WHEN** the log collector DaemonSet is deployed
- **THEN** it runs on every node including tainted ones (control-plane, GPU)
- **AND** container logs from those nodes are queryable in Loki

### Requirement: Dashboards
The system SHALL provide Grafana with VictoriaMetrics and Loki datasources and at least one aggregated logs dashboard and one cluster-metrics dashboard.

#### Scenario: Operator can view logs and metrics
- **WHEN** an operator opens Grafana
- **THEN** the VictoriaMetrics and Loki datasources are provisioned
- **AND** an "all container logs" view and a cluster-overview metrics dashboard are available
