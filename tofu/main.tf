locals {
  # Keep one provider instance beyond the managed node set so OpenTofu can
  # destroy a node's resources before that provider configuration disappears.
  proxmox_provider_nodes = merge(var.proxmox_nodes, {
    "__provider_keepalive" = var.proxmox_nodes[var.proxmox_node]
  })

  # Expand a class range string (e.g. "0-15" or "0,2,4-6") into a sorted list
  # of thread ids. sort() is lexical, so ids are zero-padded to 3 digits for a
  # numeric sort, then converted back to numbers. The threads reserved for the
  # host OS (host.reserved.cpu, the lowest threads of the reserved class) are
  # dropped so guests never land on the cores the host runs on.
  class_threads = {
    for host_name, host in var.proxmox_nodes : host_name => {
      for class_name, spec in host.cpu_classes : class_name => slice(
        [
          for t in sort(distinct(flatten([
            for part in split(",", spec) :
            can(regex("-", part)) ? [for i in range(tonumber(split("-", part)[0]), tonumber(split("-", part)[1]) + 1) : format("%03d", i)] : [format("%03d", tonumber(part))]
          ]))) : tonumber(t)
        ],
        host.reserved.cpu.class == class_name ? host.reserved.cpu.count : 0,
        length([
          for t in sort(distinct(flatten([
            for part in split(",", spec) :
            can(regex("-", part)) ? [for i in range(tonumber(split("-", part)[0]), tonumber(split("-", part)[1]) + 1) : format("%03d", i)] : [format("%03d", tonumber(part))]
          ]))) : tonumber(t)
        ])
      )
    }
  }

  # Allocation order for each class = its threads sorted numerically: physical
  # cores first (first SMT sibling), then the second sibling of each core, as
  # long as the class ranges keep SMT siblings adjacent (e.g. 0-15 = 8 P-cores
  # × 2 threads). Nodes consume threads in their declaration order
  # (var.nodes map order), so pins are stable and predictable from the plan.
  node_host = { for name, n in var.nodes : name => coalesce(n.host, var.proxmox_node) }
  class_node_names = {
    for host_name, host in var.proxmox_nodes : host_name => {
      for class_name in keys(host.cpu_classes) : class_name => [
        for name, n in var.nodes : name
        if local.node_host[name] == host_name && n.cpu_class == class_name && n.cpu_affinity == null
      ]
    }
  }

  # Deterministically slice each class's thread list across its nodes. Nodes
  # consume threads in declaration order; offsets are the cumulative cpu_cores
  # of all earlier nodes in the same class.
  class_alloc = merge(flatten([
    for host_name, classes in local.class_node_names : [
      for class_name, node_names in classes : {
        for idx, name in node_names : "${host_name}/${name}" => slice(
          local.class_threads[host_name][class_name],
          length([for prev in slice(node_names, 0, idx) : prev]) == 0 ? 0 : sum([for prev in slice(node_names, 0, idx) : var.nodes[prev].cpu_cores]),
          (length([for prev in slice(node_names, 0, idx) : prev]) == 0 ? 0 : sum([for prev in slice(node_names, 0, idx) : var.nodes[prev].cpu_cores])) + var.nodes[name].cpu_cores
        )
      }
    ]
  ])...)

  # Resolved Proxmox cpu.affinity per node: explicit cpu_affinity wins,
  # otherwise the class-allocated thread set, otherwise unpinned (null).
  node_affinity = {
    for name, n in var.nodes : name =>
    n.cpu_affinity != null ? n.cpu_affinity :
    n.cpu_class != null ? join(",", [for t in local.class_alloc["${local.node_host[name]}/${name}"] : tostring(t)]) :
    null
  }

  # Labels derivable from node/host facts — computed here so they are never
  # redeclared per node. Anything already in a node's custom node_labels wins
  # (custom map is merged last).
  node_labels_derived = {
    for name, n in var.nodes : name => merge(
      # Control-plane role marker.
      n.talos_role == "controlplane" ? { "node-role.kubernetes.io/control-plane" = "" } : {},
      # Topology zone = site (the module also sets the short "site" label).
      { "topology.kubernetes.io/zone" = "home" },
      # CPU model + vCPU count from the host/node facts.
      merge(
        { "hardware/cpu.cores" = tostring(n.cpu_cores) },
        try({ "hardware/cpu.model" = var.proxmox_nodes[local.node_host[name]].cpu_model }, {})
      ),
      # Memory in GiB (node memory is MiB).
      { "hardware/memory.gb" = tostring(floor(n.memory / 1024)) },
      # GPU facts for GPU nodes.
      n.gpu ? merge(
        { "hardware/gpu.vram_gb" = tostring(n.gpu_vram_gb) },
        length(n.hostpci) > 0 ? { "hardware/gpu.count" = tostring(length(n.hostpci)) } : {},
        # Take the GPU model from the host's gpu inventory matching the first
        # passthrough mapping name, if the host declares it.
        try({ "hardware/gpu.model" = [
          for g in var.proxmox_nodes[local.node_host[name]].gpu : g.model
          if g.name == n.hostpci[0].device
        ][0] }, {})
      ) : {},
      # iGPU passthrough (e.g. intel-igpu for Immich VAAPI): label the model
      # from the host's igpu inventory when the node maps it.
      contains([for h in n.hostpci : h.device], "intel-igpu") ?
      try({ "hardware/igpu" = var.proxmox_nodes[local.node_host[name]].igpu.model }, {})
      : {}
    )
  }

  # Final labels per node = derived base + the node's custom labels (override).
  node_labels = {
    for name, n in var.nodes : name => merge(local.node_labels_derived[name], n.node_labels)
  }

  # One VM per declared node. Nodes are assigned to a specific Proxmox host and
  # are NOT assumed to be part of a Proxmox cluster. If a node does not declare
  # a host, fall back to the default host configured for the location.
  vm_instances = {
    for name, node in var.nodes : name => {
      pool_name    = name
      host         = local.node_host[name]
      cpu_cores    = node.cpu_cores
      cpu_affinity = local.node_affinity[name]
      memory       = node.memory
      disk_size    = node.disk_size
      talos_role   = node.talos_role
      hostpci      = node.hostpci
      node_labels  = local.node_labels[name]
      # Static IP sourced from network.node_ips[<node>]; a missing
      # entry falls back to DHCP (null).
      ip = try(var.network.node_ips[name], null)
    }
  }

  # The (first) control plane node's name, used to derive the cluster endpoint
  # from its static IP without hardcoding a node name.
  controlplane_node_name = [for k, v in var.nodes : k if v.talos_role == "controlplane"][0]

  # Control Plane Endpoint, derived from network.node_ips. Baked into the API
  # server certs.
  cluster_endpoint = "https://${var.network.node_ips[local.controlplane_node_name]}:${var.cluster_api_port}"

  # Resolved per-node Talos metadata passed into the Talos module.
  # `current_ip` is the live maintenance-mode (DHCP) address reported by the VM;
  # `ip` is the pinned static address from the secret network block.
  talos_nodes = {
    for key, inst in local.vm_instances : key => {
      pool_name  = inst.pool_name
      talos_role = inst.talos_role
      ip         = inst.ip
      current_ip = try(proxmox_virtual_environment_vm.talos_nodes[key].ipv4_addresses[1][0], null)
    }
  }

  # Hardware capacity summary for the cluster (used by outputs and future checks).
  # Note: node `memory` is in MiB (bpg/proxmox provider unit).
  cluster_capacity = {
    total_memory_gb = sum([for inst in local.vm_instances : inst.memory]) / 1024
    total_cpu_cores = sum([for inst in local.vm_instances : inst.cpu_cores])
    gpu_nodes       = [for name, node in var.nodes : name if node.gpu]
  }
}

# Guard against typos in network.node_ips: every key must
# match a declared node. Nodes absent from node_ips simply fall back to DHCP.
check "network_node_ips_known_nodes" {
  assert {
    condition     = alltrue([for name in keys(var.network.node_ips) : contains(keys(var.nodes), name)])
    error_message = "network.node_ips contains keys that do not match any node declared in var.nodes."
  }
}

# Download the Talos OS ISO directly onto each Proxmox node that is used.
resource "proxmox_download_file" "talos_iso" {
  for_each = var.proxmox_nodes

  provider     = proxmox.node[each.key]
  node_name    = each.key
  content_type = "iso"
  datastore_id = each.value.iso_datastore
  file_name    = "metal-${var.talos_version}-amd64.iso"
  url          = "https://github.com/siderolabs/talos/releases/download/${var.talos_version}/metal-amd64.iso"
}

# Create Proxmox VMs for each node in the cluster
resource "proxmox_virtual_environment_vm" "talos_nodes" {
  provider = proxmox.node[each.value.host]

  for_each  = local.vm_instances
  name      = "${var.cluster_name}-${each.key}"
  node_name = each.value.host
  tags      = ["talos", "k8s", each.value.talos_role, each.value.pool_name, each.value.host]

  # CPU and memory configuration per node.
  # cpu_affinity pins vCPUs to host threads resolved from the node's cpu_class
  # (i9-13900HX: performance = threads 0-15, efficiency = 16-23; uniform hosts
  # declare a single "efficiency" class). The AI worker lands on P-cores for
  # low-latency inference; control plane and workers stay on E-cores.
  # Note: affinity requires root@pam auth on the Proxmox API token.
  cpu {
    cores    = each.value.cpu_cores
    type     = "host"
    affinity = each.value.cpu_affinity
  }

  memory {
    # dedicated-only (no floating) disables ballooning: the host never reclaims
    # guest RAM. Required for tightly packing guests up to the host's usable
    # memory (max_memory_gb minus os_reserved_memory_gb) without the host and
    # guests fighting over the reserve.
    dedicated = each.value.memory
  }

  agent {
    enabled = true
  }

  # Primary NIC — on the dedicated VM VLAN (10.20.11.0/24, VLAN 2011), tagged
  # on the host's VM bridge. The VLAN ID comes from the public network variable.
  network_device {
    bridge  = var.proxmox_nodes[each.value.host].bridge
    vlan_id = var.network.vlan_id
  }

  # Root disk (Where Talos OS will be installed during apply/bootstrap)
  disk {
    datastore_id = try(var.proxmox_nodes[each.value.host].storage_pool, var.proxmox_storage_pool)
    interface    = "scsi0"
    size         = each.value.disk_size
    file_format  = "raw"
  }

  # CDROM drive to boot into Talos live installation ISO
  cdrom {
    file_id = proxmox_download_file.talos_iso[each.value.host].id
  }

  # PCIe passthrough — maps host GPUs (e.g. the Tesla P100s) into this VM. Only
  # populated for pools with `hostpci` set (wk-main-performance). Requires IOMMU +
  # vfio-pci on the Proxmox host and PCI resource mappings named per pool
  # config (e.g. nvidia-p100 on pmx-main; nvidia-p100-0/1 on pmx-ai).
  # bpg/proxmox: `device` is the hostpciX slot, `mapping` is the Proxmox
  # resource mapping name (works with API-token auth; `id` would need root
  # password auth). pcie=true requires the q35 machine type.
  dynamic "hostpci" {
    for_each = { for idx, h in each.value.hostpci : "hostpci${idx}" => h }
    content {
      device  = hostpci.key
      id      = hostpci.value.id
      mapping = coalesce(hostpci.value.mapping, hostpci.value.device)
      pcie    = hostpci.value.pcie
      rombar  = hostpci.value.rombar
    }
  }

  operating_system {
    type = "l26" # Linux 2.6+ Kernel
  }

  # q35 is required for PCIe passthrough ports (hostpci pcie=true).
  machine = length(each.value.hostpci) > 0 ? "q35" : null

  # Define VM boot order. Disk (scsi0) is preferred so that after Talos installs to
  # disk on first boot and reboots, the VM boots the installed system rather than the
  # live ISO again. The CDROM remains as a fallback for the initial install boot.
  boot_order = ["scsi0", "ide3"]
}

module "talos_cluster" {
  source = "./modules/talos"

  cluster_name             = var.cluster_name
  cluster_endpoint         = local.cluster_endpoint
  talos_version            = var.talos_version
  kubernetes_version       = var.kubernetes_version
  talos_default_extensions = var.talos_default_extensions
  pod_cidr                 = var.pod_cidr
  service_cidr             = var.service_cidr
  location                 = "home"
  network                  = var.network
  nodes                    = local.talos_nodes

  # Per-node machine-config shaping (each node renders its own worker config).
  # node_labels here are the derived+merged set computed in locals.
  node_pools = {
    for name, node in var.nodes : name => {
      talos_role  = node.talos_role
      extensions  = node.extensions
      gpu         = node.gpu
      gpu_vram_gb = node.gpu_vram_gb
      node_labels = local.node_labels[name]
      taints      = node.taints
    }
  }

}
