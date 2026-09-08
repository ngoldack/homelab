variable "state_encryption_passphrase" {
  description = "Passphrase used to encrypt local OpenTofu state. Supplied by Taskfile from SOPS."
  type        = string
  sensitive   = true
  nullable    = false
}

variable "proxmox_nodes" {
  description = "Independent Proxmox nodes keyed by hostname. There is no shared cluster-level API endpoint; each host exposes its own endpoint and token, and may have its own storage pool, ISO datastore and VM bridge."
  type = map(object({
    endpoint      = string
    api_token     = optional(string)
    ssh_username  = optional(string, "root")
    insecure      = optional(bool, true)
    storage_pool  = optional(string, "local-lvm")
    iso_datastore = optional(string, "local")
    bridge        = optional(string, "vmbr0")

    # Hardware capacity constraints used for validation and scheduling. These
    # describe the physical host, not what is currently allocated.
    max_memory_gb = optional(number)
    max_cpu_cores = optional(number)
    cpu_threads   = optional(number)
    cpu_model     = optional(string)

    # Resources reserved for the Proxmox OS itself (host daemons + a capped
    # ZFS ARC, plus the host threads the OS runs on). Guests are allowed to
    # consume max_memory_gb minus reserved.memory, and each CPU class minus the
    # reserved threads in reserved.cpu. Keep it minimal to maximize guest
    # allocation; ~2 GiB + 2 efficiency threads suits a headless host.
    reserved = optional(object({
      memory = optional(number, 2)
      cpu = optional(object({
        class = string
        count = number
      }), { class = "efficiency", count = 2 })
    }), { memory = 2, cpu = { class = "efficiency", count = 2 } })

    # Thread classes of the host CPU, named by performance tier, as Proxmox
    # cpu.affinity ranges (0-indexed threads, inclusive, siblings adjacent).
    # Hybrid (big.LITTLE) CPUs declare "performance" and "efficiency" classes;
    # uniform CPUs declare a single "efficiency" class covering all threads.
    # A node's `cpu_class` picks its cores from the matching class.
    cpu_classes = optional(map(string), {})
    gpu = optional(list(object({
      name                = string
      model               = string
      vram_gb             = number
      passthrough_mapping = optional(string)
    })), [])
    igpu = optional(object({
      model   = string
      vram_gb = number
    }))
  }))
  default = {
    pmx-main = {
      endpoint      = "https://pmx-main:8006/"
      storage_pool  = "local-zfs"
      max_memory_gb = 96
      max_cpu_cores = 24
      cpu_threads   = 32
      cpu_model     = "i9-13900HX"
      # Minimal host reserve: headless Talos-worker host, ARC capped on-host.
      reserved = {
        memory = 2
        cpu    = { class = "efficiency", count = 2 }
      }
      # big.LITTLE: 8 P-cores (HT) = threads 0-15, 16 E-cores (no HT) = 16-31.
      cpu_classes = {
        performance = "0-15"
        efficiency  = "16-31"
      }
      igpu = {
        model   = "Intel UHD Graphics 770"
        vram_gb = 4
      }
      gpu = [
        {
          name    = "nvidia-p100-x16"
          model   = "Tesla P100"
          vram_gb = 16
        }
      ]
    }
    pmx-infra = {
      endpoint      = "https://pmx-infra:8006/"
      storage_pool  = "local-zfs"
      max_memory_gb = 32
      max_cpu_cores = 4
      cpu_threads   = 8
      cpu_model     = "Xeon E5-2430 v2"
      # Minimal host reserve (headless, ARC capped on-host).
      reserved = {
        memory = 2
        cpu    = { class = "efficiency", count = 2 }
      }
      # Uniform CPU (no big.LITTLE): one class covering all threads.
      cpu_classes = {
        efficiency = "0-7"
      }
      gpu = []
    }
  }

  # The reserved.cpu.class must be a class the host actually declares.
  validation {
    condition = alltrue([
      for host_name, host in var.proxmox_nodes :
      contains(keys(host.cpu_classes), host.reserved.cpu.class)
    ])
    error_message = "reserved.cpu.class must be one of the host's cpu_classes (e.g. \"efficiency\")."
  }

  # The reserved threads must fit inside their class.
  validation {
    condition = alltrue([
      for host_name, host in var.proxmox_nodes :
      !contains(keys(host.cpu_classes), host.reserved.cpu.class) ? true :
      host.reserved.cpu.count <= sum([for part in split(",", host.cpu_classes[host.reserved.cpu.class]) :
        can(regex("-", part)) ? tonumber(split("-", part)[1]) - tonumber(split("-", part)[0]) + 1 : 1
      ])
    ])
    error_message = "reserved.cpu.count exceeds the thread count of reserved.cpu.class."
  }
}

variable "proxmox_ssh_username" {
  description = "Default SSH user used for Proxmox host operations when a node does not override it."
  type        = string
  default     = "root"
}

variable "proxmox_node" {
  description = "Default Proxmox node used when a pool does not declare a host. This is the fallback for a single-node bootstrap."
  type        = string
  default     = "pmx-main"
}

variable "proxmox_storage_pool" {
  description = "Fallback storage pool for VM disks used when a node-specific storage_pool is not set."
  type        = string
  default     = "local-zfs"
}

variable "cluster_name" {
  description = "Name of the Talos cluster"
  type        = string
  default     = "homelab"
}

variable "network" {
  description = "Public cluster network configuration. node_ips maps node names to static IPs; omitted nodes use DHCP."
  type = object({
    vlan_id       = number
    subnet_prefix = number
    gateway       = string
    nameservers   = list(string)
    node_ips      = map(string)
  })
}

variable "cluster_domain" {
  description = "Public domain for cluster services exposed under {app}.{namespace}.svc.<cluster_domain>"
  type        = string
}

variable "talos_version" {
  description = "Talos OS version"
  type        = string
  default     = "v1.13.4"
}

variable "kubernetes_version" {
  description = "Kubernetes version installed by Talos"
  type        = string
  default     = "1.36.2"
}

variable "cluster_api_port" {
  description = "Kubernetes API port"
  type        = number
  default     = 6443
}

variable "cloud_ingress_server_type" {
  description = "Hetzner Cloud server type for the public ingress worker"
  type        = string
  default     = "cax11"
}

variable "cloud_ingress_location" {
  description = "Hetzner Cloud location for the public ingress worker"
  type        = string
  default     = "fsn1"
}

variable "cloud_cluster_domain" {
  description = "Internal Kubernetes DNS domain for the independent cloud cluster."
  type        = string
  default     = "cloud.cluster.local"
}

variable "cloud_ingress_image" {
  description = "Hetzner Cloud image for the public ingress worker. 'talos' resolves to the official Talos public ISO (schematic ce4c980…)."
  type        = string
  default     = "talos"
}

variable "cloud_ingress_image_id" {
  description = "Optional Hetzner snapshot/image ID overriding cloud_ingress_image (e.g. a self-built Talos snapshot via hcloud-upload-image)"
  type        = string
  default     = null
}

variable "cloud_ingress_ssh_key_name" {
  description = "Existing Hetzner Cloud SSH key name used by the ingress worker"
  type        = string
  default     = "homelab"
}

variable "cloud_ingress_ssh_source_ips" {
  description = "CIDRs allowed to SSH to the public ingress worker; empty means SSH is closed"
  type        = list(string)
  default     = []
}

variable "talos_default_extensions" {
  description = "Talos system extensions installed on every node. Defaults include the TrueNAS-CSI storage clients: nfs-utils (rpcbind/rpc.statd for NFS mounts) and nvme-cli (NVMe-oF userspace tooling; the nvme_tcp/nvme_fabrics kernel modules ship in the Talos kernel)."
  type        = list(string)
  default = [
    "siderolabs/nfs-utils",
    "siderolabs/nvme-cli",
  ]
}

# Cluster-internal networks. These live entirely inside Kubernetes and must
# never overlap the home VLANs (10.20.0.0/24 mgmt, 10.20.10.0/24 server,
# 10.20.11.0/24 VMs) — hence 172.x ranges instead of 10.x.
variable "pod_cidr" {
  description = "Kubernetes pod network CIDR (cluster-internal only)"
  type        = string
  default     = "172.20.0.0/16"
}

variable "service_cidr" {
  description = "Kubernetes service network CIDR (cluster-internal only)"
  type        = string
  default     = "172.21.0.0/16"
}

variable "nodes" {
  description = "Talos nodes for the home cluster, keyed by node name (VMs are named \"<cluster>-<name>\"). Each node declares its vCPU count (cpu_cores) and a cpu_class (\"performance\"/\"efficiency\") selecting which host core class it is pinned to — tofu assigns the actual threads deterministically. Alternatively set an explicit cpu_affinity string to bypass class-based assignment. Static IPs live in the SOPS secret, keyed by the same node names (network.node_ips)."
  type = map(object({
    host         = optional(string)
    cpu_cores    = number
    cpu_class    = optional(string)
    cpu_affinity = optional(string)
    memory       = number
    disk_size    = number
    talos_role   = string
    extensions   = optional(list(string), [])
    gpu          = optional(bool, false)
    gpu_vram_gb  = optional(number, 0)
    node_labels  = optional(map(string), {})
    hostpci = optional(list(object({
      # device is the Proxmox PCI resource mapping name (e.g. "nvidia-p100"),
      # translated to hostpciX slots + mapping= in the VM resource.
      device  = string
      id      = optional(string)
      mapping = optional(string)
      pcie    = optional(bool, true)
      rombar  = optional(bool, true)
    })), [])
    taints = optional(list(object({
      key    = string
      value  = optional(string, "")
      effect = string
    })), [])
  }))
  default = {
    cp = {
      host       = "pmx-main"
      cpu_cores  = 2
      cpu_class  = "efficiency"
      memory     = 4096
      disk_size  = 32
      talos_role = "controlplane"
    }
    wk-main-efficiency = {
      host       = "pmx-main"
      cpu_cores  = 6
      cpu_class  = "efficiency"
      memory     = 24576
      disk_size  = 48
      talos_role = "worker"
      node_labels = {
        "node.kubernetes.io/instance-type" = "worker"
      }
    }
    wk-main-performance = {
      host        = "pmx-main"
      cpu_cores   = 6
      cpu_class   = "performance"
      memory      = 65536
      disk_size   = 96
      talos_role  = "worker"
      gpu         = true
      gpu_vram_gb = 16
      # Pascal P100 needs the proprietary/production-branch driver (open modules
      # are Turing+ only). 580 LTS variants match the known-good host driver.
      extensions = [
        "siderolabs/nonfree-kmod-nvidia-lts",
        "siderolabs/nvidia-container-toolkit-lts",
      ]
      hostpci = [
        {
          device = "nvidia-p100-x16"
          pcie   = true
          rombar = true
        }
      ]
      taints = [{
        key    = "dedicated"
        value  = "ai"
        effect = "NoSchedule"
      }]
      node_labels = {
        "node.kubernetes.io/instance-type" = "gpu-worker"
        "workload/ai-inference"            = "true"
      }
    }
    wk-infra = {
      host       = "pmx-infra"
      cpu_cores  = 2
      cpu_class  = "efficiency"
      memory     = 4096
      disk_size  = 32
      talos_role = "worker"
      node_labels = {
        "node.kubernetes.io/instance-type" = "infra-worker"
        "workload/infrastructure"          = "true"
      }
    }
  }

  validation {
    condition     = alltrue([for n in values(var.nodes) : contains(["controlplane", "worker"], n.talos_role)])
    error_message = "Each node's talos_role must be either \"controlplane\" or \"worker\"."
  }

  validation {
    condition     = length([for k, v in var.nodes : k if v.talos_role == "controlplane"]) > 0
    error_message = "At least one node must have talos_role \"controlplane\" — its static IP derives the cluster endpoint."
  }

  # Every node's host (or the default fallback) must exist in var.proxmox_nodes.
  validation {
    condition = alltrue([
      for n in values(var.nodes) :
      contains(keys(var.proxmox_nodes), coalesce(n.host, var.proxmox_node))
    ])
    error_message = "Each node's host (or the default proxmox_node) must be declared in var.proxmox_nodes."
  }

  # cpu_class and explicit cpu_affinity are mutually exclusive pinning modes.
  validation {
    condition     = alltrue([for n in values(var.nodes) : !(n.cpu_class != null && n.cpu_affinity != null)])
    error_message = "cpu_class and cpu_affinity are mutually exclusive: use cpu_class for class-based pinning, or cpu_affinity for an explicit thread set."
  }

  # The requested cpu_class must exist on the node's target host.
  validation {
    condition = alltrue([
      for n in values(var.nodes) : n.cpu_class == null ? true :
      contains(keys(var.proxmox_nodes), coalesce(n.host, var.proxmox_node)) ?
      contains(keys(var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].cpu_classes), n.cpu_class) : true
    ])
    error_message = "cpu_class must be one of the cpu_classes declared on the node's host (e.g. \"performance\" or \"efficiency\")."
  }

  # The class must be big enough for the node's vCPU count (net of reserved).
  validation {
    condition = alltrue([
      for n in values(var.nodes) : n.cpu_class == null ? true :
      !contains(keys(var.proxmox_nodes), coalesce(n.host, var.proxmox_node)) ? true :
      !contains(keys(var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].cpu_classes), n.cpu_class) ? true :
      (sum([for part in split(",", var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].cpu_classes[n.cpu_class]) :
        can(regex("-", part)) ? tonumber(split("-", part)[1]) - tonumber(split("-", part)[0]) + 1 : 1
      ]) - (var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].reserved.cpu.class == n.cpu_class ? var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].reserved.cpu.count : 0)) >= n.cpu_cores
    ])
    error_message = "The host's cpu_class range (minus reserved threads) is smaller than the node's cpu_cores."
  }

  # cpu_affinity syntax: comma-separated host core ids or ranges, e.g. "0-5" or "0,2,4".
  validation {
    condition = alltrue(flatten([
      for n in values(var.nodes) : n.cpu_affinity == null ? [true] : [
        for part in split(",", n.cpu_affinity) :
        can(regex("^[0-9]+$", part)) ||
        (can(regex("^[0-9]+-[0-9]+$", part)) && tonumber(split("-", part)[1]) >= tonumber(split("-", part)[0]))
      ]
    ]))
    error_message = "cpu_affinity must be a comma-separated list of host core ids or ranges, e.g. \"0-5\" or \"0,2,4\"."
  }

  # Fit check: the pinned host-core set must provide exactly one core per vCPU.
  validation {
    condition = alltrue([
      for n in values(var.nodes) : n.cpu_affinity == null ? true :
      sum([for part in split(",", n.cpu_affinity) :
        can(regex("-", part)) ? tonumber(split("-", part)[1]) - tonumber(split("-", part)[0]) + 1 : 1
      ]) == n.cpu_cores
    ])
    error_message = "cpu_affinity must span exactly cpu_cores host cores (e.g. cpu_cores = 6 with cpu_affinity = \"18-23\")."
  }

  # Fit check: pinned host core ids must exist on the target host.
  validation {
    condition = alltrue([
      for n in values(var.nodes) : n.cpu_affinity == null ? true :
      !contains(keys(var.proxmox_nodes), coalesce(n.host, var.proxmox_node)) ? true :
      var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].cpu_threads == null ? true :
      max([for part in split(",", n.cpu_affinity) :
        tonumber(can(regex("-", part)) ? split("-", part)[1] : part)
      ]) < var.proxmox_nodes[coalesce(n.host, var.proxmox_node)].cpu_threads
    ])
    error_message = "cpu_affinity references host core ids beyond the target host's cpu_threads."
  }

  validation {
    condition = alltrue([
      for host_name, host in var.proxmox_nodes : host.max_memory_gb == null ? true :
      sum([for n in values(var.nodes) : coalesce(n.host, var.proxmox_node) == host_name ? n.memory : 0]) <= (host.max_memory_gb - host.reserved.memory) * 1024
    ])
    error_message = "Total allocated memory per host exceeds the host's usable memory (max_memory_gb minus reserved.memory)."
  }

  # Per-class CPU capacity: nodes in a class must fit the class's threads minus
  # the threads reserved for the host OS in that class. Counts only nodes that
  # actually use the class (cpu_class set, no explicit cpu_affinity override).
  validation {
    condition = alltrue(flatten([
      for host_name, host in var.proxmox_nodes : [
        for class_name, spec in host.cpu_classes :
        sum([for n in values(var.nodes) :
          (coalesce(n.host, var.proxmox_node) == host_name && n.cpu_class == class_name && n.cpu_affinity == null) ? n.cpu_cores : 0
          ]) <= (sum([for part in split(",", spec) :
            can(regex("-", part)) ? tonumber(split("-", part)[1]) - tonumber(split("-", part)[0]) + 1 : 1
        ]) - (host.reserved.cpu.class == class_name ? host.reserved.cpu.count : 0))
      ]
    ]))
    error_message = "Nodes in a cpu_class exceed that class's usable threads (class threads minus reserved.cpu.count on that class)."
  }

  validation {
    condition = alltrue([
      for n in values(var.nodes) : n.gpu_vram_gb == 0 || n.gpu_vram_gb <= 0 ? true :
      n.gpu == true
    ])
    error_message = "gpu_vram_gb can only be set when gpu = true."
  }
}
