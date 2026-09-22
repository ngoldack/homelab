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
# included) runs on pmx-main. Its Tesla P100 was handed back on 2026-09-22
# (local-LLM lane removed), so the host's `gpu` inventory is empty.
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
    # Host OS reservation (user policy): 6 GiB floor total for the host —
    # Proxmox + kernel + PVE services + a SHRUNK ZFS ARC. Everything else goes
    # to the VMs. ZFS ARC is capped low (zfs_arc_max) so it cannot grow into
    # VM memory; keep that cap bound so the host stays within this floor.
    #
    # History: the reserve and ARC used to be razor-thin, and it bit. The fleet
    # allocating 92 of 94 usable GiB stopped wk-main-performance booting
    # ("QEMU exited with code 1"), and even at 84 GiB the host sat ~0 free on
    # 2026-09-20, so the kernel OOM killer reaped VM qemu processes (104/105;
    # cp-main 103 is the fatal victim). The fix is NOT to starve the VMs — it
    # is to hold ZFS ARC within this 6 GiB floor so the VMs (dedicated, no
    # balloon) can own the remaining 88 GiB. Check the fleet total against
    # max_memory_gb before growing any node, and keep the ARC cap in force.
    reserved = {
      memory = 6
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
    # 8 GiB: etcd + apiserver + scheduler/controller-manager + the Talos
    # runtime itself, with room for an apiserver burst. Raised from 4 to 6 in
    # the two-worker consolidation, then to 8 on 2026-09-18 after the node hit
    # memory pressure: with no swap, Talos's OOM controller started SIGKILLing
    # besteffort pods in a tight loop, which starved the control plane
    # (apiserver TLS handshake timeouts, controller-manager and scheduler
    # not ready). Fleet totals 8 + 32 + 48 = 88 GiB = 94 usable - 6 host floor
    # (enforced by fleet_memory_within_host_ceiling in main.tf; ARC shrunk).
    memory     = 8192
    disk_size  = 32
    talos_role = "controlplane"
    # No extensions: the siderolabs/tailscale extension was removed on
    # 2026-09-18. It had been kept for an out-of-band admin path to the Talos
    # API, but the GitHub-hosted etcd-backup job that needed the tailnet was
    # already retired in favour of the in-cluster talos-backup CronJob, and
    # the extension sat in ext-tailscale's restart-forever loop on this node
    # without ever authenticating. Dropping it also shrinks the boot image.
    extensions = []
    # node_labels omitted: role, topology.zone, cpu model/cores and memory.gb
    # are derived automatically.
  }

  # THE general worker: carries every workload that is not pinned to the
  # performance worker or the control plane
  # (consolidated here from the retired wk-main-media node — exactly two
  # workers now). All remaining E-class threads: 16 - 2 host - 4 cp = 10.
  #
  # It is deliberately NOT NoSchedule-tainted: it is the only node general
  # workloads can run on, and a taint here would force a toleration onto
  # every deployment in the cluster while isolating nothing (a taint is only
  # meaningful when there is somewhere else to go).
  #
  # node.homelab/class=efficiency is the intended landing spot for ordinary
  # workloads; the performance worker is reached by opt-in (its own class
  # value, or the capability selectors Kata and BuildKit already use). The
  # class is a separate key rather than a new value of topology.homelab/site
  # because ~55 manifests select site=home and BOTH workers must keep
  # matching it — the site label says which site a node is in, the class
  # label says how it wants to be used.
  #
  # The iGPU and the workload/media capability label moved to the performance
  # worker on 2026-09-22 (see that block): jellyfin, the only pod that mounts
  # /dev/dri, must live on whichever node owns the card.
  wk-main-efficiency = {
    host      = "pmx-main"
    vm_id     = 104
    cpu_cores = 10
    # 32 GiB: the general worker carries everything that is not
    # performance-pinned or control-plane, plus the media role and the iGPU.
    # Sized to fill the VM pool (88 GiB) under
    # the 6 GiB host floor: ballooning is off (memory.dedicated), so the only
    # cap that matters is allocated <= (max_memory_gb - reserved.memory) — ARC
    # is shrunk so ZFS cannot grow into this. Raised back to 32 on 2026-09-20
    # after an earlier OOM-driven trim to 24 proved unnecessary once the ARC cap
    # (not VM size) was the true pressure valve.
    memory    = 32768
    disk_size = 48

    talos_role = "worker"

    # No extensions: this node carries no passthrough device. The i915 driver
    # and firmware moved to wk-main-performance with the UHD 770, and the
    # VAAPI userspace (libva, intel-media-driver) lives in whichever CONTAINER
    # transcodes, mounted with /dev/dri; Talos has no host-side VAAPI
    # extension (the old siderolabs/intel-vaapi was removed from the catalog
    # entirely).

    node_labels = {
      "node.kubernetes.io/instance-type" = "worker"
      "node.homelab/class"               = "efficiency"
    }
  }

  # Performance worker — all 16 P-class threads and 48 GiB (the shape it kept
  # when the 2026-09-16 sandbox merge folded the dedicated sandbox worker in).
  # Kata sandbox workloads land here (label-pinned, and the kata RuntimeClass
  # itself carries that nodeSelector) and so does the rootless BuildKit
  # builder, which is why this node carries the user-namespace sysctl below.
  # It hosted the Tesla P100 until 2026-09-22; the local-LLM lane went with
  # it, and the Intel UHD 770 iGPU passthrough arrived the same day.
  #
  # node.homelab/class=performance marks this node as opt-in: it is NOT the
  # default landing spot (that is the efficiency worker), so a workload ends
  # up here either because it selects a capability only this node has (Kata's
  # workload.hermes.io/sandbox, BuildKit's instance-type, the iGPU) or
  # because the efficiency node could not fit it.
  #
  # 48 GiB — at the top of the 88 GiB VM pool (94 usable - 6 host). Ballooning
  # is off (memory.dedicated), so allocated == committed; the ZFS ARC is shrunk
  # so it never grows into this budget.
  # 48 is the proven ceiling for this VM: it would not start at 64 ("QEMU
  # exited with code 1" = allocation failure). A hand-resize on the live VM
  # must precede any tfvars change so the next apply doesn't revert the size.
  # Fleet total: 8 (cp) + 32 (eff) + 48 (perf) = 88 GiB = 94 usable - 6 host.
  wk-main-performance = {
    host      = "pmx-main"
    vm_id     = 105
    cpu_cores = 16
    cpu_class = "performance"

    memory     = 49152
    disk_size  = 96
    talos_role = "worker"
    # Rootless buildkitd refuses to start with Talos's hardened default
    # (user.max_user_namespaces=0); the builder lives on this node, so the
    # sysctl patch in talos.tf is keyed on this flag.
    rootless_buildkit = true
    # Kata Containers — containerd runtime + QEMU microVM machinery — plus
    # i915 (kernel driver + firmware) for the passed-through UHD 770. Both are
    # baked into the boot image via Image Factory (this node's extension list
    # forms its own schematic/ISO in main.tf).
    #
    # OPERATIONAL CONSEQUENCE, and the reason this is a maintenance-window
    # change: `install.image` is keyed on the extension set, so a node that
    # has never carried kata+i915 together gets a brand-new Image Factory
    # schematic, and the swap only lands through a node REINSTALL (main.tf
    # documents the schematic build and its download-timeout recovery).
    extensions = [
      "siderolabs/kata-containers",
      "siderolabs/i915",
    ]

    # The UHD 770, moved here from the efficiency worker on 2026-09-22.
    # intel-igpu is a host-level PCI resource mapping on pmx-main and a
    # passthrough device can only be claimed by one guest at a time, so the
    # two nodes must be changed in separate applies: remove it from the
    # efficiency worker and let that VM stop so vfio releases the device,
    # then add it here. A single apply writes both configs with whatever
    # ordering the provider's parallelism gives it.
    #
    # The VGA-arbitration history applies here now: an iGPU passthrough guest
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

    # Sandbox workloads pin via workload.hermes.io/sandbox=true. The
    # `instance-type` value is historical — the node has no NVIDIA GPU any
    # more — and is kept because the BuildKit builder selects on it
    # (kubernetes/infrastructure/home/buildkit/helmrelease.yaml); changing the
    # value would strand the builder until the node restart that applies it.
    #
    # workload/media is the iGPU's other half: jellyfin selects it and is the
    # only pod in the cluster that mounts /dev/dri (media/helmrelease.yaml),
    # so the capability label travels with the card. immich-machine-learning
    # needs nothing here — it selects the derived hardware/igpu label (from
    # the hostpci mapping above) and follows automatically.
    #
    # STALE LABELS to remove by hand after the next apply, because Talos's
    # nodeLabels patch does not prune keys that are no longer declared:
    # `ai=true` and `workload/ai-inference=true` (declared in no repo file,
    # so they are hand-applied leftovers) and `hardware/gpu.model=Tesla-P100`
    # / `hardware/gpu.count` / `hardware/gpu.vram_gb` (derived only when a
    # node sets `gpu = true`, which no node does any more):
    #   kubectl label node talos-919-w9u ai- workload/ai-inference- \
    #     hardware/gpu.model- hardware/gpu.count- hardware/gpu.vram_gb-
    node_labels = {
      "node.kubernetes.io/instance-type" = "gpu-worker"
      "workload.hermes.io/sandbox"       = "true"
      "workload/media"                   = "true"
      "node.homelab/class"               = "performance"
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
