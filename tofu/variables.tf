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
  default     = "local-lvm"
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

variable "node_pools" {
  type = map(object({
    cpu_cores  = number
    memory     = number # in MB or GB. Let's do MB for consistency or declare clearly in bytes. We'll use MB.
    disk_size  = number # in GB
    count      = number
    talos_role = string # "controlplane" or "worker"
  }))
  default = {
    "master" = {
      cpu_cores  = 2
      memory     = 4096 # 4 GB
      disk_size  = 32
      count      = 3
      talos_role = "controlplane"
    }
    "worker-default" = {
      cpu_cores  = 6
      memory     = 8192 # 8 GB
      disk_size  = 64
      count      = 2
      talos_role = "worker"
    }
    "worker-large" = {
      cpu_cores  = 12
      memory     = 49152 # 48 GB
      disk_size  = 128
      count      = 1
      talos_role = "worker"
    }
  }
}
