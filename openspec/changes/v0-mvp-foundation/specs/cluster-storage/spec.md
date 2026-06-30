## ADDED Requirements

### Requirement: Default StorageClass for persistent workloads
The cluster SHALL provide at least one CSI-backed StorageClass usable for persistent volumes (model weights, monitoring data).

#### Scenario: PVC binds
- **WHEN** a PersistentVolumeClaim is created against the default StorageClass
- **THEN** a PersistentVolume is dynamically provisioned and the claim becomes `Bound`

#### Scenario: Model weights survive a restart
- **WHEN** the llama.cpp pod is restarted
- **THEN** its model weights remain on the PVC and are not re-downloaded
