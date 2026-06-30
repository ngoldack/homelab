variable "proxmox_api_endpoint" {
  description = "The Proxmox VE API endpoint (e.g. https://192.168.1.100:8006/)"
  type        = string
}

variable "proxmox_api_username" {
  description = "The Proxmox VE API user name"
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
    # NVIDIA driver kmod + container toolkit for the P100 (single-node = the
    # control-plane IS the GPU node, so these must be cluster-wide defaults).
    # VERIFY-BEFORE-DEPLOY: pin tags matching the Talos version AND a Pascal-
    # supporting driver (the host runs 580.159.04); generate from the Image Factory.
    "ghcr.io/siderolabs/nonfree-kmod-nvidia-production:535.247.01-v1.13.3",
    "ghcr.io/siderolabs/nvidia-container-toolkit-production:535.247.01-v1.17.8",
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

variable "hetzner_objectstorage_location" {
  description = "Hetzner Object Storage location/region. Object Storage is EU-only."
  type        = string
  default     = "fsn1" # Falkenstein
  validation {
    condition     = contains(["fsn1", "nbg1", "hel1"], var.hetzner_objectstorage_location)
    error_message = "Hetzner Object Storage location must be one of: fsn1, nbg1, hel1."
  }
}

variable "hetzner_dr_bucket_name" {
  description = "Hetzner Object Storage bucket for offsite disaster-recovery backups. Must be globally unique across ALL Hetzner customers, 3-63 chars, lowercase, no dots."
  type        = string
  default     = "ngoldack-homelab-dr"
  validation {
    condition     = can(regex("^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$", var.hetzner_dr_bucket_name))
    error_message = "Bucket name must be 3-63 chars, lowercase alphanumeric or hyphen, and not start/end with a hyphen (no dots allowed)."
  }
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
    # v0 MVP: a single Talos node on the homelab Proxmox host. It is a control
    # plane that also schedules all workloads (allowSchedulingOnControlPlanes,
    # set in talos.tf), with the Tesla P100 passed through. Multi-node / the
    # dual-P100 ai-host / the Hetzner cloud node arrive in later phases.
    "pmx-main" = {
      cpu_cores  = 16
      memory     = 90112 # 88 GB (headroom on the 96GB host)
      disk_size  = 256
      count      = 1
      talos_role = "controlplane"
      extensions = []
      gpu        = true
      # VERIFY-BEFORE-DEPLOY: set the real host PCI id from `lspci -nn | grep -i
      # nvidia`; the Proxmox host needs IOMMU + vfio-pci bound to the P100.
      hostpci = [
        { device = "hostpci0", id = "0000:01:00" },
      ]
      enable_secure_nic = false
      taints            = [] # single node: schedulable, no taint
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
