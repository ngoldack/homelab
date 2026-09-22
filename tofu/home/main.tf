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

  # Kubernetes label values must be <= 63 chars, start/end alphanumeric, and
  # contain only [A-Za-z0-9._-] — free-text hardware names in proxmox_nodes
  # (e.g. "Tesla P100", "Intel UHD Graphics 770") violate that on the spaces
  # alone (confirmed live: Talos rejected the machine config outright,
  # "invalid machine node labels: label value ... is invalid"). Precompute a
  # sanitized version of every such string once, per host, instead of
  # repeating the same replace() chain at each label site below.
  hw_label_values = {
    for host_name, host in var.proxmox_nodes : host_name => {
      cpu_model = replace(replace(trimspace(host.cpu_model), "/[^A-Za-z0-9._-]+/", "-"), "/^[^A-Za-z0-9]+|[^A-Za-z0-9]+$/", "")
      gpu = { for g in host.gpu : g.name =>
        replace(replace(trimspace(g.model), "/[^A-Za-z0-9._-]+/", "-"), "/^[^A-Za-z0-9]+|[^A-Za-z0-9]+$/", "")
      }
      igpu = try(replace(replace(trimspace(host.igpu.model), "/[^A-Za-z0-9._-]+/", "-"), "/^[^A-Za-z0-9]+|[^A-Za-z0-9]+$/", ""), null)
    }
  }

  # Labels derivable from node/host facts — computed here so they are never
  # redeclared per node. Anything already in a node's custom node_labels wins
  # (custom map is merged last).
  node_labels_derived = {
    for name, n in var.nodes : name => merge(
      # Control-plane role marker.
      n.talos_role == "controlplane" ? { "node-role.kubernetes.io/control-plane" = "" } : {},
      # Custom key, not topology.kubernetes.io/zone: on the cloud side that
      # standard key is owned and silently overwritten by hcloud-cloud-
      # controller-manager (it sets it to the Hetzner datacenter, e.g.
      # "fsn1-dc8"). Home has no such controller, so kubernetes.io/zone would
      # actually stick here — but using the same custom key on both clusters
      # means "which site is this node in" resolves consistently everywhere,
      # rather than the two clusters answering the same question at two
      # different label keys.
      { "topology.homelab/site" = "home" },
      # CPU model + vCPU count from the host/node facts.
      merge(
        { "hardware/cpu.cores" = tostring(n.cpu_cores) },
        try({ "hardware/cpu.model" = local.hw_label_values[local.node_host[name]].cpu_model }, {})
      ),
      # Memory in GiB (node memory is MiB).
      { "hardware/memory.gb" = tostring(floor(n.memory / 1024)) },
      # GPU facts for GPU nodes.
      n.gpu ? merge(
        { "hardware/gpu.vram_gb" = tostring(n.gpu_vram_gb) },
        length(n.hostpci) > 0 ? { "hardware/gpu.count" = tostring(length(n.hostpci)) } : {},
        # Take the GPU model from the host's gpu inventory, matching ANY of
        # the node's hostpci device names against it (not just hostpci[0] —
        # a node with an iGPU passthrough ahead of its GPU in the list, like
        # wk-main-performance, would otherwise silently get no gpu.model
        # label at all, since hostpci[0] would be the iGPU, not the GPU).
        try({ "hardware/gpu.model" = [
          for g in var.proxmox_nodes[local.node_host[name]].gpu : local.hw_label_values[local.node_host[name]].gpu[g.name]
          if contains([for h in n.hostpci : h.device], g.name)
        ][0] }, {})
      ) : {},
      # iGPU passthrough (e.g. intel-igpu for Immich VAAPI): label the model
      # from the host's igpu inventory when the node maps it.
      contains([for h in n.hostpci : h.device], "intel-igpu") ?
      try({ "hardware/igpu" = local.hw_label_values[local.node_host[name]].igpu }, {})
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
      vm_id        = node.vm_id
      cpu_cores    = node.cpu_cores
      cpu_affinity = local.node_affinity[name]
      memory       = node.memory
      disk_size    = node.disk_size
      talos_role   = node.talos_role
      hostpci      = node.hostpci
      node_labels  = local.node_labels[name]
      ext_key      = local.extension_set_keys[name]
      # Static IP sourced from network.node_ips[<node>]; a missing
      # entry falls back to DHCP (null).
      ip = try(var.network.node_ips[name], null)
    }
  }

  # machine.install.extensions in a Talos machine config has had no effect
  # since Talos 1.10 (kept only so pre-1.10 configs still validate) — system
  # extensions must be baked into the boot image itself via Image Factory.
  # Group nodes by their fully-resolved (defaults + per-node), deduped,
  # sorted extension list, so nodes that need the same set share one
  # schematic/image instead of building one per node.
  node_extension_sets = {
    for name, node in var.nodes : name => sort(distinct(concat(var.talos_default_extensions, node.extensions)))
  }
  extension_set_keys = {
    for name, exts in local.node_extension_sets : name => substr(sha256(join(",", exts)), 0, 12)
  }
  # Two (or more) nodes legitimately sharing a key is the whole point here
  # (that's the dedup), but a `{k => v}` for-expression treats ANY repeated
  # key as a hard error regardless of whether the values match — confirmed
  # live: "Duplicate object key" the moment two nodes actually share a
  # schematic. Iterate the already-deduplicated key list instead, so this
  # for-expression only ever produces each key once.
  unique_extension_sets = {
    for key in distinct(values(local.extension_set_keys)) :
    key => local.node_extension_sets[[
      for name, k in local.extension_set_keys : name if k == key
    ][0]]
  }
  # (host, extension-set) pairs actually needed — not every host necessarily
  # runs every profile.
  iso_downloads = {
    for combo in distinct([
      for name, node in var.nodes :
      "${local.node_host[name]}::${local.extension_set_keys[name]}"
      ]) : combo => {
      host    = split("::", combo)[0]
      ext_key = split("::", combo)[1]
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
      # ipv4_addresses is a list PER NETWORK INTERFACE, not just "the NIC" —
      # a Talos guest reports lo/bond0/dummy0/teql0/tunl0/sit0/ip6tnl0 before
      # its real NIC (confirmed live via the QEMU agent's own
      # network-get-interfaces output), so a fixed index like [1] picks up
      # an empty/irrelevant interface instead.
      #
      # It also reports addresses from EVERY interface the guest runs —
      # cilium_host's 172.20.x/w32 among them, and, while cp-main still ran
      # the tailscale extension, its 100.100.x CGNAT address. Taking the first
      # non-loopback entry once pointed `node` at 100.100.108.66 and every
      # apply hung dialing an address no operator host can route (observed
      # live, 2026-09-16). Prefer an address inside the node's own cluster
      # subnet
      # (the static IP's /24) first; fall back to first-non-loopback.
      current_ip = coalesce(
        try([
          for iface in proxmox_virtual_environment_vm.talos_nodes[key].ipv4_addresses :
          iface[0]
          if length(iface) > 0 && !startswith(iface[0], "127.")
          && inst.ip != null
          && startswith(iface[0], "${join(".", slice(split(".", inst.ip), 0, 3))}.")
        ][0], null),
        try([
          for iface in proxmox_virtual_environment_vm.talos_nodes[key].ipv4_addresses :
          iface[0] if length(iface) > 0 && !startswith(iface[0], "127.")
        ][0], null),
      )
    }
  }

  # Hardware capacity summary for the cluster (used by outputs and future checks).
  # Note: node `memory` is in MiB (bpg/proxmox provider unit).
  cluster_capacity = {
    total_memory_gb = sum([for inst in local.vm_instances : inst.memory]) / 1024
    total_cpu_cores = sum([for inst in local.vm_instances : inst.cpu_cores])
    gpu_nodes       = [for name, node in var.nodes : name if node.gpu]
  }

  # Workers that carry the VLAN 2080 ("Obfuscated") second NIC — UniFi
  # auto-VPN-routes this network, so download/indexer pods egress via it.
  # Control plane and cloud (Hetzner) nodes never get it.
  vlan2080_nodes = [
    for name, node in var.nodes :
    name if node.talos_role == "worker" && coalesce(node.host, var.proxmox_node) == "pmx-main"
  ]
  # Static IPs for the VLAN 2080 interface, keyed by node name.
  # .1 is the UniFi gateway; .2-.4 are the three home workers.
  vlan2080_ips = {
    wk-main-efficiency  = "10.20.80.2"
    wk-main-performance = "10.20.80.3"
  }

  # Deterministic MACs for every VM NIC. Critical: this CHANGES the primary
  # NIC's MAC on existing VMs (Proxmox previously auto-generated it), so the
  # first apply after this change updates each VM's network config in place —
  # nodes keep their static Talos IP (address is config, not DHCP), but ARP
  # caches refresh and UniFi may briefly show new clients. Plan accordingly
  # (one node at a time if uptime matters).
  nic_macs = {
    for pair in flatten([
      for name in keys(var.nodes) : [
        { key = "${name}-primary", mac = "52:54:00:${substr(sha256("${name}-primary"), 0, 2)}:${substr(sha256("${name}-primary"), 2, 2)}:${substr(sha256("${name}-primary"), 4, 2)}" },
        { key = "${name}-vlan2080", mac = "52:54:00:${substr(sha256("${name}-vlan2080"), 0, 2)}:${substr(sha256("${name}-vlan2080"), 2, 2)}:${substr(sha256("${name}-vlan2080"), 4, 2)}" },
      ]
    ]) : pair.key => pair.mac
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

# One Image Factory schematic per unique resolved extension set (see
# local.unique_extension_sets) — this is what actually gets system
# extensions onto a node; the per-node machine.install.extensions config
# patches in talos.tf do not (see the comment there).
resource "talos_image_factory_schematic" "this" {
  for_each = local.unique_extension_sets

  schematic = yamlencode({
    customization = {
      # Serial console, set here rather than via machine.install.extraKernelArgs:
      # these nodes boot a UKI (the live config carries grubUseUKICmdline: true),
      # so the kernel command line is baked into the image by the Factory and
      # install-time extra args do not reach it. Talos's stock metal cmdline is
      # `console=tty0` only — verified against /proc/cmdline and /proc/consoles
      # on all three nodes — so without this the serial0 display configured on
      # each VM would be attached to a console nothing ever writes to.
      # tty0 is kept first so the framebuffer console (where the Talos dashboard
      # renders) still works on nodes without a GPU passed through.
      extraKernelArgs = ["console=tty0", "console=ttyS0,115200n8"]
      systemExtensions = {
        officialExtensions = each.value
      }
    }
  })
}

data "talos_image_factory_urls" "this" {
  for_each = local.unique_extension_sets

  talos_version = var.talos_version
  schematic_id  = talos_image_factory_schematic.this[each.key].id
  platform      = "metal"
  architecture  = "amd64"
}

# Download the schematic-specific Talos ISO onto each (host, extension-set)
# pair actually needed — not every host necessarily runs every profile.
resource "proxmox_download_file" "talos_iso" {
  for_each = local.iso_downloads

  provider     = proxmox.node[each.value.host]
  node_name    = each.value.host
  content_type = "iso"
  datastore_id = var.proxmox_nodes[each.value.host].iso_datastore
  # Named for the schematic ID, not the extension-set key. ext_key hashes the
  # extension list alone, so any other schematic change (kernel args, overlays,
  # meta) would produce new image CONTENT behind an unchanged FILENAME —
  # and Proxmox refuses to overwrite an existing ISO ("refusing to override
  # existing file"), leaving the apply permanently wedged until the stale file
  # is deleted by hand. Keying the name on the schematic ID makes every distinct
  # image a distinct file, so a changed schematic replaces cleanly.
  # The resource's for_each key stays ext_key: it must be known at plan time,
  # and the schematic ID is only known after apply.
  file_name = "talos-${var.talos_version}-${substr(talos_image_factory_schematic.this[each.value.ext_key].id, 0, 12)}-amd64.iso"
  url       = data.talos_image_factory_urls.this[each.value.ext_key].urls.iso

  # Note: a brand-new schematic (like the kata one) triggers a one-off build +
  # a 302-to-S3 redirect in the Talos Image Factory; the Proxmox download
  # task at v1.13.4 took >10 min there and the bpg provider (v0.112.0, no
  # per-resource download timeout attribute) interrupts it at a fixed window.
  # Recovery recipe, proven live: curl -L the urls.iso to the datastore path
  # <datastore_free_dir>/<file_name> directly on pmx-main (or elsewhere +
  # scp), verify size 576008192, then
  #   tofu import 'proxmox_download_file.talos_iso["<host>::<ext_key>"]' '<node>:iso/<file_name>'
  # and apply (the resource then only manages the already-present file).

  # Download the replacement BEFORE removing the old one. The running VMs keep
  # the outgoing ISO mounted as their cdrom, and Proxmox will not delete a
  # volume that is still attached — destroy-then-create would try exactly that
  # and strand the apply. Safe only because file_name is schematic-derived
  # above, so the incoming file never collides with the outgoing one.
  lifecycle {
    create_before_destroy = true
    # url's UNKNOWN-at-plan re-read must not force a replacement. When the
    # for_each map of data.talos_image_factory_urls.this loses any key (here:
    # the default-only extension set retiring with wk-main-media), OpenTofu
    # re-reads ALL its instances at apply time, so every consumer's url
    # becomes "known after apply" and this ForceNew attribute plans a
    # replacement even when the schematic — and therefore the file — is
    # byte-identical. create_before_destroy then downloads a file whose name
    # equals the still-present old one, and Proxmox refuses: "File already
    # exists ... managed by another resource" — the resource colliding with
    # ITSELF (learned live; it wedged the whole apply). Ignoring url is
    # safe because file_name is derived from the schematic ID itself: any
    # REAL image change (extensions, kernel args, overlays) moves the
    # filename, still triggers the replacement, and lands on a distinct
    # file — exactly the property the naming comment above wants. url
    # changes that do NOT move the filename are, by construction, re-reads
    # of the same image.
    ignore_changes = [url]
  }
}

# Create Proxmox VMs for each node in the cluster
resource "proxmox_virtual_environment_vm" "talos_nodes" {
  # node_password (root@pam), not node (api_token) — see the comment on
  # provider.proxmox.node_password in providers.tf: this resource sets
  # cpu.affinity, which Proxmox only accepts from a root@pam session.
  provider = proxmox.node_password[each.value.host]

  for_each  = local.vm_instances
  vm_id     = each.value.vm_id
  name      = "${var.cluster_name}-${each.key}"
  node_name = each.value.host
  tags      = ["talos", "k8s", each.value.talos_role, each.value.pool_name, each.value.host]

  # CPU and memory configuration per node.
  # cpu_affinity pins vCPUs to host threads resolved from the node's cpu_class
  # (i9-13900HX: performance = threads 0-15, efficiency = 16-23; uniform hosts
  # declare a single "efficiency" class). The performance worker (sandboxes,
  # image builds) lands on P-cores; control plane and the general worker stay
  # on E-cores.
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
    # Explicit ipv4=true rather than the provider's default "any global
    # unicast address" wait: on a dual-stack-capable network, IPv6 alone
    # can satisfy the default and this repo does everything over IPv4.
    wait_for_ip {
      ipv4 = true
    }
  }

  # Primary NIC — on the dedicated VM VLAN (10.30.0.0/24, VLAN 3000), tagged
  # on the host's VM bridge. The VLAN ID comes from the public network variable.
  # MACs are PINNED (see mac_address comment below): the Talos machine config
  # selects each NIC by hardwareAddr, which requires Proxmox to assign a
  # stable, known MAC.
  network_device {
    bridge      = var.proxmox_nodes[each.value.host].bridge
    vlan_id     = var.network.vlan_id
    mac_address = local.nic_macs["${each.key}-primary"]
  }

  # Second NIC — VLAN 2080 ("Obfuscated"), UniFi auto-VPN-routed. Same bridge,
  # different tag: the bridge tags the frame on veth egress, nothing host-side.
  # Only the home workers carry it (control plane and Hetzner never do).
  #
  # mac_address: Talos's virtio_net deviceSelector is AMBIGUOUS once a VM has
  # two virtio NICs (a multi-match selector is a config error / bond risk),
  # and interface names are ens18/ens19 — not stable across a NIC reorder.
  # Pinning the MACs here makes the Talos hardwareAddr selectors
  # ordering-proof and deterministic. 52:54:00 is Proxmox's KVM default OUI;
  # the low bytes hash the node name + role so the two NICs never collide.
  dynamic "network_device" {
    for_each = contains(local.vlan2080_nodes, each.key) ? [1] : []
    content {
      bridge      = var.proxmox_nodes[each.value.host].bridge
      vlan_id     = 2080
      mac_address = local.nic_macs["${each.key}-vlan2080"]
    }
  }

  # Root disk (Where Talos OS will be installed during apply/bootstrap)
  disk {
    datastore_id = try(var.proxmox_nodes[each.value.host].storage_pool, var.proxmox_storage_pool)
    interface    = "scsi0"
    size         = each.value.disk_size
    file_format  = "raw"
  }

  # Required alongside bios = "ovmf" below: the small EFI vars disk. "4m" is
  # the current recommended size (vs. the older "2m") — same datastore as
  # the root disk, no reason to split it elsewhere.
  efi_disk {
    datastore_id = try(var.proxmox_nodes[each.value.host].storage_pool, var.proxmox_storage_pool)
    file_format  = "raw"
    type         = "4m"
  }

  # CDROM drive to boot into Talos live installation ISO — the schematic
  # build matching this node's own resolved extension set (see
  # local.iso_downloads), not just any ISO on the node's host.
  cdrom {
    # The ISO is only the live install carrier; the node's OWN schematic is
    # delivered by machine.install.image (talos.tf). Factory ISOs are NOT
    # safe to fetch with parallel range downloads — a --ranged assembly
    # silently dropped the EFI partition of the kata ISO (booted nowhere,
    # hash self-consistent); single-stream downloads only (see the
    # hand-download recipe above).
    file_id = proxmox_download_file.talos_iso["${each.value.host}::${each.value.ext_key}"].id
  }

  # PCIe passthrough — maps a host PCI device (today only the Intel UHD 770
  # iGPU, on wk-main-efficiency) into this VM. Only populated for pools with
  # `hostpci` set. Requires IOMMU + vfio-pci on the Proxmox host and a PCI
  # resource mapping named per pool config (intel-igpu on pmx-main).
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

  # Every node uses the same machine type + firmware, deliberately — no
  # per-node branching (q35 was previously conditional on hostpci being set;
  # now it's unconditional so the whole fleet is consistent, not just the
  # PCIe-passthrough node).
  machine = "q35"
  bios    = "ovmf"

  # Kept so the serial console still exists and `qm terminal <vmid>` works from
  # the Proxmox host. It is no longer what the noVNC Console tab shows — see
  # the vga block below.
  serial_device {
    device = "socket"
  }

  # std, not serial0, and not virtio-gpu.
  #
  # This used to be `serial0`, which means the VM has NO emulated graphics
  # device at all and Proxmox's Console tab is a bare serial terminal. The
  # reasoning was that a passed-through GPU's driver takes VGA console
  # ownership away from the emulated display once it loads, freezing noVNC on
  # the last frame. That part is true, but the cure was worse: with no
  # framebuffer there is no UEFI/POST output, no bootloader, and no Talos
  # console dashboard — the Console tab just prints "starting serial terminal
  # on interface serial0" and then sits empty, because Talos renders its
  # dashboard on tty0 and nothing writes to ttyS0 once boot is done. A VM that
  # fails before Talos starts (as wk-main-performance did when the host could
  # not allocate its memory) shows absolutely nothing either way.
  #
  # std is the Bochs/stdvga adapter: it works off the plain EFI framebuffer
  # with NO guest driver at any stage, so POST, the bootloader and the Talos
  # dashboard are all visible. virtio-gpu would need the guest's virtio_gpu
  # DRM driver (and VirtioGpuDxe in OVMF) to show anything, which buys
  # performance and resizing this text console has no use for, at the cost of
  # a driver dependency exactly when things are broken enough to need looking
  # at.
  #
  # The passthrough trade-off is accepted deliberately: on the iGPU node the
  # graphical console will freeze once i915 claims it, but POST and early boot —
  # the part worth seeing — appear first.
  # Serial is not lost in either case; it moves to `qm terminal <vmid>`.
  #
  # The kernel cmdline already targets both consoles
  # (console=tty0 console=ttyS0,115200n8, set via the Image Factory schematic),
  # so no Talos-side change is needed.
  #
  # Takes effect on the VM's next power cycle — a display adapter cannot be
  # hot-changed.
  #
  # PER-NODE, and this distinction is load-bearing rather than tidy. Nodes with
  # a passed-through GPU keep serial0 (no emulated display at all): giving such
  # a VM BOTH an emulated VGA and a real passed-through GPU puts two devices
  # into VGA arbitration — visible in the guest log as
  # "vgaarb: VGA decodes changed" right after the driver binds — a documented
  # way to hang a passthrough guest at boot. wk-main-media did exactly that:
  # fine for weeks on serial0, and the first boot after switching it to std
  # never came back, while the GPU-less nodes cycled cleanly.
  #
  # Being precise about the evidence, because the rule is broader than what was
  # actually observed: when the P100 node existed it also had passthrough and
  # DID come back on std, so the hang was specific to the Intel iGPU, which
  # participates in VGA arbitration in a way a compute-only card does not.
  # Keying off hostpci rather than "is it an iGPU" stays deliberately
  # conservative: it applies serial0 to any future passthrough pool, which is
  # the right side to err on for a setting whose failure mode is a node that
  # never boots.
  vga {
    type = length(each.value.hostpci) > 0 ? "serial0" : "std"
  }

  # Define VM boot order. Disk (scsi0) is preferred so that after Talos installs to
  # disk on first boot and reboots, the VM boots the installed system rather than the
  # live ISO again. The CDROM remains as a fallback for the initial install boot.
  boot_order = ["scsi0", "ide3"]
}

# local.cluster_endpoint indexes var.network.node_ips by the control plane's
# node name — without this check, forgetting to give a new control-plane
# node a static IP surfaces only as OpenTofu's generic "the given key does
# not exist" error deep in a `local.cluster_endpoint` reference, far from
# the actual misconfiguration in terraform.tfvars.
check "controlplane_has_static_ip" {
  assert {
    condition     = contains(keys(var.network.node_ips), local.controlplane_node_name)
    error_message = "The control-plane node ${local.controlplane_node_name} must have a static IP in network.node_ips — it derives cluster_endpoint."
  }
}

# data.talos_machine_configuration.controlplane in talos.tf is a SINGLETON:
# its install image and nodeLabels are hardcoded to
# keys(local.controlplane_instances)[0], because it isn't for_each'd the way
# the worker data source is. talos_machine_configuration_apply.controlplane
# DOES for_each over every control-plane node, so a second control plane
# would silently re-render and push the FIRST control plane's own config
# (install image, labels) onto itself under a different node's identity —
# keys() sorts lexicographically, so which node is "first" isn't even stable
# across a tfvars reorder. Enforced here rather than by generalizing the
# singleton, since this repo has never needed more than one control plane and
# multi-CP wiring (etcd bootstrap ordering, cluster_endpoint as a single IP)
# would need its own design pass regardless.
check "single_controlplane_only" {
  assert {
    condition     = length(local.controlplane_instances) == 1
    error_message = "tofu/home supports exactly one control-plane node today: data.talos_machine_configuration.controlplane in talos.tf is a singleton indexed by keys(local.controlplane_instances)[0], not for_each'd. Adding a second control-plane node needs that data source (and its install-image/nodeLabels indexing) generalized first."
  }
}

# Enforce the host memory floor so the fleet cannot exceed
# (max_memory_gb - reserved.memory = 88 GiB) and then lose a VM to the kernel
# OOM killer. Policy: the host gets a 6 GiB floor (Proxmox + kernel + PVE +
# SHRUNK ZFS ARC) and everything else goes to the VMs. Ballooning is OFF
# (memory.dedicated in the VM block), so allocated == committed with no
# reclaim. This check is the guardrail that keeps the fleet at-or-under the
# floor; the ARC cap (zfs_arc_max) is what keeps the host INSIDE its 6 GiB.
# History: the fleet sat ~0-free at 84 GiB on 2026-09-20 and the kernel OOM
# killer reaped qemu processes for VMs 104/105 on 2026-09-11/12 — cp-main
# (103) is the fatal victim.
check "fleet_memory_within_host_ceiling" {
  assert {
    condition     = local.cluster_capacity.total_memory_gb <= var.proxmox_nodes[var.proxmox_node].max_memory_gb - var.proxmox_nodes[var.proxmox_node].reserved.memory
    error_message = "Fleet commits ${local.cluster_capacity.total_memory_gb} GiB; the host ceiling ${var.proxmox_nodes[var.proxmox_node].max_memory_gb} GiB minus the ${var.proxmox_nodes[var.proxmox_node].reserved.memory} GiB host floor allows at most ${var.proxmox_nodes[var.proxmox_node].max_memory_gb - var.proxmox_nodes[var.proxmox_node].reserved.memory} GiB of VM memory. Reduce node memory, or shrink the ARC - never grow past the floor or the host OOM-kills a VM (cp-main 103 is the single control plane)."
  }
}

