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
    cp-main             = "10.30.0.10"
    wk-main-efficiency  = "10.30.0.21"
    wk-main-performance = "10.30.0.22"
    wk-main-media       = "10.30.0.23"
  }
}

# Cluster-internal networks (never overlap the home VLANs)
pod_cidr     = "172.20.0.0/16"
service_cidr = "172.21.0.0/16"

# A single Proxmox host. pmx-infra was removed: everything (control plane
# included) runs on pmx-main.
# pmx-main carries one Tesla P100 in the x16 slot.
proxmox_nodes = {
  pmx-main = {
    endpoint      = "https://10.20.10.21:8006/"
    storage_pool  = "local-zfs"
    iso_datastore = "local"
    bridge        = "vmbr0"
    # 94, not the 96 nameplate: the host's real MemTotal is 96273 MiB (≈94.02
    # GiB after firmware/DIMM overhead), confirmed live via the Proxmox API.
    # The capacity validation below is a hard <= check, so an inflated ceiling
    # here doesn't fail loudly — it just quietly under-counts what the host
    # actually needs to keep for itself.
    max_memory_gb = 94
    max_cpu_cores = 24
    cpu_threads   = 32
    cpu_model     = "i9-13900HX"
    # Minimal host reserve: 2 GiB RAM + 2 efficiency threads for the host OS
    # (headless, ARC capped on-host).
    #
    # This reserve used to be razor-thin, and it eventually bit. With the fleet
    # allocating 92 of the 94 usable GiB, wk-main-performance stopped starting
    # at all: Proxmox failed the task with "QEMU exited with code 1", which is
    # what a host unable to hand back the requested memory looks like once ZFS
    # ARC has grown into the free space.
    #
    # The follow-up this comment used to defer has now happened —
    # wk-main-performance went from 64 to 48 GiB — so the fleet totals 76 GiB
    # and roughly 18 GiB is genuinely free for the host OS and ARC. These 2 GiB
    # are now a floor with real slack above them, rather than the only thing
    # between the host and an OOM. Keep it that way: check the fleet total
    # against max_memory_gb before growing any node's memory.
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
}

nodes = {
  # Control plane on E-cores (efficiency class). Efficiency usable = 16-2 = 14.
  cp-main = {
    host       = "pmx-main"
    vm_id      = 103
    cpu_cores  = 4
    cpu_class  = "efficiency"
    memory     = 4096
    disk_size  = 32
    talos_role = "controlplane"
    # Tailscale, control-plane only: lets the etcd-backup GitHub Actions job
    # reach this node's Talos API (:50000) from a GitHub-hosted runner over
    # the tailnet, instead of needing a self-hosted runner with LAN access.
    # See local.tailscale_config_patch in talos.tf for the TS_AUTHKEY wiring.
    extensions = ["siderolabs/tailscale"]
    # node_labels omitted: role, topology.zone, cpu model/cores and memory.gb
    # are derived automatically.
  }

  # General-purpose worker. Shares E-cores with cp-main and wk-main-media.
  # cp-main(4) + wk-main-efficiency(6) + wk-main-media(4) = 14 = fully used.
  # Shrunk from 10 threads / 24 GiB to make room for wk-main-media: the host
  # was already at 100% thread allocation, so a fourth VM had to be carved out
  # of somewhere, and the AI worker's P-cores and 64 GiB are the whole point of
  # that node. No longer carries workload/media — that moved with the iGPU.
  wk-main-efficiency = {
    host      = "pmx-main"
    vm_id     = 104
    cpu_cores = 6
    cpu_class = "efficiency"
    memory    = 16384
    disk_size = 48

    talos_role = "worker"

    node_labels = {
      "node.kubernetes.io/instance-type" = "worker"
    }
  }

  # Dedicated media/transcode worker — owns the Intel UHD 770 iGPU for
  # QuickSync. Deliberately small: a hardware transcode runs almost entirely in
  # the iGPU's fixed-function block, so the vCPUs only feed it and demux/mux.
  # E-cores are the right class for exactly that reason; the P-cores stay with
  # the AI worker.
  wk-main-media = {
    host      = "pmx-main"
    vm_id     = 106
    cpu_cores = 4
    cpu_class = "efficiency"
    memory    = 8192
    # Headroom for transcode scratch space, matching the general worker.
    disk_size = 48

    talos_role = "worker"

    # i915 only — the kernel driver + firmware for the iGPU. The VAAPI
    # userspace libraries (libva, intel-media-driver) belong in whichever
    # container does the transcoding, together with a /dev/dri passthrough;
    # there is no host-side VAAPI extension for Talos (siderolabs/intel-vaapi
    # was removed from the catalog entirely). Merges with the cluster-wide
    # nfs-utils / nvme-cli / qemu-guest-agent defaults.
    extensions = [
      "siderolabs/i915",
    ]

    hostpci = [
      {
        device = "intel-igpu"
        pcie   = true
        rombar = true
      },
    ]

    # Keep generic workloads off the only node with QuickSync. Media pods
    # tolerate dedicated=media (applied by a Flux Job, not Talos — see
    # kubernetes/infrastructure/home/node-taints/); this
    # node.kubernetes.io/instance-type label is what that Job selects on.
    node_labels = {
      "node.kubernetes.io/instance-type" = "media-worker"
      "workload/media"                   = "true"
    }
  }

  # Dedicated AI/inference worker — 48GB RAM for models plus runtime overhead,
  # and the x16 P100. Pinned to all 8 P-cores (16 threads).
  # The Intel iGPU moved to wk-main-media: this node is single-purpose now, so
  # inference cannot starve a transcode (or vice versa) and either capability
  # can be rebooted without taking the other down.
  wk-main-performance = {
    host      = "pmx-main"
    vm_id     = 105
    cpu_cores = 16
    cpu_class = "performance"
    # 48 GiB, down from 64. The VM stopped starting at 64: Proxmox failed the
    # task with "QEMU exited with code 1", which is what a failure to allocate
    # looks like — the host has 94 GiB usable and the fleet was asking for 92
    # of it, leaving nothing for the host OS once ZFS ARC had grown. At 48 the
    # fleet totals 76 GiB (4 + 16 + 8 + 48), leaving ~18 GiB of real headroom.
    # Kept in sync with the live VM, which was resized by hand first: without
    # this line the next apply would push it straight back to 64 and break the
    # node again.
    memory      = 49152
    disk_size   = 96
    talos_role  = "worker"
    gpu         = true
    gpu_vram_gb = 16
    # NVIDIA driver + container toolkit for the P100. Pascal (cc 6.0) needs the
    # proprietary/production-branch driver — the open kernel modules support
    # Turing+ (cc 7.5+) only. Known-good host driver is 580.159.04 (CUDA 13.0),
    # so pin the 580 LTS extension variants, not the newer `production` (595).
    # These merge with the cluster-wide nfs-utils/nvme-cli/qemu-guest-agent
    # defaults. No siderolabs/i915 here any more — it went to wk-main-media
    # along with the iGPU, which also makes this node's boot image
    # meaningfully smaller and keeps an i915 regression away from inference.
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
    # Tainted dedicated=ai (applied by a Flux Job, not Talos — see
    # kubernetes/infrastructure/home/node-taints/), not dedicated=gpu: this
    # node serves one role now that the iGPU moved to wk-main-media, so the
    # role-named taint is the clearer contract. This
    # node.kubernetes.io/instance-type label is what that Job selects on.
    node_labels = {
      "node.kubernetes.io/instance-type" = "gpu-worker"
      "workload/ai-inference"            = "true"
    }
  }
}


# Public ingress on Hetzner Cloud. An ordinary WORKER of this same cluster —
# there is no second cluster and no ClusterMesh. It joins over Talos KubeSpan,
# so the cluster endpoint stays the LAN address and nothing is forwarded on the
# home router.
#
# CAX11 (2 vCPU Ampere, 4 GB, ARM64) is the smallest Hetzner instance and is
# generously sized for what this node does: terminate TLS and proxy. All real
# workloads stay on the LAN nodes — this one carries a
# dedicated=ingress:NoSchedule taint applied at registration.
cloud_nodes = {
  ingress-fsn1 = {
    server_type = "cax11"
    location    = "fsn1"
    arch        = "arm64"
    # node.homelab/role=ingress and topology.homelab/site=cloud are set
    # unconditionally in ingress.tf — the first is what Cilium's Gateway API
    # host-network selector matches (it fails OPEN, so it must be exact), the
    # second is what keeps truenas-csi's everything-tolerating node DaemonSet
    # off this host.
    node_labels = {
      "node.kubernetes.io/instance-type" = "ingress"
    }
  }
}
