locals {
  controlplane_instances = { for name, node in var.nodes : name => node if node.talos_role == "controlplane" }
  worker_instances       = { for name, node in var.nodes : name => node if node.talos_role == "worker" }

  # Storage-client paths that truenas-csi's node DaemonSet hostPath-mounts.
  #
  # Talos runs kubelet as its own containerized process, isolated from the
  # host's mount namespace — installing siderolabs/iscsi-tools or
  # siderolabs/nvme-cli puts their directories on the HOST, but kubelet itself
  # still can't see them, so any pod's hostPath mount of those paths
  # (truenas-csi's node DaemonSet does exactly this) fails with "hostPath type
  # check failed: ... is not a directory" even though `talosctl ls` shows the
  # directory exists. extraMounts is Talos's documented fix: explicitly bind
  # these into kubelet's own view. Applied to every node (not just
  # truenas-csi's usual targets) because its DaemonSet's blanket tolerations
  # schedule it everywhere, control plane included.
  csi_kubelet_extra_mounts = yamlencode({
    machine = {
      kubelet = {
        extraMounts = [
          {
            destination = "/etc/iscsi"
            type        = "bind"
            source      = "/etc/iscsi"
            options     = ["bind", "rshared", "rw"]
          },
          {
            destination = "/var/lib/iscsi"
            type        = "bind"
            source      = "/var/lib/iscsi"
            options     = ["bind", "rshared", "rw"]
          },
          # /etc/nvme holds the host's stable NVMe host NQN and hostid, put
          # there by the siderolabs/nvme-cli extension. The truenas-csi node
          # DaemonSet's container image ships no /etc/nvme/hostnqn of its own
          # (only discovery.conf — confirmed by inspecting the running
          # container), and nvme-cli invents a RANDOM host NQN whenever that
          # file is missing. A host identity that changes on every connect is
          # not one a TrueNAS NVMe-oF subsystem can reliably present a
          # namespace to, which is why staging failed with "NVMe-oF device ...
          # did not appear after connect" while the connection itself was
          # being made.
          #
          # A hostPath mount alone does not fix that — the same reason the two
          # iscsi entries above exist. Talos runs kubelet in its own mount
          # namespace, so a directory can be plainly visible via `talosctl ls`
          # on the host and still be invisible to a pod's hostPath. The
          # corresponding volume/volumeMount is added to the DaemonSet by a
          # postRenderer patch in
          # kubernetes/infrastructure/home/truenas-csi/helmrelease.yaml,
          # because the chart offers no values key for it.
          {
            destination = "/etc/nvme"
            type        = "bind"
            source      = "/etc/nvme"
            options     = ["bind", "rshared", "rw"]
          },
        ]
      }
    }
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
      # OIDC (headlamp): the browser flow hands headlamp an id_token from
      # authentik, and headlamp presents that token to the kube-apiserver —
      # which must therefore trust the same issuer. Without these flags every
      # login ends in "The cluster did not accept your sign-in. Its API server
      # may not trust this OIDC provider". The issuer is the provider's
      # APPLICATION slug path (…/application/o/headlamp/), not the client id:
      # …/application/o/headlamp-oidc/ 404s, which is what the ID token's iss
      # claim carries. Prefixes keep OIDC identities distinct from any local
      # user of the same name; the matching ClusterRoleBinding for oidc:akadmin
      # lives in kubernetes/infrastructure/home/authentik/cluster-oidc-rbac.yaml.
      yamlencode({
        cluster = {
          apiServer = {
            extraArgs = {
              "oidc-issuer-url"      = "https://authentik.ngoldack.de/application/o/headlamp/"
              "oidc-client-id"       = "headlamp-oidc"
              "oidc-username-claim"  = "preferred_username"
              "oidc-groups-claim"    = "groups"
              "oidc-username-prefix" = "oidc:"
              "oidc-groups-prefix"   = "oidc:"
            }
          }
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
      local.csi_kubelet_extra_mounts,
      # Lets a pod obtain a scoped Talos API credential by creating a
      # ServiceAccount CR (serviceaccounts.talos.dev). Enabling this is what
      # makes Talos install and serve that CRD at all, and it runs a
      # controller on the control plane that reconciles each CR into a Secret
      # holding a generated talosconfig. Used by the talos-backup CronJob in
      # kubernetes/infrastructure/home/talos-backup/ to take etcd snapshots
      # from inside the cluster.
      #
      # This is a control-plane-only feature — etcd lives here, and Talos
      # documents it as such — so it is deliberately absent from the worker
      # config below. It applies live through
      # talos_machine_configuration_apply with no reboot.
      #
      # Both lists are deliberately as narrow as they go:
      #   allowedRoles — os:etcd:backup authorizes exactly one API method,
      #     /machine.MachineService/EtcdSnapshot. This is the ceiling; the
      #     reconciler rejects any CR requesting a role absent from it, so a
      #     compromised pod cannot escalate to os:admin by asking for it.
      #   allowedKubernetesNamespaces — only the namespace containing the
      #     backup CronJob. kube-system would be far too broad: every
      #     workload already running there could mint an etcd credential.
      #
      # Field name note: this is the pre-v1.14 spelling, which is correct for
      # var.talos_version (v1.13.4) — verified against
      # pkg/machinery/config/types/v1alpha1/v1alpha1_types.go at that tag.
      # Talos v1.14 replaces it with a separate KubeTalosAPIAccessConfig
      # document, so this block must be revisited before bumping past v1.13.
      yamlencode({
        machine = {
          features = {
            kubernetesTalosAPIAccess = {
              enabled                     = true
              allowedRoles                = ["os:etcd:backup"]
              allowedKubernetesNamespaces = ["talos-backup"]
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
      local.csi_kubelet_extra_mounts,
    ],
    each.value.gpu ? [
      yamlencode({
        machine = {
          # Talos ships with unprivileged user namespaces OFF as a
          # hardening default (user.max_user_namespaces=0) — and that is
          # what wedges rootless buildkitd here: it refuses to start with
          # "/proc/sys/user/max_user_namespaces needs to be set to
          # non-zero". Rootless is the deliberate posture for a builder
          # that eats arbitrary Dockerfiles, so the kernel limit is raised
          # instead, on this node only (the builder's home). Note the key
          # is machine.sysctls on pre-1.14 Talos (docs, "User Namespaces"
          # guide); the singular sysctl form is the v1.14 SysctlConfig
          # document, and the provider rejects it here — learned live.
          sysctls = {
            "user.max_user_namespaces" = "15000"
          }
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
    # No in-repo taint mechanism exists any more (the Flux node-taints Job
    # was retired 2026-09-16); placement is label-based.
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
              # Primary NIC: VLAN 3000 (10.30.0.x), MAC pinned in main.tf so
              # hardwareAddr is unambiguous even with two virtio NICs present
              # (a bare driver selector would multi-match).
              deviceSelector = { hardwareAddr = local.nic_macs["${each.key}-primary"] }
              addresses      = ["${local.talos_nodes[each.key].ip}/${var.network.subnet_prefix}"]
              routes         = [{ network = "0.0.0.0/0", gateway = var.network.gateway }]
            },
            {
              # VLAN 2080 "Obfuscated" second NIC (Proxmox-side tag). Static
              # address only — NO routes: the node must keep its normal
              # default via the VLAN 3000 gateway. Kubernetes pods reach this
              # NIC via Multus macvlan; the node itself never routes via it.
              deviceSelector = { hardwareAddr = local.nic_macs["${each.key}-vlan2080"] }
              addresses = [
                "${local.vlan2080_ips[each.key]}/24"
              ]
            }
          ]
          nameservers = var.network.nameservers
        }
        # Kubelet picks its node address from the FIRST address-bearing
        # interface once several exist, and with the VLAN 2080 NIC present it
        # chose 10.20.80.x — flipping every worker's InternalIP onto the
        # UniFi-auto-VPN network that Cilium's tunnel endpoints and
        # cross-site traffic must never use (learned live, 2026-09-16).
        # validSubnets pins the kubelet node IP back to the cluster VLAN.
        kubelet = {
          nodeIP = {
            validSubnets = [
              "${join(".", slice(split(".", local.talos_nodes[each.key].ip), 0, 3))}.0/${var.network.subnet_prefix}"
            ]
          }
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