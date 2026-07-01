## ADDED Requirements

### Requirement: Cilium Cluster Mesh joins the clusters
The home and cloud clusters SHALL be joined by Cilium Cluster Mesh: each cluster has a unique `cluster.id` and `cluster.name`, a shared/trusted CA, and a reachable `clustermesh-apiserver`.

#### Scenario: Clusters are meshed
- **WHEN** the mesh is established between home (id 1) and cloud (id 2)
- **THEN** `cilium clustermesh status` reports both clusters connected and healthy

#### Scenario: Unique identities
- **WHEN** a cluster is added to the mesh
- **THEN** it uses a cluster.id/name not used by any other cluster (home=1, cloud=2, offsite=3)

### Requirement: Cross-cluster service discovery
A Service SHALL be reachable from another meshed cluster when marked global.

#### Scenario: Global service resolves cross-cluster
- **WHEN** a Service is annotated `service.cilium.io/global: "true"`
- **THEN** workloads in the other meshed cluster can resolve and reach it by its cluster DNS name

### Requirement: Mesh is extensible to offsite
The mesh design SHALL allow a third cluster (offsite, id 3) to join later without re-architecting.

#### Scenario: Offsite can join later
- **WHEN** the offsite cluster is provisioned (v3)
- **THEN** it joins the existing mesh by adding cluster.id 3 + the shared CA, no change to home/cloud
