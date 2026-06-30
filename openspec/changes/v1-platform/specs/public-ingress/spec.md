## ADDED Requirements

### Requirement: Public ingress via the Hetzner cloud node
External (non-LAN) traffic SHALL enter through a Talos node at the `cloud` site (public IP) and be routed to homelab services using Cilium Gateway API.

#### Scenario: External request reaches a homelab service
- **WHEN** a client outside the LAN requests a published hostname
- **THEN** the request enters via the cloud node and is served by the homelab backend over TLS

### Requirement: Encrypted homelab-cloud link
Traffic between the `cloud` and `homelab` sites SHALL be encrypted using Cilium.

#### Scenario: Inter-site traffic is encrypted
- **WHEN** the cloud node forwards traffic to homelab
- **THEN** that traffic traverses a Cilium-encrypted path
