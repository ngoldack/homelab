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
    "ghcr.io/siderolabs/netbird:0.71.2",     # WireGuard-based Zero Trust overlay network
    "ghcr.io/siderolabs/nvme-cli:v2.14",     # NVMe-oF command line interface
    "ghcr.io/siderolabs/iscsi-tools:v0.2.0", # iSCSI initiator tools (required for Longhorn, etc.)
    "ghcr.io/siderolabs/nfs-utils:v0.1.1",   # rpcbind + rpc.statd for NFSv3 file locking
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
    "master" = {
      cpu_cores         = 4
      memory            = 4096 # 4 GB
      disk_size         = 32
      count             = 1
      talos_role        = "controlplane"
      extensions        = []
      enable_secure_nic = false
      taints            = []
    }
    "worker-default" = {
      cpu_cores  = 8
      memory     = 24576 # 24 GB
      disk_size  = 64
      count      = 1
      talos_role = "worker"
      extensions = [
        "ghcr.io/siderolabs/gvisor:20260427.0",
      ]
      enable_secure_nic = false
      taints            = []
    }
    "worker-ai" = {
      cpu_cores  = 12
      memory     = 86016 # 84 GB
      disk_size  = 256
      count      = 1
      talos_role = "worker"
      # 3x Tesla P100 (Pascal). vLLM dropped Pascal, so this node runs Ollama
      # (llama.cpp) which still supports it. NVIDIA driver kmod + container
      # toolkit ship as Talos system extensions; the kernel modules are loaded
      # via gpu=true below.
      # VERIFY-BEFORE-DEPLOY: these extension tags MUST match the Talos version
      # (v1.13.3) AND a driver branch that still supports Pascal (the -production
      # 535/550 branch does; the newest branches drop it). Generate the exact
      # tags from the Talos Image Factory / github.com/siderolabs/extensions.
      extensions = [
        "ghcr.io/siderolabs/gvisor:20260427.0",
        "ghcr.io/siderolabs/nonfree-kmod-nvidia-production:535.247.01-v1.13.3",
        "ghcr.io/siderolabs/nvidia-container-toolkit-production:535.247.01-v1.17.8",
      ]
      gpu = true
      # VERIFY-BEFORE-DEPLOY: replace the `id`s with the real host PCI addresses
      # from `lspci -nn | grep -i nvidia` on the Proxmox host, and ensure the host
      # has IOMMU + vfio-pci bound to the P100s. Whole-device passthrough (no mdev).
      hostpci = [
        { device = "hostpci0", id = "0000:01:00" },
        { device = "hostpci1", id = "0000:02:00" },
        { device = "hostpci2", id = "0000:03:00" },
      ]
      enable_secure_nic = false
      taints = [
        { key = "ai", value = "true", effect = "NoSchedule" },
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

# --- Hetzner edge node (edge.tf) ---

variable "hetzner_edge_server_type" {
  description = "Hetzner Cloud server type for the edge node"
  type        = string
  default     = "cx22" # 2 vCPU / 4 GB, ~EUR 3.49/mo
}

variable "hetzner_edge_location" {
  description = "Hetzner Cloud location for the edge node"
  type        = string
  default     = "nbg1" # Nuremberg
}

variable "edge_admin_ips" {
  description = "CIDRs allowed to reach the edge Talos API (port 50000)."
  type        = list(string)
  # VERIFY-BEFORE-DEPLOY: restrict to your admin / mesh CIDRs (spec: admins only).
  default = ["0.0.0.0/0"]
}
