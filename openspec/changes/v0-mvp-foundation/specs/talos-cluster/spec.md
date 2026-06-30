## ADDED Requirements

### Requirement: Single-node Talos cluster on the homelab main host
The system SHALL provision a Talos OS Kubernetes node as a virtual machine on the Proxmox VE host `pmx-main` (homelab site) and bootstrap a single-node cluster that schedules workloads on the control plane. The homelab host is always Proxmox; Talos is never installed bare-metal.

#### Scenario: Cluster reachable after bootstrap
- **WHEN** the Talos machine config is applied and the cluster is bootstrapped
- **THEN** `kubectl get nodes` reports `pmx-main` in `Ready` state
- **AND** the node is labelled with its site (`site=homelab`)

#### Scenario: Talos defers networking to Cilium
- **WHEN** the machine config is generated
- **THEN** the built-in CNI is set to `none` and kube-proxy is disabled

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
