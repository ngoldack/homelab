## ADDED Requirements

### Requirement: Per-app Postgres and Valkey via operators
The system SHALL provide CloudNativePG and Valkey operators, and each application needing a database or cache SHALL get its own instance and credentials (no shared instances).

#### Scenario: App gets a dedicated database
- **WHEN** an application declares a CloudNativePG Cluster
- **THEN** a dedicated Postgres instance with its own credentials is provisioned for that app

#### Scenario: Database backups run
- **WHEN** a CloudNativePG instance has a scheduled backup
- **THEN** its WAL + base backups are shipped to the Hetzner S3 bucket
