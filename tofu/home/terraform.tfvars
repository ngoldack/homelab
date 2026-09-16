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
    wk-main-efficiency  = "10.30.0.23"
    wk-main-performance = "10.30.0.22"
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
    # Host OS reservation (user policy): 2 GiB RAM floor of hard reserve +
    # the ZFS ARC cap (~1 GiB) + headroom => 4 GiB, and 2 efficiency threads
    # for the host itself (headless).
    #
    # This reserve used to be razor-thin, and it eventually bit: the fleet
    # allocating 92 of the 94 usable GiB stopped wk-main-performance booting
    # at all ("QEMU exited with code 1" = the host cannot hand back the
    # memory once ZFS ARC has grown into the free space). The consolidation
    # (two workers) sized the fleet at 86 GiB of the 90 allocatable, so the
    # host keeps ~8 GiB of REAL slack above this floor. Keep it that way:
    # check the fleet total against max_memory_gb before growing any node's
    # memory — an inflated ceiling here doesn't fail loudly, it just quietly
    # under-counts what the host needs for itself.
    reserved = {
      memory = 4
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
  # Control plane on E-cores (efficiency class). Efficiency usable =
  # 16 threads - 2 host-reserved = 14: cp-main(4) + wk-main-efficiency(10)
  # = 14, fully used, deliberately (the AR900i has no spare E-cores).
  cp-main = {
    host      = "pmx-main"
    vm_id     = 103
    cpu_cores = 4
    cpu_class = "efficiency"
    # 6 GiB: etcd + apiserver + scheduler/controller-manager + the Talos
    # runtime itself, with room for an apiserver burst. Raised from 4 in the
    # two-worker consolidation; fleet total stays inside the allocatable
    # budget (6 + 28 + 48 = 82 of 90).
    memory     = 6144
    disk_size  = 32
    talos_role = "controlplane"
    # Tailscale, control-plane only: an out-of-band admin path to the Talos
    # API (:50000) that works from anywhere without touching LAN or the
    # KubeSpan mesh. (Originally added for the GitHub Actions etcd-backup
    # job; that job is gone — backups are now an in-cluster CronJob — but
    # the admin path is kept. See README "Known limitations".)
    # See local.tailscale_config_patch in talos.tf for the TS_AUTHKEY wiring.
    extensions = ["siderolabs/tailscale"]
    # node_labels omitted: role, topology.zone, cpu model/cores and memory.gb
    # are derived automatically.
  }

  # THE general worker: carries everything that is neither nvidia-pinned nor
  # control-plane, and owns the Intel UHD 770 iGPU for QuickSync/VAAPI
  # (consolidated here from the retired wk-main-media node — exactly two
  # workers now). All remaining E-class threads: 16 - 2 host - 4 cp = 10.
  #
  # It is deliberately NOT NoSchedule-tainted: it is the only node general
  # workloads can run on, and a taint here would force a toleration onto
  # every deployment in the cluster while isolating nothing (a taint is only
  # meaningful when there is somewhere else to go). QuickSync workloads
  # PULL themselves here via the labels instead — hardware/igpu is derived
  # automatically from the hostpci mapping (see local.node_labels_derived in
  # main.tf), so the scheduling contract is: selector hardware/igpu + the
  # workload/media capability label, no toleration needed.
  wk-main-efficiency = {
    host      = "pmx-main"
    vm_id     = 104
    cpu_cores = 10
    # 28 GiB (was 32 at plan time): first boot of the consolidated fleet
    # OOM-killed the P100 VM's qemu mid-start (kernel global OOM, 82 GiB of
    # VM commits + qemu/VFIO overhead + PVE services against 94 physical —
    # the allocatable arithmetic must leave ~12 GiB of REAL slack, not just
    # the 4 GiB reserved floor). Still absorbs media's role and nets +4 over
    # the old split; demand concentration post-consolidation is HERE, but
    # the host budget binds first.
    memory    = 28672
    disk_size = 48

    talos_role = "worker"

    # i915 (kernel driver + firmware) for the passed-through UHD 770 — moved
    # with the iGPU from wk-main-media. The VAAPI userspace (libva,
    # intel-media-driver) belongs in whichever CONTAINER transcodes, mounted
    # with /dev/dri; Talos has no host-side VAAPI extension (the old
    # siderolabs/intel-vaapi was removed from the catalog entirely).
    extensions = [
      "siderolabs/i915",
    ]

    # The VGA-arbitration history matters here: an iGPU passthrough guest
    # with an emulated display present can hang at boot (this bit
    # wk-main-media exactly once). main.tf's vga rule — serial0 whenever
    # hostpci is set — is what keeps this safe; do not re-add `std` to
    # passthrough nodes.
    hostpci = [
      {
        device = "intel-igpu"
        pcie   = true
        rombar = true
      },
    ]

    node_labels = {
      "node.kubernetes.io/instance-type" = "worker"
      "workload/media"                   = "true"
    }
  }

  # AI/inference worker — the x16 P100 and, since the 2026-09-16 sandbox
  # merge, all 16 P-class threads and 48 GiB (the pre-carve shape restored;
  # Kata sandbox workloads also land here, label-pinned).
  #
  # 48 GiB, not 64: the VM stopped starting at 64 ("QEMU exited with code 1"
  # = allocation failure — see the host reserved block above).
  # Kept in sync with the live VM, which was resized by hand first: without
  # this line the next apply would push it straight back to the old size and
  # break the node again.
  # Fleet total: 6 (cp) + 28 (eff) + 48 (perf) = 82 GiB committed, ~12 GiB
  # real host slack.
  wk-main-performance = {
    host      = "pmx-main"
    vm_id     = 105
    cpu_cores = 16
    cpu_class = "performance"

    memory     = 49152
    disk_size  = 96
    talos_role = "worker"
    gpu        = true
    # NVIDIA driver + container toolkit for the P100. Pascal (cc 6.0) needs the
    # proprietary/production-branch driver — the open kernel modules support
    # Turing+ (cc 7.5+) only. Known-good host driver is 580.159.04 (CUDA 13.0),
    # so pin the 580 LTS extension variants, not the newer `production` (595).
    # These merge with the cluster-wide nfs-utils/nvme-cli/qemu-guest-agent
    # defaults. No siderolabs/i915 here — the iGPU lives on the efficiency
    # worker, which also keeps this node's boot image smaller and any i915
    # regression away from inference.
    extensions = [
      "siderolabs/nonfree-kmod-nvidia-lts",
      "siderolabs/nvidia-container-toolkit-lts",
      # Kata Containers — containerd runtime + QEMU microVM machinery, baked
      # into the boot image via Image Factory (merged from the old dedicated
      # sandbox worker on 2026-09-16; this node's extension list forms its own
      # schematic/ISO in main.tf).
      "siderolabs/kata-containers",
    ]
    hostpci = [
      {
        device = "nvidia-p100-x16"
        pcie   = true
        rombar = true
      }
    ]
    # GPU workload placement is by label + nvidia RuntimeClass (the old
    # dedicated=nvidia taint was deleted live 2026-09-15; no taint applies).
    # Sandbox workloads pin via workload.hermes.io/sandbox=true only —
    # non-sandbox pods also schedule here.
    node_labels = {
      "node.kubernetes.io/instance-type" = "gpu-worker"
      "workload/ai-inference"            = "true"
      "workload.hermes.io/sandbox"       = "true"
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
