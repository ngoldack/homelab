# ============================================================================
# Hetzner CX22 edge worker — public ingress for the homelab cluster.
# Joins the EXISTING Talos cluster as a worker. All public 80/443 traffic enters
# here; Traefik (scheduled via the node-role=edge taint) routes to homelab
# Services. Replaces NetBird ingress.
#
# NETWORKING: uses Cilium's native WireGuard node-to-node encryption (enabled in
# the Cilium HelmRelease) — NOT KubeSpan — because KubeSpan is incompatible with
# this cluster's eBPF-masquerade Cilium datapath, whereas Cilium WireGuard works
# with it.
#
# !!! REQUIREMENT — READ BEFORE `tofu apply` !!!
# Cilium WireGuard ENCRYPTS node-to-node traffic but does NOT NAT-traverse like
# KubeSpan. The edge node (Hetzner, public IP) and the homelab nodes must have L3
# reachability: ensure var.cluster_endpoint (control plane) is reachable from
# Hetzner and homelab node IPs are routable from the edge (public endpoint,
# port-forward, or an existing site link). Without that underlay the worker cannot
# join / form pod connectivity.
#
# VERIFY-BEFORE-DEPLOY: the talos_image_factory_* attribute names
# (siderolabs/talos ~0.11) and the hcloud-talos/imager provider (ALPHA) outputs.
# ============================================================================

# --- Talos image: built via the official Image Factory, imported as an hcloud
# snapshot by the (alpha) hcloud-talos/imager provider. Edge node is minimal
# (no TrueNAS/gVisor/NetBird extensions). ---
resource "talos_image_factory_schematic" "edge" {
  schematic = yamlencode({
    customization = {
      systemExtensions = {
        officialExtensions = []
      }
    }
  })
}

data "talos_image_factory_urls" "edge" {
  talos_version = var.talos_version
  schematic_id  = talos_image_factory_schematic.edge.id
  platform      = "hcloud"
  architecture  = "amd64"
}

resource "imager_image" "edge" {
  image_url    = data.talos_image_factory_urls.edge.urls.disk_image
  architecture = "x86" # imager: x86 == amd64
  location     = var.hetzner_edge_location
  description  = "talos-${var.talos_version}-edge"
}

# --- Firewall: 80/443 public, 51820/udp KubeSpan, 50000/tcp Talos API (admins).
# Port 6443 (k8s API) is intentionally NOT opened — the edge dials the homelab CP.
resource "hcloud_firewall" "edge" {
  name = "${var.cluster_name}-edge"

  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "80"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "443"
    source_ips = ["0.0.0.0/0", "::/0"]
  }
  rule {
    direction  = "in"
    protocol   = "udp"
    port       = "51820" # KubeSpan WireGuard
    source_ips = ["0.0.0.0/0", "::/0"]
  }
  rule {
    direction  = "in"
    protocol   = "tcp"
    port       = "50000" # Talos apid — admin IPs only
    source_ips = var.edge_admin_ips
  }
}

resource "hcloud_server" "edge" {
  name         = "${var.cluster_name}-edge-0"
  server_type  = var.hetzner_edge_server_type
  location     = var.hetzner_edge_location
  image        = imager_image.edge.image_id
  firewall_ids = [hcloud_firewall.edge.id]
  labels       = { cluster = var.cluster_name, role = "edge" }

  public_net {
    ipv4_enabled = true
    ipv6_enabled = true
  }

  # Talos reinstalls from the snapshot on first boot; don't rebuild on image drift.
  lifecycle {
    ignore_changes = [image]
  }
}

# --- Edge worker machine config: joins the existing cluster via KubeSpan ---
data "talos_machine_configuration" "edge" {
  cluster_name       = var.cluster_name
  cluster_endpoint   = var.cluster_endpoint
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  config_patches = [
    yamlencode({ cluster = { network = { cni = { name = "none" } } } }),
    yamlencode({ cluster = { proxy = { disabled = true } } }),
    # Edge taint + topology labels — only edge-specific workloads land here.
    yamlencode({
      machine = {
        nodeLabels = {
          "node-role"                   = "edge"
          "location"                    = "hetzner-${var.hetzner_edge_location}"
          "topology.kubernetes.io/zone" = "hetzner-${var.hetzner_edge_location}"
        }
        nodeTaints = {
          "node-role" = "edge:NoSchedule"
        }
      }
    }),
    # Let Traefik bind 80/443 without root.
    yamlencode({
      machine = { sysctls = { "net.ipv4.ip_unprivileged_port_start" = "0" } }
    }),
    # hcloud servers expose a single root disk as /dev/sda.
    yamlencode({
      machine = { install = { disk = "/dev/sda" } }
    }),
  ]
}

resource "talos_machine_configuration_apply" "edge" {
  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.edge.machine_configuration
  node                        = hcloud_server.edge.ipv4_address
  endpoint                    = hcloud_server.edge.ipv4_address
}
