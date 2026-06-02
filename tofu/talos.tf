# Generate Talos cluster-wide machine secrets
resource "talos_machine_secrets" "this" {
  talos_version = var.talos_version
}

# Generate control plane machine configuration
data "talos_machine_configuration" "controlplane" {
  cluster_name       = var.cluster_name
  cluster_endpoint   = var.cluster_endpoint
  machine_type       = "controlplane"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  # Patch control plane nodes to:
  # 1. Disable the default Flannel CNI (so we can use Cilium)
  # 2. Disable default kube-proxy (so Cilium can run in strict eBPF mode with host services)
  # 3. Install cluster-wide default extensions (netbird, nvme-cli, iscsi-tools, nfs-utils)
  config_patches = concat(
    [
      # Disable default Flannel CNI
      yamlencode({
        cluster = {
          network = {
            cni = {
              name = "none"
            }
          }
        }
      }),
      # Disable kube-proxy in favor of Cilium kube-proxy replacement
      yamlencode({
        cluster = {
          proxy = {
            disabled = true
          }
        }
      }),
      # Enable extra kubelet mounts for Tetragon eBPF telemetry & tracing
      yamlencode({
        machine = {
          kubelet = {
            extraMounts = [
              {
                hostPath  = "/sys/kernel/tracing"
                mountPath = "/sys/kernel/tracing"
                readOnly  = false
              },
              {
                hostPath  = "/sys/kernel/btf"
                mountPath = "/sys/kernel/btf"
                readOnly  = true
              },
              {
                hostPath  = "/sys/fs/bpf"
                mountPath = "/sys/fs/bpf"
                readOnly  = false
              },
              {
                hostPath  = "/run/containerd/containerd.sock"
                mountPath = "/run/containerd/containerd.sock"
                readOnly  = true
              }
            ]
          }
        }
      }),
    ],
    length(var.talos_default_extensions) > 0 ? [
      yamlencode({
        machine = {
          install = {
            extensions = [
              for img in var.talos_default_extensions : { image = img }
            ]
          }
        }
      }),
    ] : []
  )
}

# Per-pool worker machine configuration
# Each pool gets its own config so gVisor can be toggled independently per pool.
data "talos_machine_configuration" "worker" {
  for_each = { for k, v in var.node_pools : k => v if v.talos_role == "worker" }

  cluster_name       = var.cluster_name
  cluster_endpoint   = var.cluster_endpoint
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  config_patches = concat(
    [
      # Disable default Flannel CNI (Cilium replaces it)
      yamlencode({
        cluster = { network = { cni = { name = "none" } } }
      }),
      # Disable kube-proxy (Cilium runs in strict eBPF kube-proxy replacement mode)
      yamlencode({
        cluster = { proxy = { disabled = true } }
      }),
      # Enable extra kubelet mounts for Tetragon eBPF telemetry & tracing
      yamlencode({
        machine = {
          kubelet = {
            extraMounts = [
              {
                hostPath  = "/sys/kernel/tracing"
                mountPath = "/sys/kernel/tracing"
                readOnly  = false
              },
              {
                hostPath  = "/sys/kernel/btf"
                mountPath = "/sys/kernel/btf"
                readOnly  = true
              },
              {
                hostPath  = "/sys/fs/bpf"
                mountPath = "/sys/fs/bpf"
                readOnly  = false
              },
              {
                hostPath  = "/run/containerd/containerd.sock"
                mountPath = "/run/containerd/containerd.sock"
                readOnly  = true
              }
            ]
          }
        }
      }),
    ],
    # System extensions — merges cluster-wide defaults with per-pool extras.
    # Extensions are baked into Talos during installation (first boot from ISO).
    length(concat(var.talos_default_extensions, each.value.extensions)) > 0 ? [
      yamlencode({
        machine = {
          install = {
            extensions = [
              for img in concat(var.talos_default_extensions, each.value.extensions) : { image = img }
            ]
          }
        }
      }),
    ] : [],
    # Node taints — applied at kubelet registration via machine.nodeTaints.
    # Format: key => "value:effect" (empty value is allowed: ":NoSchedule").
    length(each.value.taints) > 0 ? [
      yamlencode({
        machine = {
          nodeTaints = {
            for t in each.value.taints : t.key => "${t.value}:${t.effect}"
          }
        }
      }),
    ] : []
  )
}

# Config apply logic for control plane nodes
resource "talos_machine_configuration_apply" "controlplane" {
  for_each                    = { for k, v in local.vm_instances : k => v if v.talos_role == "controlplane" }
  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.controlplane.machine_configuration
  node                        = proxmox_virtual_environment_vm.talos_nodes[each.key].ipv4_addresses[1][0]
}

# Config apply logic for worker nodes — selects the per-pool config by pool_name
resource "talos_machine_configuration_apply" "worker" {
  for_each                    = { for k, v in local.vm_instances : k => v if v.talos_role == "worker" }
  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.worker[each.value.pool_name].machine_configuration
  node                        = proxmox_virtual_environment_vm.talos_nodes[each.key].ipv4_addresses[1][0]
}

# Talos client configuration (used to construct talosconfig)
data "talos_client_configuration" "this" {
  cluster_name         = var.cluster_name
  client_configuration = talos_machine_secrets.this.client_configuration
  endpoints            = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"]
}

# Bootstrap the cluster on the first master node
resource "talos_machine_bootstrap" "this" {
  depends_on           = [talos_machine_configuration_apply.controlplane]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"][0]
}

# Retrieve kubeconfig from the bootstrapped cluster
resource "talos_cluster_kubeconfig" "this" {
  depends_on           = [talos_machine_bootstrap.this]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"][0]
}
