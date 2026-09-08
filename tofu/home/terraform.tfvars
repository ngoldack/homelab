# Minimal v1 bootstrap: everything runs on pmx-main.
# Node names MUST match the keys of network.node_ips below so static IPs resolve
# instead of falling back to DHCP.
proxmox_node         = "pmx-main"
proxmox_storage_pool = "local-zfs"
cluster_name         = "home-talos"

# Public cluster network configuration. Credentials and state encryption remain
# in secret.sops.yaml; this topology is intentionally plaintext configuration.
network = {
  vlan_id       = 3000
  subnet_prefix = 24
  gateway       = "10.30.0.1"
  nameservers   = ["10.30.0.1", "1.1.1.1"]
  node_ips = {
    cp                  = "10.30.0.10"
    wk-main-efficiency  = "10.30.0.21"
    wk-main-performance = "10.30.0.22"
    wk-infra            = "10.30.0.23"
  }
}

# Cluster-internal networks (never overlap the home VLANs)
pod_cidr     = "172.20.0.0/16"
service_cidr = "172.21.0.0/16"

# Independent Proxmox hosts — no shared cluster API endpoint.
# Each host declares its own endpoint, storage pool, ISO datastore and bridge.
# pmx-main carries one Tesla P100 in the x16 slot.
proxmox_nodes = {
  pmx-main = {
    endpoint      = "https://10.20.10.21:8006/"
    storage_pool  = "local-zfs"
    iso_datastore = "local"
    bridge        = "vmbr0"
    max_memory_gb = 96
    max_cpu_cores = 24
    cpu_threads   = 32
    cpu_model     = "i9-13900HX"
    # Minimal host reserve: 2 GiB RAM + 2 efficiency threads for the host OS
    # (headless, ARC capped on-host).
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
      },
    ]
  }
  pmx-infra = {
    endpoint      = "https://10.20.10.22:8006/"
    storage_pool  = "local-zfs"
    iso_datastore = "local"
    bridge        = "vmbr0"
    max_memory_gb = 32
    max_cpu_cores = 4
    cpu_threads   = 8
    cpu_model     = "Xeon E3-1230 v2"
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

nodes = {
  # Control plane on E-cores (efficiency class). Efficiency usable = 16-2 = 14.
  cp = {
    host       = "pmx-main"
    cpu_cores  = 4
    cpu_class  = "efficiency"
    memory     = 4096
    disk_size  = 32
    talos_role = "controlplane"
    # node_labels omitted: role, topology.zone, cpu model/cores and memory.gb
    # are derived automatically.
  }

  # Default worker. Shares E-cores with cp.
  # cp(4) + wk-main-efficiency(10) = 14 efficiency threads = fully used.
  wk-main-efficiency = {
    host      = "pmx-main"
    cpu_cores = 10
    cpu_class = "efficiency"
    memory    = 24576
    disk_size = 48

    talos_role = "worker"

    node_labels = {
      "node.kubernetes.io/instance-type" = "worker"
      "workload/media"                   = "true"
    }
  }

  # Dedicated performance worker — 64GB RAM for large models plus runtime
  # overhead, the x16 P100 for inference, and the Intel iGPU for media
  # transcoding. Pinned to all 8 P-cores (16 threads).
  wk-main-performance = {
    host        = "pmx-main"
    cpu_cores   = 16
    cpu_class   = "performance"
    memory      = 65536
    disk_size   = 96
    talos_role  = "worker"
    gpu         = true
    gpu_vram_gb = 16
    # NVIDIA driver + container toolkit for the P100. Pascal (cc 6.0) needs the
    # proprietary/production-branch driver — the open kernel modules support
    # Turing+ (cc 7.5+) only. Known-good host driver is 580.159.04 (CUDA 13.0),
    # so pin the 580 LTS extension variants, not the newer `production` (595).
    # These merge with the cluster-wide nfs-utils/nvme-cli defaults.
    extensions = [
      "siderolabs/i915",
      "siderolabs/intel-vaapi",
      "siderolabs/nonfree-kmod-nvidia-lts",
      "siderolabs/nvidia-container-toolkit-lts",
    ]
    hostpci = [
      {
        device = "intel-igpu"
        pcie   = true
        rombar = true
      },
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
      "workload/media"                   = "true"
    }
  }

  # Dedicated infrastructure worker for cluster management and auxiliary services.
  wk-infra = {
    host       = "pmx-infra"
    cpu_cores  = 4
    cpu_class  = "efficiency"
    memory     = 8192
    disk_size  = 32
    talos_role = "worker"
    node_labels = {
      "node.kubernetes.io/instance-type" = "infra-worker"
      "workload/infrastructure"          = "true"
    }
  }
}

