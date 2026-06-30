## ADDED Requirements

### Requirement: Offsite single-node cluster joined to the mesh
The system SHALL run a single-node Talos cluster on a TrueNAS Scale VM (6GB, control plane + workloads) and join it to the Cilium Cluster Mesh as cluster id 3.

#### Scenario: Offsite node ready
- **WHEN** the TrueNAS VM is provisioned and Talos is bootstrapped
- **THEN** the offsite cluster has one `Ready` node running its own Cilium + Flux

#### Scenario: Offsite joins the existing mesh
- **WHEN** the offsite cluster (id 3) is connected
- **THEN** `cilium clustermesh status` on home and cloud shows offsite connected
- **AND** no change to the home/cloud cluster.id/name was required

### Requirement: Minimal DR footprint
The offsite cluster SHALL run only the minimal set needed for disaster recovery / remote presence, not the LLM or full app stack.

#### Scenario: Offsite stays small
- **WHEN** workloads are assigned to offsite
- **THEN** only DR-relevant components (mesh + a minimal critical/global set) run there, fitting the 6GB box
