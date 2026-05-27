locals {
  # Generate a map of "nodepool_name-instance_index" -> parameters
  vm_instances = merge([
    for pool_name, pool in var.node_pools : {
      for idx in range(pool.count) : "${pool_name}-${idx}" => {
        pool_name  = pool_name
        cpu_cores  = pool.cpu_cores
        memory     = pool.memory
        disk_size  = pool.disk_size
        talos_role = pool.talos_role
        index      = idx
      }
    }
  ]...)
}

# Download the Talos OS ISO directly onto the Proxmox node
resource "proxmox_virtual_environment_download_file" "talos_iso" {
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

  network_device {
    bridge = "vmbr0"
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
    file_id = proxmox_virtual_environment_download_file.talos_iso.id
  }

  operating_system {
    type = "l26" # Linux 2.6+ Kernel
  }

  # Define VM boot order to ensure it boots from CDROM first to boot into the Talos live ISO
  boot_order = ["cdrom", "scsi0"]
}
