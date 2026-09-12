# Public ingress worker(s) on Hetzner Cloud.
#
# These are ordinary workers of THIS cluster — there is no second cluster and
# no ClusterMesh. They join the LAN control plane over Talos KubeSpan
# (WireGuard), which means:
#
#   * cluster.controlPlane.endpoint stays https://10.30.0.10:6443. 10.30.0.10/32
#     is already inside every peer's KubeSpan allowedIPs, so no certSAN change
#     and no change to any existing node is required to add one of these.
#   * Nothing has to be forwarded on the home router. All four LAN nodes sit
#     behind one NAT and advertise the same public ip:port, so they cannot
#     accept inbound — they dial OUT to this node instead. That is exactly why
#     UDP 51820 must be open INBOUND *here*, on the public side. Without it
#     three of the four peerings never form, and with forceRouting=true (live)
#     KubeSpan fails closed and blackholes them rather than falling back.
#   * Peering is brokered by discovery.talos.dev before this node has ever
#     reached the API server, so there is no chicken-and-egg on join.
#
# Deliberately hand-rolled rather than delegated to the hcloud-talos module.
# That module pins user_data behind `ignore_changes` and ships no
# talos_machine_configuration_apply at all, making machine config
# install-time-only, and it renders Cilium client-side where no cluster lookup
# can work. Only the snapshot build is delegated (to the imager provider):
# Hetzner cannot boot an arbitrary image, and the alternative is bespoke
# rescue-mode `dd` over an SSH provisioner.

locals {
  # Ingress nodes get a deliberately minimal extension set. NOT the cluster
  # defaults (var.talos_default_extensions): qemu-guest-agent is Proxmox-only,
  # and nfs-utils/iscsi-tools/nvme-cli exist solely for truenas-csi, which must
  # never run here — its node DaemonSet tolerates every taint and hostPath
  # mounts "/", so the only thing keeping it off this node is a positive
  # nodeSelector on the home site label (see the truenas-csi HelmRelease).
  #
  # tailscale is included purely as an independent admin path. It is NOT given
  # --advertise-routes/--accept-routes: cp-main advertises this cluster's own
  # pod/service CIDRs into the tailnet, and accepting those here would put
  # Tailscale's ip rule (pref 5270 -> table 52) ahead of KubeSpan's (pref
  # 32500), hijacking cluster traffic onto a 1280-MTU tun with asymmetric
  # return paths.
  cloud_base_extensions = ["siderolabs/tailscale"]

  cloud_extension_sets = {
    for name, node in var.cloud_nodes :
    name => sort(distinct(concat(local.cloud_base_extensions, node.extensions)))
  }
}

resource "talos_image_factory_schematic" "cloud" {
  for_each = var.cloud_nodes

  schematic = yamlencode({
    customization = {
      systemExtensions = {
        officialExtensions = local.cloud_extension_sets[each.key]
      }
    }
  })
}

data "talos_image_factory_urls" "cloud" {
  for_each = var.cloud_nodes

  talos_version = var.talos_version
  schematic_id  = talos_image_factory_schematic.cloud[each.key].id
  platform      = "hcloud"
  architecture  = each.value.arch
}

# Builds a Hetzner snapshot from the Image Factory disk image. Keyed by the
# schematic, so changing this node's extension set produces a NEW snapshot
# rather than silently reusing a stale one — Talos extensions are install-time,
# and a node's running extension set can only change by reinstalling from a
# new image.
resource "imager_image" "cloud" {
  for_each = var.cloud_nodes

  image_url = data.talos_image_factory_urls.cloud[each.key].urls.disk_image
  # hcloud's own vocabulary is x86/arm, not Talos's amd64/arm64.
  architecture = each.value.arch == "amd64" ? "x86" : "arm"
  location     = each.value.location

  timeouts {
    create = "20m"
  }
}

# Static public IP, kept across server replacements so DNS and the firewall
# don't have to chase it. auto_delete=false keeps it when the server goes.
#
# NO prevent_destroy here, deliberately: the two prevent_destroy buckets in the
# old tofu/cloud root turned `tofu destroy` into a plan-time abort that
# destroyed nothing at all, which is how that root ended up needing manual
# state surgery to tear down.
resource "hcloud_primary_ip" "cloud" {
  for_each = var.cloud_nodes

  name        = "${var.cluster_name}-${each.key}-ipv4"
  type        = "ipv4"
  location    = each.value.location
  auto_delete = false

  labels = {
    cluster = var.cluster_name
    role    = "ingress"
  }
}

resource "hcloud_firewall" "cloud" {
  for_each = var.cloud_nodes

  name = "${var.cluster_name}-${each.key}"

  labels = {
    cluster = var.cluster_name
    role    = "ingress"
  }

  # KubeSpan. The single most load-bearing rule here: the LAN nodes are all
  # behind one NAT and cannot accept inbound, so they dial out to this node and
  # this node must accept. Cannot be narrowed to the home WAN IP — that address
  # is dynamic, and narrowing it wrongly silently drops three of four peerings.
  rule {
    description = "KubeSpan WireGuard (cluster mesh)"
    direction   = "in"
    protocol    = "udp"
    port        = "51820"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  # Public HTTPS. This node is the cluster's sole public entry point; Envoy
  # binds 443 here via Cilium's Gateway API host-network mode.
  rule {
    description = "Public HTTPS ingress"
    direction   = "in"
    protocol    = "tcp"
    port        = "443"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  rule {
    description = "ICMP (diagnostics)"
    direction   = "in"
    protocol    = "icmp"
    source_ips  = ["0.0.0.0/0", "::/0"]
  }

  # Talos apid (50000) is deliberately NOT opened. The initial machine config
  # arrives via user_data at first boot, and every later config push is proxied
  # through the control plane's own apid over KubeSpan (see the `endpoint`
  # argument on talos_machine_configuration_apply below) rather than dialed
  # directly. Leaving 50000 public would mean that, in the window before this
  # node's config is applied, anyone who reached it could hand it a machine
  # config — and thereby own a full KubeSpan mesh member with a tunnel into the
  # home LAN.
  #
  # Kubelet (10250) and VXLAN (8472) are likewise not opened: that traffic
  # rides inside the KubeSpan tunnel and arrives as UDP 51820.
}

resource "hcloud_server" "cloud" {
  for_each = var.cloud_nodes

  name        = "${var.cluster_name}-${each.key}"
  server_type = each.value.server_type
  location    = each.value.location
  image       = tostring(imager_image.cloud[each.key].id)

  firewall_ids = [hcloud_firewall.cloud[each.key].id]

  public_net {
    ipv4_enabled = true
    ipv4         = hcloud_primary_ip.cloud[each.key].id
    ipv6_enabled = false
  }

  # Talos reads its machine config from user_data at first boot, so the node
  # comes up already configured and joins on its own. There is no bootstrap
  # step that needs to reach it from outside.
  user_data = data.talos_machine_configuration.cloud_worker[each.key].machine_configuration

  labels = {
    cluster = var.cluster_name
    role    = "ingress"
    os      = "talos"
  }

  lifecycle {
    # Without this, every machine-config edit REPLACES the server, because
    # user_data is part of the create-only payload. Config changes are applied
    # live by talos_machine_configuration_apply.cloud_worker instead. The image
    # is ignored for the same reason: a new schematic means a deliberate
    # reinstall (`tofu apply -replace`), not an implicit one mid-plan.
    ignore_changes = [user_data, image]
  }
}

data "talos_machine_configuration" "cloud_worker" {
  for_each = var.cloud_nodes

  cluster_name = var.cluster_name
  # The LAN address, unchanged. Reachable from this node over KubeSpan, and
  # already covered by the existing API server certificate.
  cluster_endpoint   = local.cluster_endpoint
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  config_patches = [
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
          # Hetzner Cloud servers present their root disk as /dev/sda.
          disk = "/dev/sda"
          # Same load-bearing reason as the Proxmox nodes: without an Image
          # Factory installer reference, Talos installs the stock installer on
          # first boot and silently discards every extension baked into the
          # image — here that would drop tailscale, the only independent admin
          # path onto this node.
          image = data.talos_image_factory_urls.cloud[each.key].urls.installer
        }
        network = {
          # KubeSpan is how this node reaches the rest of the cluster at all.
          # NOTE: no `interfaces` block and no hostname. Hetzner NICs are
          # virtio_net, the same selector the Proxmox workers use to pin a
          # static 10.30.0.x address — applying that here would strand the node
          # on a LAN address it cannot route. And a static
          # machine.network.hostname is rejected outright by this Talos version
          # ("static hostname is already set in v1alpha1 config"), which would
          # leave the node sitting in maintenance mode.
          kubespan = {
            enabled = true
          }
        }
        kubelet = {
          extraArgs = {
            # Taint this node at REGISTRATION, via the kubelet's own flag —
            # not machine.nodeTaints (not a valid key here, and self-applied
            # taints are blocked by the NodeRestriction admission plugin
            # anyway, which is the whole reason the LAN nodes need a
            # Flux-managed Job to taint them). NodeRestriction explicitly
            # permits a node to set taints at registration; what it forbids is
            # modifying them afterwards.
            #
            # Registration-time matters here: the Flux Job approach leaves a
            # window between a node joining and the Job reconciling, during
            # which anything could schedule onto it. That is tolerable on the
            # LAN and is not tolerable on a public node.
            "register-with-taints" = "dedicated=ingress:NoSchedule"
          }
        }
        nodeLabels = merge(
          {
            # What Cilium's gatewayAPI.hostNetwork node selector matches, so
            # Envoy binds 80/443 on this node and nowhere else. That selector
            # fails OPEN if it does not match — a typo here means Envoy binds
            # 443 on every LAN node instead, with nothing logged.
            "node.homelab/role" = "ingress"
            # Positive site label. truenas-csi selects on this to keep its node
            # DaemonSet on LAN nodes only; a taint alone will not stop it.
            "topology.homelab/site" = "cloud"
          },
          each.value.node_labels,
        )
      }
    }),
    # Tailscale purely as an out-of-band admin path, with no route flags — see
    # the comment on local.cloud_base_extensions for why accepting routes here
    # would hijack cluster traffic.
    yamlencode({
      apiVersion = "v1alpha1"
      kind       = "ExtensionServiceConfig"
      name       = "tailscale"
      environment = [
        "TS_AUTHKEY=${local.secrets["tailscale_auth_key"]}",
      ]
    }),
  ]
}

resource "talos_machine_configuration_apply" "cloud_worker" {
  for_each = var.cloud_nodes

  client_configuration        = talos_machine_secrets.this.client_configuration
  machine_configuration_input = data.talos_machine_configuration.cloud_worker[each.key].machine_configuration

  # Reach this node's apid THROUGH the control plane rather than dialing its
  # public address: talosctl's --endpoints/--nodes split proxies the request
  # over KubeSpan. That is what lets port 50000 stay closed to the internet.
  endpoint = local.controlplane_bootstrap_addresses[0]
  node     = hcloud_server.cloud[each.key].ipv4_address

  # No config_patches: everything is already in the generated config above.
  # In particular there is deliberately no static-IP patch — Hetzner assigns
  # the address via DHCP and the node must keep its public routing.
}

output "cloud_ingress_ipv4" {
  description = "Public IPv4 of each ingress node. NOT a DNS target any more: ingress moved to the LAN VIP 10.30.0.200 when gatewayAPI.hostNetwork was turned off, and external-dns publishes that instead, via the external-dns.alpha.kubernetes.io/target annotation on the Gateway. Kept only as a record of the address the firewall rules are written against."
  value       = { for name, ip in hcloud_primary_ip.cloud : name => ip.ip_address }
}
