locals {
  network_ipv4_cidr = "172.30.0.0/16"
  node_ipv4_cidr    = "172.30.1.0/24"
  pod_ipv4_cidr     = "172.30.16.0/20"
  service_ipv4_cidr = "172.30.8.0/21"

  control_plane_name       = "${var.cluster_name}-control-plane-1"
  worker_name              = "${var.cluster_name}-worker-1"
  control_plane_private_ip = cidrhost(local.node_ipv4_cidr, 101)
  worker_private_ip        = cidrhost(local.node_ipv4_cidr, 201)
  cluster_endpoint         = "https://${hcloud_primary_ip.control_plane.ip_address}:6443"
}

resource "talos_image_factory_schematic" "arm64" {
  schematic = yamlencode({
    customization = {}
  })
}

data "talos_image_factory_urls" "hcloud_arm64" {
  talos_version = var.talos_version
  schematic_id  = talos_image_factory_schematic.arm64.id
  platform      = "hcloud"
  architecture  = "arm64"
}

resource "imager_image" "talos_arm" {
  image_url    = data.talos_image_factory_urls.hcloud_arm64.urls.disk_image
  architecture = "arm"
  description  = "Talos Linux ${var.talos_version} ARM64 for ${var.cluster_name}"

  labels = {
    "cluster" = var.cluster_name
    "os"      = "talos"
    "version" = var.talos_version
  }
}

resource "hcloud_network" "cluster" {
  name     = var.cluster_name
  ip_range = local.network_ipv4_cidr

  labels = {
    "cluster" = var.cluster_name
  }
}

resource "hcloud_network_subnet" "nodes" {
  network_id   = hcloud_network.cluster.id
  type         = "cloud"
  network_zone = "eu-central"
  ip_range     = local.node_ipv4_cidr
}

resource "hcloud_placement_group" "control_plane" {
  name = "${var.cluster_name}-control-plane"
  type = "spread"

  labels = {
    "cluster" = var.cluster_name
  }
}

resource "hcloud_placement_group" "worker" {
  name = "${var.cluster_name}-worker"
  type = "spread"

  labels = {
    "cluster" = var.cluster_name
  }
}

resource "hcloud_firewall" "cluster" {
  name = var.cluster_name

  rule {
    description = "Allow Kubernetes API"
    direction   = "in"
    protocol    = "tcp"
    port        = "6443"
    source_ips  = ["0.0.0.0/0"]
  }

  rule {
    description = "Allow Talos API"
    direction   = "in"
    protocol    = "tcp"
    port        = "50000"
    source_ips  = ["0.0.0.0/0"]
  }

  rule {
    description = "Allow public HTTP"
    direction   = "in"
    protocol    = "tcp"
    port        = "80"
    source_ips  = ["0.0.0.0/0"]
  }

  rule {
    description = "Allow public HTTPS"
    direction   = "in"
    protocol    = "tcp"
    port        = "443"
    source_ips  = ["0.0.0.0/0"]
  }

  rule {
    description = "Allow cluster-internal TCP"
    direction   = "in"
    protocol    = "tcp"
    port        = "any"
    source_ips  = [local.network_ipv4_cidr]
  }

  rule {
    description = "Allow cluster-internal UDP"
    direction   = "in"
    protocol    = "udp"
    port        = "any"
    source_ips  = [local.network_ipv4_cidr]
  }

  rule {
    description = "Allow cluster-internal ICMP"
    direction   = "in"
    protocol    = "icmp"
    source_ips  = [local.network_ipv4_cidr]
  }

  labels = {
    "cluster" = var.cluster_name
  }
}

resource "hcloud_primary_ip" "control_plane" {
  name              = "${local.control_plane_name}-ipv4"
  location          = var.location
  type              = "ipv4"
  auto_delete       = false
  delete_protection = false

  labels = {
    "cluster" = var.cluster_name
    "role"    = "control-plane"
  }
}

resource "hcloud_primary_ip" "worker" {
  name              = "${local.worker_name}-ipv4"
  location          = var.location
  type              = "ipv4"
  auto_delete       = false
  delete_protection = false

  labels = {
    "cluster" = var.cluster_name
    "role"    = "worker"
  }
}

resource "talos_machine_secrets" "cluster" {
  talos_version = var.talos_version
}

locals {
  common_machine_patch = {
    machine = {
      install = {
        image = "ghcr.io/siderolabs/installer:${var.talos_version}"
      }
      kubelet = {
        extraArgs = {
          "cloud-provider"             = "external"
          "rotate-server-certificates" = true
        }
        nodeIP = {
          validSubnets = [local.node_ipv4_cidr]
        }
      }
      network = {
        interfaces = [
          {
            interface = "eth0"
            dhcp      = true
          },
          {
            interface = "eth1"
            dhcp      = true
          },
        ]
      }
      time = {
        servers = ["ntp1.hetzner.de", "ntp2.hetzner.com", "ntp3.hetzner.net"]
      }
    }
  }

  cluster_patch = {
    cluster = {
      allowSchedulingOnControlPlanes = true
      network = {
        dnsDomain      = var.cluster_domain
        podSubnets     = [local.pod_ipv4_cidr]
        serviceSubnets = [local.service_ipv4_cidr]
        cni            = { name = "none" }
      }
      proxy = { disabled = true }
      apiServer = {
        certSANs = [hcloud_primary_ip.control_plane.ip_address]
      }
      controllerManager = {
        extraArgs = {
          "bind-address"             = "0.0.0.0"
          "cloud-provider"           = "external"
          "node-cidr-mask-size-ipv4" = split("/", local.node_ipv4_cidr)[1]
        }
      }
      externalCloudProvider = {
        enabled = true
        manifests = [
          "https://raw.githubusercontent.com/siderolabs/talos-cloud-controller-manager/v1.6.0/docs/deploy/cloud-controller-manager-daemonset.yml",
        ]
      }
      inlineManifests = [
        {
          name = "hcloud-credentials"
          contents = yamlencode({
            apiVersion = "v1"
            kind       = "Secret"
            type       = "Opaque"
            metadata = {
              name      = "hcloud"
              namespace = "kube-system"
            }
            stringData = {
              network = hcloud_network.cluster.id
              token   = local.secrets["hcloud_api_token"]
            }
          })
        },
      ]
    }
  }
}

data "talos_machine_configuration" "control_plane" {
  cluster_name       = var.cluster_name
  cluster_endpoint   = local.cluster_endpoint
  machine_type       = "controlplane"
  machine_secrets    = talos_machine_secrets.cluster.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version
  docs               = false
  examples           = false
  config_patches = [
    yamlencode(local.common_machine_patch),
    yamlencode(merge(local.cluster_patch, {
      machine = {
        nodeLabels = {
          "topology.kubernetes.io/zone" = "cloud"
          "workload/public-ingress"     = "true"
        }
      }
    })),
  ]
}

data "talos_machine_configuration" "worker" {
  cluster_name       = var.cluster_name
  cluster_endpoint   = local.cluster_endpoint
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.cluster.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version
  docs               = false
  examples           = false
  config_patches = [
    yamlencode(local.common_machine_patch),
    yamlencode(merge(local.cluster_patch, {
      machine = {
        nodeLabels = {
          "topology.kubernetes.io/zone" = "cloud"
        }
      }
    })),
  ]
}

resource "hcloud_server" "control_plane" {
  name               = local.control_plane_name
  location           = var.location
  server_type        = var.server_type
  image              = imager_image.talos_arm.id
  user_data          = data.talos_machine_configuration.control_plane.machine_configuration
  placement_group_id = hcloud_placement_group.control_plane.id
  firewall_ids       = [hcloud_firewall.cluster.id]

  public_net {
    ipv4_enabled = true
    ipv4         = hcloud_primary_ip.control_plane.id
    ipv6_enabled = false
  }

  network {
    network_id = hcloud_network.cluster.id
    ip         = local.control_plane_private_ip
    alias_ips  = []
  }

  labels = {
    "cluster"     = var.cluster_name
    "role"        = "control-plane"
    "server_type" = var.server_type
  }

  depends_on = [hcloud_network_subnet.nodes]

  lifecycle {
    ignore_changes = [image, user_data]
  }
}

resource "hcloud_server" "worker" {
  name               = local.worker_name
  location           = var.location
  server_type        = var.server_type
  image              = imager_image.talos_arm.id
  user_data          = data.talos_machine_configuration.worker.machine_configuration
  placement_group_id = hcloud_placement_group.worker.id
  firewall_ids       = [hcloud_firewall.cluster.id]

  public_net {
    ipv4_enabled = true
    ipv4         = hcloud_primary_ip.worker.id
    ipv6_enabled = false
  }

  network {
    network_id = hcloud_network.cluster.id
    ip         = local.worker_private_ip
    alias_ips  = []
  }

  labels = {
    "cluster"     = var.cluster_name
    "role"        = "worker"
    "server_type" = var.server_type
  }

  depends_on = [hcloud_network_subnet.nodes]

  lifecycle {
    ignore_changes = [image, user_data]
  }
}

resource "talos_machine_configuration_apply" "control_plane" {
  client_configuration        = talos_machine_secrets.cluster.client_configuration
  machine_configuration_input = data.talos_machine_configuration.control_plane.machine_configuration
  node                        = hcloud_primary_ip.control_plane.ip_address

  depends_on = [hcloud_server.control_plane]
}

resource "talos_machine_configuration_apply" "worker" {
  client_configuration        = talos_machine_secrets.cluster.client_configuration
  machine_configuration_input = data.talos_machine_configuration.worker.machine_configuration
  node                        = hcloud_primary_ip.worker.ip_address

  depends_on = [hcloud_server.worker]
}

resource "talos_machine_bootstrap" "cluster" {
  client_configuration = talos_machine_secrets.cluster.client_configuration
  endpoint             = hcloud_primary_ip.control_plane.ip_address
  node                 = hcloud_primary_ip.control_plane.ip_address

  depends_on = [talos_machine_configuration_apply.control_plane]
}

resource "talos_cluster_kubeconfig" "cluster" {
  client_configuration = talos_machine_secrets.cluster.client_configuration
  node                 = hcloud_primary_ip.control_plane.ip_address

  depends_on = [talos_machine_bootstrap.cluster]
}

resource "terraform_data" "kubernetes_api_ready" {
  triggers_replace = [
    talos_cluster_kubeconfig.cluster.id,
    hcloud_primary_ip.control_plane.ip_address,
  ]

  provisioner "local-exec" {
    command = <<-EOT
      for attempt in $(seq 1 60); do
        if curl --silent --show-error --connect-timeout 3 --max-time 5 --insecure ${local.cluster_endpoint}/version >/dev/null; then
          exit 0
        fi
        sleep 2
      done
      echo "Kubernetes API did not become ready within 120 seconds" >&2
      exit 1
    EOT
  }

  depends_on = [talos_cluster_kubeconfig.cluster]
}

resource "helm_release" "cilium" {
  name             = "cilium"
  repository       = "https://helm.cilium.io/"
  chart            = "cilium"
  version          = "1.19.5"
  namespace        = "kube-system"
  create_namespace = false
  take_ownership   = true
  wait             = false

  values = [yamlencode({
    cluster = {
      name = "cloud"
      id   = 2
    }
    clustermesh = {
      useAPIServer = true
    }
    ipam = {
      mode = "kubernetes"
    }
    kubeProxyReplacement = true
    k8sServiceHost       = "127.0.0.1"
    k8sServicePort       = 7445
    bpf = {
      masquerade = true
    }
    routingMode    = "tunnel"
    tunnelProtocol = "vxlan"
    securityContext = {
      capabilities = {
        ciliumAgent      = ["CHOWN", "KILL", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "SYS_ADMIN", "SYS_RESOURCE", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"]
        cleanCiliumState = ["NET_ADMIN", "SYS_ADMIN", "SYS_RESOURCE"]
      }
    }
    cgroup = {
      autoMount = {
        enabled = false
      }
      hostRoot = "/sys/fs/cgroup"
    }
    gatewayAPI = {
      enabled = true
    }
  })]

  depends_on = [terraform_data.kubernetes_api_ready]
}

resource "random_id" "home_talos_backup_bucket" {
  byte_length = 4
}

resource "random_id" "cloud_talos_backup_bucket" {
  byte_length = 4
}

resource "aws_s3_bucket" "home_talos_backups" {
  bucket        = "home-talos-etcd-${random_id.home_talos_backup_bucket.hex}"
  force_destroy = false
}

resource "aws_s3_bucket" "cloud_talos_backups" {
  bucket        = "${var.cluster_name}-etcd-${random_id.cloud_talos_backup_bucket.hex}"
  force_destroy = false
}

output "home_talos_backup_bucket" {
  description = "Hetzner Object Storage bucket for Home Talos etcd snapshots"
  value       = aws_s3_bucket.home_talos_backups.bucket
}

output "cloud_talos_backup_bucket" {
  description = "Hetzner Object Storage bucket for Cloud Talos etcd snapshots"
  value       = aws_s3_bucket.cloud_talos_backups.bucket
}

output "talos_backup_s3_endpoint" {
  description = "S3 endpoint used by the Talos etcd backup workflow"
  value       = "https://${var.object_storage_location}.your-objectstorage.com"
}

output "cloud_ingress_ipv4" {
  description = "Public IPv4 address of the cloud control plane"
  value       = hcloud_primary_ip.control_plane.ip_address
}

output "cloud_ingress_ipv6" {
  description = "The cloud cluster is IPv4-only; retained for output compatibility"
  value       = null
}

output "cloud_cluster_kubeconfig" {
  description = "Kubeconfig for the cloud cluster"
  value       = talos_cluster_kubeconfig.cluster.kubeconfig_raw
  sensitive   = true
}

output "cloud_cluster_talosconfig" {
  description = "Talosconfig for the cloud cluster"
  value = yamlencode({
    context = var.cluster_name
    contexts = {
      (var.cluster_name) = {
        endpoints = [hcloud_primary_ip.control_plane.ip_address]
        ca        = talos_machine_secrets.cluster.client_configuration.ca_certificate
        crt       = talos_machine_secrets.cluster.client_configuration.client_certificate
        key       = talos_machine_secrets.cluster.client_configuration.client_key
      }
    }
  })
  sensitive = true
}
