## ADDED Requirements

### Requirement: Offsite backup to Hetzner S3
The system SHALL back up cluster and database state to a remote Hetzner S3 bucket on a schedule, and backups SHALL be restorable.

#### Scenario: Scheduled backup completes
- **WHEN** the backup schedule fires
- **THEN** a backup object is written to the Hetzner S3 bucket
- **AND** the backup status reports success

#### Scenario: Restore is possible
- **WHEN** an operator restores from a backup object
- **THEN** the targeted database/state is recovered to the backup point
