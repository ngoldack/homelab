## ADDED Requirements

### Requirement: Talos cluster on the home main host
The system SHALL provision four Talos VMs on the Proxmox VE host `pmx-main` (home site) and bootstrap a cluster: a dedicated control plane (`cp`), a general worker (`wk`), a GPU worker (`wk-gpu`, P100 passthrough), and a dedicated CPU-inference worker (`wk-cpu`, tainted). The home host is always Proxmox; Talos is never installed bare-metal.

#### Scenario: Cluster reachable after bootstrap
- **WHEN** the Talos machine configs are applied and the cluster is bootstrapped
- **THEN** `kubectl get nodes` reports all four nodes in `Ready` state
- **AND** every node is labelled with its site (`site=home`)

#### Scenario: Talos defers networking to Cilium
- **WHEN** the machine config is generated
- **THEN** the built-in CNI is set to `none` and kube-proxy is disabled

### Requirement: Dedicated Kubernetes node subnet
The Talos nodes SHALL live on a dedicated VLAN-tagged subnet with static IP addresses, isolated from the management LAN. The nodes' primary NIC is tagged with the k8s VLAN; the router owns the VLAN gateway and a maintenance DHCP scope (for the install-ISO boot before the static config applies).

#### Scenario: Nodes on the dedicated subnet with static IPs
- **WHEN** the VMs are provisioned and the Talos config is applied
- **THEN** each node's primary NIC is tagged with the k8s VLAN and carries its assigned static IP
- **AND** the control-plane endpoint resolves to the control plane's static IP on that subnet

### Requirement: GPU enablement for the Tesla P100
The system SHALL pass the host's NVIDIA Tesla P100 through to the Talos VM (Proxmox PCIe passthrough) and expose it to Kubernetes as a schedulable `nvidia.com/gpu` resource using a Pascal-supporting driver matching `580.159.04`.

#### Scenario: GPU advertised to the scheduler
- **WHEN** the P100 is passed through and the NVIDIA Talos extensions, kernel modules, and device plugin are deployed
- **THEN** the node advertises `nvidia.com/gpu: 1`
- **AND** a pod requesting `nvidia.com/gpu` can run `nvidia-smi` and see the P100

### Requirement: Reproducible infrastructure as code
The infrastructure SHALL be declared in OpenTofu and validate without credentials, with every resource tagged by site.

#### Scenario: Tofu validates
- **WHEN** `tofu init -backend=false && tofu validate` runs in `tofu/`
- **THEN** validation succeeds
