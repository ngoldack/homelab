# Generate Talos cluster-wide machine secrets
resource "talos_machine_secrets" "this" {
  talos_version = var.talos_version
}

# Generate control plane machine configuration
data "talos_machine_configuration" "controlplane" {
  cluster_name     = var.cluster_name
  cluster_endpoint = var.cluster_endpoint
  machine_type     = "controlplane"
  machine_secrets  = talos_machine_secrets.this.machine_secrets
  talos_version    = var.talos_version

  # Patch control plane nodes to:
  # 1. Disable the default Flannel CNI (so we can use Cilium)
  # 2. Disable default kube-proxy (so Cilium can run in strict eBPF mode with host services)
  config_patches = [
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
    })
  ]
}

# Generate worker machine configuration
data "talos_machine_configuration" "worker" {
  cluster_name     = var.cluster_name
  cluster_endpoint = var.cluster_endpoint
  machine_type     = "worker"
  machine_secrets  = talos_machine_secrets.this.machine_secrets
  talos_version    = var.talos_version

  # Patch worker nodes to:
  # 1. Disable the default Flannel CNI
  # 2. Disable default kube-proxy
  config_patches = [
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
    })
  ]
}

# Config apply logic for control plane nodes
resource "talos_machine_configuration_apply" "controlplane" {
  for_each                    = { for k, v in local.vm_instances : k => v if v.talos_role == "controlplane" }
  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.controlplane.machine_config
  node                        = proxmox_virtual_environment_vm.talos_nodes[each.key].ipv4_addresses[1][0]
}

# Config apply logic for worker nodes
resource "talos_machine_configuration_apply" "worker" {
  for_each                    = { for k, v in local.vm_instances : k => v if v.talos_role == "worker" }
  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.worker.machine_config
  node                        = proxmox_virtual_environment_vm.talos_nodes[each.key].ipv4_addresses[1][0]
}

# Talos client configuration (used to construct talosconfig)
data "talos_client_configuration" "this" {
  cluster_name         = var.cluster_name
  client_configuration = talos_machine_secrets.this.client_configuration
  endpoints            = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"]
}

# Bootstrap the cluster on the first master node
resource "talos_cluster_bootstrap" "this" {
  depends_on           = [talos_machine_configuration_apply.controlplane]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"][0]
}

# Retrieve kubeconfig from the bootstrapped cluster
resource "talos_cluster_kubeconfig" "this" {
  depends_on           = [talos_cluster_bootstrap.this]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = [for k, v in local.vm_instances : proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses[1][0] if v.talos_role == "controlplane"][0]
}
