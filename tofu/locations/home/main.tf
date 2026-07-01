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
      }
    }
  ]...)
}

# Download the Talos OS ISO directly onto the Proxmox node
resource "proxmox_download_file" "talos_iso" {
  node_name    = var.proxmox_node
  content_type = "iso"
  datastore_id = "local"
  file_name    = "talos-${var.talos_version}-amd64.iso"
  url          = "https://github.com/siderolabs/talos/releases/download/${var.talos_version}/talos-amd64.iso"
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

  # Default (primary) NIC — general cluster traffic
  network_device {
    bridge = var.network_default_bridge
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
      device = hostpci.value.device
      id     = hostpci.value.id
      pcie   = hostpci.value.pcie
      rombar = hostpci.value.rombar
    }
  }

  operating_system {
    type = "l26" # Linux 2.6+ Kernel
  }

  # Define VM boot order. Disk (scsi0) is preferred so that after Talos installs to
  # disk on first boot and reboots, the VM boots the installed system rather than the
  # live ISO again. The CDROM remains as a fallback for the initial install boot.
  boot_order = ["scsi0", "cdrom"]
}
