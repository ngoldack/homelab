locals {
  # Generate a map of "nodepool_name-instance_index" -> parameters
  vm_instances = merge([
    for pool_name, pool in var.node_pools : {
      for idx in range(pool.count) : "${pool_name}-${idx}" => {
        pool_name         = pool_name
        cpu_cores         = pool.cpu_cores
        memory            = pool.memory
        disk_size         = pool.disk_size
        talos_role        = pool.talos_role
        index             = idx
        enable_secure_nic = pool.enable_secure_nic
        extensions        = pool.extensions
        gpu               = pool.gpu
        hostpci           = pool.hostpci
        taints            = pool.taints
        # Static IP sourced from the secret network.node_ips[<pool>] (index-
        # aligned with count); missing pool/index falls back to DHCP (null).
        ip = try(local.network.node_ips[pool_name][idx], null)
      }
    }
  ]...)

  # The (first) controlplane pool's name, used to derive the cluster endpoint
  # from its static IP without hardcoding a pool name.
  controlplane_pool_name = [for k, v in var.node_pools : k if v.talos_role == "controlplane"][0]

  # Control Plane Endpoint, derived from the secret network.node_ips — never a
  # committed literal. Baked into the API server certs.
  cluster_endpoint = "https://${local.network.node_ips[local.controlplane_pool_name][0]}:${var.cluster_api_port}"
}

# Guard the shape of the secret-sourced network.node_ips against the (public)
# node_pools structure: any pool it lists must supply exactly one IP per
# instance. Pools it omits simply fall back to DHCP.
check "network_node_ips_shape" {
  assert {
    condition = alltrue([
      for pool_name, pool in var.node_pools :
      !contains(keys(local.network.node_ips), pool_name) || length(local.network.node_ips[pool_name]) == pool.count
    ])
    error_message = "secret.sops.yaml's network.node_ips must have exactly one IP per instance for any pool it lists (length == node_pools[pool].count)."
  }
}

# Download the Talos OS ISO directly onto the Proxmox node
resource "proxmox_download_file" "talos_iso" {
  node_name    = var.proxmox_node
  content_type = "iso"
  datastore_id = "local"
  file_name    = "metal-${var.talos_version}-amd64.iso"
  url          = "https://github.com/siderolabs/talos/releases/download/${var.talos_version}/metal-amd64.iso"
}

# Create Proxmox VMs for each node in the cluster
resource "proxmox_virtual_environment_vm" "talos_nodes" {
  for_each  = local.vm_instances
  name      = "${var.cluster_name}-${each.key}"
  node_name = var.proxmox_node
  tags      = ["talos", "k8s", each.value.talos_role, each.value.pool_name]

  # CPU and memory configuration derived from pool type
  cpu {
    cores = each.value.cpu_cores
    type  = "host"
  }

  memory {
    dedicated = each.value.memory
  }

  agent {
    enabled = true
  }

  # Default (primary) NIC — on the dedicated Kubernetes VLAN (tagged on vmbr0)
  network_device {
    bridge  = var.network_default_bridge
    vlan_id = local.network.vlan_id
  }

  # Secure (secondary) NIC — VLAN-isolated network; only attached when enabled for the pool
  dynamic "network_device" {
    for_each = each.value.enable_secure_nic ? [true] : []
    content {
      bridge  = var.network_secure_bridge
      vlan_id = var.network_secure_vlan_id
    }
  }

  # Root disk (Where Talos OS will be installed during apply/bootstrap)
  disk {
    datastore_id = var.proxmox_storage_pool
    interface    = "scsi0"
    size         = each.value.disk_size
    file_format  = "raw"
  }

  # CDROM drive to boot into Talos live installation ISO
  cdrom {
    file_id = proxmox_download_file.talos_iso.id
  }

  # PCIe passthrough — maps host GPUs (e.g. the Tesla P100s) into this VM. Only
  # populated for pools with `hostpci` set (worker-ai). Requires IOMMU + vfio-pci
  # on the Proxmox host.
  dynamic "hostpci" {
    for_each = { for h in each.value.hostpci : h.device => h }
    content {
      device  = hostpci.value.device
      id      = hostpci.value.id
      mapping = hostpci.value.mapping
      pcie    = hostpci.value.pcie
      rombar  = hostpci.value.rombar
    }
  }

  operating_system {
    type = "l26" # Linux 2.6+ Kernel
  }

  # Define VM boot order. Disk (scsi0) is preferred so that after Talos installs to
  # disk on first boot and reboots, the VM boots the installed system rather than the
  # live ISO again. The CDROM remains as a fallback for the initial install boot.
  boot_order = ["scsi0", "ide3"]
}
