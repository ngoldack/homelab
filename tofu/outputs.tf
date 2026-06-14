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

output "hetzner_dr_bucket" {
  description = "Name and S3 endpoint of the Hetzner Object Storage offsite DR bucket"
  value = {
    bucket   = aws_s3_bucket.dr.id
    endpoint = "https://${var.hetzner_objectstorage_location}.your-objectstorage.com"
    region   = var.hetzner_objectstorage_location
  }
}

output "edge_node" {
  description = "Hetzner edge worker node public addresses (point public DNS here)."
  value = {
    name = hcloud_server.edge.name
    ipv4 = hcloud_server.edge.ipv4_address
    ipv6 = hcloud_server.edge.ipv6_address
  }
}
