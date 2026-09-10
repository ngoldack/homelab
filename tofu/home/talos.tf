locals {
  controlplane_instances = { for name, node in var.nodes : name => node if node.talos_role == "controlplane" }
  worker_instances       = { for name, node in var.nodes : name => node if node.talos_role == "worker" }

  # Tailscale, same ExtensionServiceConfig shape tofu/cloud uses for its
  # nodes — lets the etcd-backup GitHub Actions job reach a node's Talos API
  # over the tailnet from a GitHub-hosted runner. Only applied to nodes that
  # actually carry the siderolabs/tailscale extension (see each node's
  # `extensions` in terraform.tfvars), gated below rather than unconditional.
  tailscale_config_patch = yamlencode({
    apiVersion = "v1alpha1"
    kind       = "ExtensionServiceConfig"
    name       = "tailscale"
    environment = [
      "TS_AUTHKEY=${local.secrets["tailscale_auth_key"]}",
    ]
  })

  # Prefer the live QEMU-agent-reported address (needed pre-bootstrap: the
  # node is still on DHCP, its eventual static IP isn't live yet), but fall
  # back to the known static IP if the agent query comes back empty. A
  # single flaky/slow qemu-guest-agent response (common under load — the
  # agent socket is single-threaded) would otherwise null out `current_ip`
  # and hard-fail `node`, a required, non-nullable provider attribute, even
  # for an already-bootstrapped node whose static IP is perfectly reachable.
  apply_address = { for name, node in local.talos_nodes : name => coalesce(node.current_ip, node.ip) }
  bootstrap_address = {
    for name, node in local.talos_nodes : name => coalesce(node.ip, node.current_ip)
  }
  controlplane_bootstrap_addresses = [
    for name in keys(local.controlplane_instances) : local.bootstrap_address[name]
  ]
}

resource "talos_machine_secrets" "this" {
  talos_version = var.talos_version

  # prevent_destroy temporarily removed for the full teardown/rebuild —
  # restore it once the fresh cluster is up.
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
            # Install from the Image Factory installer for THIS node's
            # extension set. Without it Talos installs the stock
            # ghcr.io/siderolabs/installer, and every extension baked into the
            # boot ISO is silently discarded the moment the node installs to
            # disk and reboots — the ISO's extensions only ever live in the
            # live/maintenance boot. That failure is invisible: the node comes
            # up healthy and joins the cluster, just with no qemu-guest-agent
            # (so `agent { wait_for_ip }` in main.tf times out on every later
            # plan/apply), no nvidia driver, and no nfs-utils for truenas-csi.
            # This URL is also version-pinned to var.talos_version, so it is
            # what makes that variable actually govern the installed OS.
            image = data.talos_image_factory_urls.this[
              local.extension_set_keys[keys(local.controlplane_instances)[0]]
            ].urls.installer
          }
        }
      }),
      yamlencode({
        machine = {
          network = {
            # advertiseKubernetesNetworks stays unset (false): per Talos's
            # own docs, enabling it makes KubeSpan "take over pod-to-pod
            # traffic and send it over KubeSpan directly" — a direct
            # conflict with Cilium's own routingMode=tunnel/vxlan overlay,
            # which already owns pod-to-pod encapsulation. KubeSpan stays
            # enabled purely for its encrypted node-to-node WireGuard mesh
            # (API/discovery/general traffic), not as a second CNI.
            kubespan = {
              enabled = true
            }
          }
        }
      }),
      # Same derived-label treatment as workers get (see
      # data.talos_machine_configuration.worker below) — this data source
      # assumes a single control plane (indexed [0] the same way
      # talos_machine_bootstrap.this does), so pull that one node's labels
      # directly rather than for_each-ing.
      yamlencode({
        machine = {
          nodeLabels = local.node_labels[keys(local.controlplane_instances)[0]]
        }
      }),
    ],
    contains(var.nodes[keys(local.controlplane_instances)[0]].extensions, "siderolabs/tailscale") ? [
      local.tailscale_config_patch
    ] : [],
    # No machine.install.extensions patch: that field has had no effect
    # since Talos 1.10 (kept only so pre-1.10 configs still validate).
    # var.talos_default_extensions is instead baked into the boot image
    # itself via the talos_image_factory_schematic resources in main.tf —
    # see local.unique_extension_sets and proxmox_download_file.talos_iso.
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
            # Per-node Image Factory installer — see the control-plane install
            # block above for why this is load-bearing. Keyed by this worker's
            # own resolved extension set, so the GPU worker gets the nvidia
            # image and the others get the lean one.
            image = data.talos_image_factory_urls.this[
              local.extension_set_keys[each.key]
            ].urls.installer
          }
          network = {
            # advertiseKubernetesNetworks stays unset (false): per Talos's
            # own docs, enabling it makes KubeSpan "take over pod-to-pod
            # traffic and send it over KubeSpan directly" — a direct
            # conflict with Cilium's own routingMode=tunnel/vxlan overlay,
            # which already owns pod-to-pod encapsulation. KubeSpan stays
            # enabled purely for its encrypted node-to-node WireGuard mesh
            # (API/discovery/general traffic), not as a second CNI.
            kubespan = {
              enabled = true
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
    # No machine.install.extensions patch here either — same reasoning as
    # the control plane above; this node's resolved extension set (defaults
    # + each.value.extensions) is baked into its boot image instead.
    #
    # No machine.nodeTaints patch: Kubernetes' NodeRestriction admission
    # plugin forbids a node from setting its own taints — confirmed on every
    # node this repo has ever tainted, brand-new ones included — so Talos's
    # own NodeApplyController can never land this, and it also blocks that
    # same controller's label patch (labels+taints ride one atomic PATCH).
    # Taints are applied instead by a Flux-managed Job
    # (kubernetes/infrastructure/home/node-taints/), selecting nodes by the
    # labels below rather than by name.
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