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
  default     = "v1.7.5"
}

variable "kubernetes_version" {
  description = "Kubernetes version loaded by Talos"
  type        = string
  default     = "1.30.2"
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

variable "node_pools" {
  type = map(object({
    cpu_cores         = number
    memory            = number # MB
    disk_size         = number # GB
    count             = number
    talos_role        = string                     # "controlplane" or "worker"
    extensions        = optional(list(string), []) # per-pool Talos system extension images (merged with talos_default_extensions)
    enable_secure_nic = optional(bool, false)      # attach the secondary VLAN-isolated NIC to nodes in this pool
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
        "ghcr.io/siderolabs/gvisor:20240115.0",
      ]
      enable_secure_nic = false
      taints            = []
    }
    "worker-ai" = {
      cpu_cores  = 12
      memory     = 65536 # 64 GB
      disk_size  = 128
      count      = 1
      talos_role = "worker"
      extensions = [
        "ghcr.io/siderolabs/gvisor:20240115.0",
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
