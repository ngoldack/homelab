locals {
  controlplane_instances = { for name, node in var.nodes : name => node if node.talos_role == "controlplane" }
  worker_instances       = { for name, node in var.nodes : name => node if node.talos_role == "worker" }

  apply_address = { for name, node in local.talos_nodes : name => node.current_ip }
  bootstrap_address = {
    for name, node in local.talos_nodes : name => coalesce(node.ip, node.current_ip)
  }
  controlplane_bootstrap_addresses = [
    for name in keys(local.controlplane_instances) : local.bootstrap_address[name]
  ]
}

resource "talos_machine_secrets" "this" {
  talos_version = var.talos_version

  lifecycle {
    prevent_destroy = true
  }
}

data "talos_machine_configuration" "controlplane" {
  cluster_name       = var.cluster_name
  cluster_endpoint   = local.cluster_endpoint
  machine_type       = "controlplane"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  config_patches = concat(
    [
      yamlencode({
        cluster = {
          network = {
            cni            = { name = "none" }
            podSubnets     = [var.pod_cidr]
            serviceSubnets = [var.service_cidr]
          }
          proxy = { disabled = true }
        }
      }),
      yamlencode({
        machine = {
          install = {
            diskSelector = { size = ">= 10GB" }
          }
          nodeLabels = { "site" = "home" }
        }
      }),
      yamlencode({
        machine = {
          network = {
            kubespan = {
              enabled                     = true
              advertiseKubernetesNetworks = true
            }
          }
        }
      }),
    ],
    length(var.talos_default_extensions) > 0 ? [
      yamlencode({
        machine = {
          install = {
            extensions = [
              for image in var.talos_default_extensions : { image = image }
            ]
          }
        }
      }),
    ] : [],
  )
}

data "talos_machine_configuration" "worker" {
  for_each = local.worker_instances

  cluster_name       = var.cluster_name
  cluster_endpoint   = local.cluster_endpoint
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  config_patches = concat(
    [
      yamlencode({
        cluster = {
          network = {
            cni            = { name = "none" }
            podSubnets     = [var.pod_cidr]
            serviceSubnets = [var.service_cidr]
          }
          proxy = { disabled = true }
        }
      }),
      yamlencode({
        machine = {
          install = {
            diskSelector = { size = ">= 10GB" }
          }
          network = {
            kubespan = {
              enabled                     = true
              advertiseKubernetesNetworks = true
            }
          }
        }
      }),
    ],
    each.value.gpu ? [
      yamlencode({
        machine = {
          kernel = {
            modules = [
              { name = "nvidia" },
              { name = "nvidia_uvm" },
              { name = "nvidia_drm" },
              { name = "nvidia_modeset" },
            ]
          }
          nodeLabels = { "ai" = "true" }
        }
      }),
    ] : [],
    length(concat(var.talos_default_extensions, each.value.extensions)) > 0 ? [
      yamlencode({
        machine = {
          install = {
            extensions = [
              for image in concat(var.talos_default_extensions, each.value.extensions) : { image = image }
            ]
          }
        }
      }),
    ] : [],
    length(each.value.taints) > 0 ? [
      yamlencode({
        machine = {
          nodeTaints = {
            for taint in each.value.taints : taint.key => "${taint.value}:${taint.effect}"
          }
        }
      }),
    ] : [],
    length(local.node_labels[each.key]) > 0 ? [
      yamlencode({
        machine = {
          nodeLabels = local.node_labels[each.key]
        }
      }),
    ] : [],
  )
}

resource "talos_machine_configuration_apply" "controlplane" {
  for_each = local.controlplane_instances

  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.controlplane.machine_configuration
  node                        = local.apply_address[each.key]

  config_patches = local.talos_nodes[each.key].ip != null ? [
    yamlencode({
      machine = {
        network = {
          interfaces = [
            {
              deviceSelector = { driver = "virtio_net" }
              addresses      = ["${local.talos_nodes[each.key].ip}/${var.network.subnet_prefix}"]
              routes         = [{ network = "0.0.0.0/0", gateway = var.network.gateway }]
            }
          ]
          nameservers = var.network.nameservers
        }
      }
    }),
  ] : []
}

resource "talos_machine_configuration_apply" "worker" {
  for_each = local.worker_instances

  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.worker[each.key].machine_configuration
  node                        = local.apply_address[each.key]

  config_patches = local.talos_nodes[each.key].ip != null ? [
    yamlencode({
      machine = {
        network = {
          interfaces = [
            {
              deviceSelector = { driver = "virtio_net" }
              addresses      = ["${local.talos_nodes[each.key].ip}/${var.network.subnet_prefix}"]
              routes         = [{ network = "0.0.0.0/0", gateway = var.network.gateway }]
            }
          ]
          nameservers = var.network.nameservers
        }
      }
    }),
  ] : []
}

data "talos_client_configuration" "this" {
  cluster_name         = var.cluster_name
  client_configuration = talos_machine_secrets.this.client_configuration
  endpoints            = local.controlplane_bootstrap_addresses
}

resource "talos_machine_bootstrap" "this" {
  depends_on           = [talos_machine_configuration_apply.controlplane]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = local.controlplane_bootstrap_addresses[0]
}

resource "talos_cluster_kubeconfig" "this" {
  depends_on           = [talos_machine_bootstrap.this]
  client_configuration = talos_machine_secrets.this.client_configuration
  node                 = local.controlplane_bootstrap_addresses[0]
}