## ADDED Requirements

### Requirement: Four storage tiers via TrueNAS CSI
The cluster SHALL expose the four existing TrueNAS CSI StorageClasses, with `standard` (`tns-fast-nfs`) as the cluster default. Each workload SHALL use the tier matching its need:
- `fast` (`tns-fast-nvmeof`, NVMe-oF block) — only where it materially helps (databases).
- `standard` (`tns-fast-nfs`, NFS) — the default for everything.
- `storage` (`tns-tank-nfs`, HDD/NFS) — huge media libraries only.
- `local` (`local-path`, node-local) — temporary/scratch only.

#### Scenario: Default tier provisions
- **WHEN** a PVC is created without specifying a StorageClass
- **THEN** it is provisioned on `tns-fast-nfs` (standard) and becomes `Bound`

#### Scenario: Tier chosen per workload
- **WHEN** a database requests `tns-fast-nvmeof` (fast)
- **THEN** it is provisioned on the NVMe-oF block tier

### Requirement: Persistent model weights
Model weights SHALL persist across restarts on an appropriate tier (standard or fast), not be re-downloaded.

#### Scenario: Weights survive a restart
- **WHEN** the llama.cpp pod is restarted
- **THEN** its weights remain on the PVC and are not re-downloaded
