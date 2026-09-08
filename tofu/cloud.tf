module "cloud_cluster" {
  source  = "hcloud-talos/talos/hcloud"
  version = "3.4.15"

  hcloud_token       = local.secrets["hcloud_api_token"]
  cluster_name       = "${var.cluster_name}-cloud"
  cluster_domain     = var.cloud_cluster_domain
  location_name      = var.cloud_ingress_location
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version
  disable_x86        = true

  # The cloud cluster is deliberately independent from the home cluster.
  # Its single control-plane node is also schedulable and has no workers.
  control_plane_nodes = [{
    id   = 1
    type = var.cloud_ingress_server_type
    labels = {
      "topology.kubernetes.io/zone" = "cloud"
      "workload/public-ingress"     = "true"
    }
  }]
  control_plane_allow_schedule = true
  worker_nodes                 = []

  firewall_use_current_ip = true
  deploy_cilium           = true
  cilium_version          = "1.19.5"
  cilium_values = [yamlencode({
    cluster = {
      name = "cloud"
      id   = 2
    }
    clustermesh = {
      useAPIServer = true
    }
  })]

}

output "cloud_ingress_ipv4" {
  description = "Public IPv4 address of the independent cloud cluster control plane"
  value       = module.cloud_cluster.public_ipv4_list[0]
}

output "cloud_ingress_ipv6" {
  description = "The cloud cluster is IPv4-only; retained for output compatibility"
  value       = null
}

output "cloud_cluster_kubeconfig" {
  description = "Kubeconfig for the independent cloud cluster"
  value       = module.cloud_cluster.kubeconfig
  sensitive   = true
}

output "cloud_cluster_talosconfig" {
  description = "Talosconfig for the independent cloud cluster"
  value       = module.cloud_cluster.talosconfig
  sensitive   = true
}
