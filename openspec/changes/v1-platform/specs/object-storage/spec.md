## ADDED Requirements

### Requirement: In-cluster S3 via the SeaweedFS operator
The system SHALL provide in-cluster, S3-compatible object storage using the SeaweedFS operator (https://github.com/seaweedfs/seaweedfs-operator), and each application needing object storage SHALL get its own bucket and credentials (no shared buckets). MinIO and Crossplane SHALL NOT be used. This in-cluster S3 is distinct from the `offsite-backup` capability, which targets a remote bucket for disaster recovery.

#### Scenario: App gets a dedicated bucket and credentials
- **WHEN** an application declares its object-storage resources (bucket + credentials)
- **THEN** a dedicated bucket and a generated S3 credential Secret are provisioned for that app
- **AND** the credentials grant access only to that app's bucket

#### Scenario: In-cluster S3 endpoint is reachable
- **WHEN** a workload addresses the in-cluster SeaweedFS S3 endpoint
- **THEN** it can read and write objects in buckets it owns using its credentials
