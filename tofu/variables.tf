variable "proxmox_api_endpoint" {
  description = "The Proxmox VE API endpoint (e.g. https://192.168.1.100:8006/)"
  type        = string
}

variable "proxmox_ssh_username" {
  description = "The SSH user for Proxmox"
  type        = string
  default     = "root"
}

variable "proxmox_node" {
  description = "Proxmox node to deploy VMs on"
  type        = string
  default     = "pve"
}

variable "proxmox_storage_pool" {
  description = "Proxmox storage pool for virtual disks (e.g., local-lvm or local-zfs)"
  type        = string
  default     = "local-zfs"
}

variable "cluster_name" {
  description = "Name of the Talos cluster"
  type        = string
  default     = "homelab-talos"
}

variable "talos_version" {
  description = "Talos OS version"
  type        = string
  # renovate: datasource=github-releases depName=siderolabs/talos
  default = "v1.13.3"
}

variable "kubernetes_version" {
  description = "Kubernetes version loaded by Talos"
  type        = string
  default     = "1.36.1"
}

variable "cluster_endpoint" {
  description = "Control Plane Endpoint (DNS or IP of Virtual IP or first master node, e.g. https://192.168.1.200:6443)"
  type        = string
}

variable "talos_default_extensions" {
  description = "Talos system extension images installed on every node. Applied before per-pool extensions. See https://github.com/siderolabs/extensions for available images."
  type        = list(string)
  default = [
    "ghcr.io/siderolabs/nvme-cli:v2.14",     # NVMe-oF CLI (tns-fast-nvmeof tier)
    "ghcr.io/siderolabs/iscsi-tools:v0.2.0", # iSCSI initiator tools
    "ghcr.io/siderolabs/nfs-utils:v0.1.1",   # NFS client (tns-*-nfs tiers)
  ]
}

variable "network_default_bridge" {
  description = "Proxmox Linux bridge for the primary (default) NIC on all nodes"
  type        = string
  default     = "vmbr0"
}

variable "network_secure_bridge" {
  description = "Proxmox Linux bridge for the secondary 'secure' NIC"
  type        = string
  default     = "vmbr1"
}

variable "network_secure_vlan_id" {
  description = "802.1Q VLAN tag applied to the secure NIC (null = no tagging, use the bridge's native VLAN)"
  type        = number
  default     = null
}

variable "node_pools" {
  type = map(object({
    cpu_cores         = number
    memory            = number # MB
    disk_size         = number # GB
    count             = number
    talos_role        = string                     # "controlplane" or "worker"
    extensions        = optional(list(string), []) # per-pool Talos system extension images (merged with talos_default_extensions)
    enable_secure_nic = optional(bool, false)      # attach the secondary VLAN-isolated NIC to nodes in this pool
    gpu               = optional(bool, false)      # load the NVIDIA kernel modules on this pool (pair with the nvidia extensions + hostpci)
    node_labels       = optional(map(string), {})  # Kubernetes node labels applied via Talos machine.nodeLabels
    # Proxmox PCIe passthrough devices (e.g. the Tesla P100s). Each entry maps one
    # host PCI device into the VM. `id` is the host PCI address from `lspci -nn` on
    # the Proxmox host (the host must have IOMMU/vfio configured for passthrough).
    hostpci = optional(list(object({
      device = string           # VM slot: "hostpci0", "hostpci1", ...
      id     = optional(string) # host PCI address, e.g. "0000:01:00" (whole device)
      pcie   = optional(bool, true)
      rombar = optional(bool, true)
    })), [])
    taints = optional(list(object({
      key    = string
      value  = optional(string, "")
      effect = string # NoSchedule | PreferNoSchedule | NoExecute
    })), [])
  }))
  default = {
    # v0 homelab compute = four Talos VMs on the pmx-main Proxmox host (96GB / 32
    # threads, Ryzen 9 7945HX). NVIDIA driver/toolkit ship per-pool on the GPU
    # worker only (not cluster-wide), so the cp / general / cpu nodes stay clean.
    # VERIFY-BEFORE-DEPLOY: 4+12+12+64 = 92GB VMs leaves ~4GB host overhead; tune
    # if Proxmox/ZFS is starved. Cores 2+4+4+20 = 30 (+2 host).
    "cp" = {
      cpu_cores  = 2
      memory     = 4096 # 4 GB - dedicated control plane (not schedulable)
      disk_size  = 32
      count      = 1
      talos_role = "controlplane"
      node_labels = {
        "site" = "home"
      }
    }
    "wk" = {
      cpu_cores  = 4
      memory     = 12288 # 12 GB - general worker (cluster services: observability, etc.)
      disk_size  = 48
      count      = 1
      talos_role = "worker"
      node_labels = {
        "site" = "home"
      }
    }
    "wk-gpu" = {
      cpu_cores  = 4
      memory     = 12288 # 12 GB - GPU model is in VRAM; modest host RAM
      disk_size  = 48
      count      = 1
      talos_role = "worker"
      gpu        = true
      # NVIDIA driver kmod + container toolkit (only this pool has the P100).
      # VERIFY-BEFORE-DEPLOY: pin tags matching the Talos version AND a Pascal-
      # supporting driver (host runs 580.159.04); generate from the Image Factory.
      extensions = [
        "ghcr.io/siderolabs/nonfree-kmod-nvidia-production:535.247.01-v1.13.3",
        "ghcr.io/siderolabs/nvidia-container-toolkit-production:535.247.01-v1.17.8",
      ]
      # VERIFY-BEFORE-DEPLOY: real host PCI id (`lspci -nn | grep -i nvidia`);
      # Proxmox host needs IOMMU + vfio-pci bound to the P100.
      hostpci = [
        { device = "hostpci0", id = "0000:01:00" },
      ]
      node_labels = {
        "site" = "home"
      }
    }
    "wk-cpu" = {
      cpu_cores  = 20
      memory     = 65536 # 64 GB - dedicated CPU model (qwen3-coder-next, mlock'd)
      disk_size  = 48
      count      = 1
      talos_role = "worker"
      node_labels = {
        "site"     = "homelab"
        "workload" = "cpu-inference"
      }
      # Dedicated: only the tolerating CPU-inference workload schedules here.
      taints = [
        { key = "workload", value = "cpu-inference", effect = "NoSchedule" },
      ]
    }
  }

  validation {
    condition     = alltrue([for p in values(var.node_pools) : contains(["controlplane", "worker"], p.talos_role)])
    error_message = "Each node pool talos_role must be either \"controlplane\" or \"worker\"."
  }

  validation {
    condition     = alltrue([for p in values(var.node_pools) : p.count > 0])
    error_message = "Each node pool count must be greater than 0."
  }

  validation {
    condition     = alltrue([for p in values(var.node_pools) : p.cpu_cores > 0 && p.memory > 0 && p.disk_size > 0])
    error_message = "Each node pool must have positive cpu_cores, memory, and disk_size."
  }

  validation {
    condition = alltrue([
      for p in values(var.node_pools) : alltrue([
        for t in p.taints : contains(["NoSchedule", "PreferNoSchedule", "NoExecute"], t.effect)
      ])
    ])
    error_message = "Taint effect must be one of NoSchedule, PreferNoSchedule, or NoExecute."
  }
}

# Hetzner cloud/edge variables + hcloud provider arrive in v1 (public ingress).
