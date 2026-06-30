## ADDED Requirements

### Requirement: Cilium is the exclusive networking layer
All cluster networking SHALL be provided by Cilium. No second CNI, kube-proxy, or service mesh SHALL be introduced.

#### Scenario: Cilium is the only CNI
- **WHEN** the cluster is running
- **THEN** Cilium is the sole CNI and pod-to-pod, pod-to-service, and node traffic are handled by Cilium
- **AND** kube-proxy is not running (Cilium kube-proxy replacement is active)

#### Scenario: New networking needs use Cilium features
- **WHEN** a later phase needs ingress, load balancing, network policy, or encryption
- **THEN** it SHALL be implemented with a Cilium feature (Gateway API, LB-IPAM, CiliumNetworkPolicy, WireGuard/transparent encryption) rather than a new component

### Requirement: Cilium observability and L2/L7 features enabled
Cilium SHALL be installed with Hubble, LB-IPAM, and Gateway API support enabled so later phases consume them without reconfiguration.

#### Scenario: Hubble flow visibility
- **WHEN** Cilium is deployed
- **THEN** Hubble is enabled and pod-to-pod flows are observable

#### Scenario: Load-balancer IP allocation available
- **WHEN** a Service of type LoadBalancer is created
- **THEN** Cilium LB-IPAM assigns it an address from a configured pool
