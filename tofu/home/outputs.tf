output "talosconfig" {
  description = "The generated talosconfig client state file"
  value       = data.talos_client_configuration.this.talos_config
  sensitive   = true
}

output "kubeconfig" {
  description = "The bootstrapped Kubernetes kubeconfig file content"
  value       = talos_cluster_kubeconfig.this.kubeconfig_raw
  sensitive   = true
}

output "node_ips" {
  description = "Assigned IP addresses of all cluster nodes in Proxmox"
  value = {
    for k, v in local.vm_instances : k => {
      role = v.talos_role
      ips  = proxmox_virtual_environment_vm.talos_nodes[k].ipv4_addresses
    }
  }
}

output "cluster_capacity" {
  description = "Summary of total allocated resources across the cluster"
  value       = local.cluster_capacity
}

output "machine_secrets" {
  description = "Cluster machine secrets — sensitive"
  value       = talos_machine_secrets.this.machine_secrets
  sensitive   = true
}

output "client_configuration" {
  description = "Cluster client configuration — sensitive"
  value       = talos_machine_secrets.this.client_configuration
  sensitive   = true
}

output "cluster_endpoint" {
  description = "Kubernetes API endpoint URL"
  value       = local.cluster_endpoint
  sensitive   = true
}

output "node_cpu_affinity" {
  description = "Resolved Proxmox cpu.affinity per node — the actual host threads tofu pinned each node's vCPUs to, derived from cpu_class (or the explicit cpu_affinity override)"
  value       = local.node_affinity
}
