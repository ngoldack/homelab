locals {
  network_ipv4_cidr = "172.30.0.0/16"
  node_ipv4_cidr    = "172.30.1.0/24"
  pod_ipv4_cidr     = "172.30.16.0/20"
  service_ipv4_cidr = "172.30.8.0/21"

  # Tailscale is delivered as a Talos ExtensionServiceConfig patch. The module
  # only auto-applies this patch to control planes (see its talos.tf); pass
  # the same patch to workers explicitly so every node joins the tailnet, not
  # just the control plane.
  tailscale_config_patch = yamlencode({
    apiVersion = "v1alpha1"
    kind       = "ExtensionServiceConfig"
    name       = "tailscale"
    environment = [
      "TS_AUTHKEY=${local.secrets["tailscale_auth_key"]}",
    ]
  })
}

resource "talos_image_factory_schematic" "arm64" {
  schematic = yamlencode({
    customization = {
      systemExtensions = {
        officialExtensions = ["siderolabs/tailscale"]
      }
    }
  })
}

data "talos_image_factory_urls" "hcloud_arm64" {
  talos_version = var.talos_version
  schematic_id  = talos_image_factory_schematic.arm64.id
  platform      = "hcloud"
  architecture  = "arm64"
}

resource "imager_image" "talos_arm" {
  image_url    = data.talos_image_factory_urls.hcloud_arm64.urls.disk_image
  architecture = "arm"
  description  = "Talos Linux ${var.talos_version} ARM64 for ${var.cluster_name}"

  labels = {
    "cluster" = var.cluster_name
    "os"      = "talos"
    "version" = var.talos_version
  }
}

module "talos" {
  source  = "hcloud-talos/talos/hcloud"
  version = "3.4.15"

  hcloud_token       = local.secrets["hcloud_api_token"]
  cluster_name       = var.cluster_name
  cluster_domain     = var.cluster_domain
  cluster_prefix     = true
  location_name      = var.location
  talos_version      = var.talos_version
  kubernetes_version = var.kubernetes_version

  # disable_arm looks redundant with talos_image_id_arm (both make the
  # module skip its "os=talos" ARM image lookup, see server.tf) but is NOT:
  # on a from-scratch apply, imager_image.talos_arm.id is unknown until
  # apply, so `talos_image_id_arm != null` alone can't resolve
  # data.hcloud_image.arm's `count` at plan time ("Invalid count argument" —
  # confirmed by actually running a clean apply). disable_arm's plain
  # boolean literal is what keeps that count statically known on a first
  # apply; once imager_image.talos_arm exists, either one alone would do.
  talos_image_id_arm = tostring(imager_image.talos_arm.id)
  disable_arm        = true
  disable_x86        = true

  talos_worker_extra_config_patches = [local.tailscale_config_patch]

  control_plane_nodes = [
    {
      id   = 1
      type = var.server_type
      labels = {
        # Not topology.kubernetes.io/zone: hcloud-cloud-controller-manager
        # owns that key and silently overwrites it with the Hetzner
        # datacenter (e.g. "fsn1-dc8") — confirmed live via managedFields.
        # The custom key matches what tofu/home now sets (topology.homelab/site
        # = "home"), so "which site is this node in" resolves the same way on
        # both clusters instead of at two different label keys.
        "topology.homelab/site"   = "cloud"
        "workload/public-ingress" = "true"
      }
    },
  ]
  control_plane_allow_schedule = true

  worker_nodes = [
    {
      id   = 1
      type = var.server_type
      labels = {
        # See control_plane_nodes above for why this isn't
        # topology.kubernetes.io/zone.
        "topology.homelab/site" = "cloud"
      }
    },
  ]

  network_ipv4_cidr = local.network_ipv4_cidr
  node_ipv4_cidr    = local.node_ipv4_cidr
  pod_ipv4_cidr     = local.pod_ipv4_cidr
  service_ipv4_cidr = local.service_ipv4_cidr

  # Kube API stays reachable from anywhere (protected by TLS + client-cert
  # auth, the normal posture for a public homelab edge). The Talos API
  # (raw node control — no equivalent auth layering) is instead restricted to
  # whichever machine runs `tofu apply`: firewall_talos_api_source is left
  # unset so it falls back to firewall_use_current_ip's live lookup. Re-apply
  # from a new location to update the allowed IP.
  firewall_kube_api_source = ["0.0.0.0/0"]
  firewall_use_current_ip  = true
  extra_firewall_rules = [
    {
      description = "Allow public HTTP"
      direction   = "in"
      protocol    = "tcp"
      port        = "80"
      source_ips  = ["0.0.0.0/0"]
    },
    {
      description = "Allow public HTTPS"
      direction   = "in"
      protocol    = "tcp"
      port        = "443"
      source_ips  = ["0.0.0.0/0"]
    },
    {
      description = "Allow cluster-internal TCP"
      direction   = "in"
      protocol    = "tcp"
      port        = "any"
      source_ips  = [local.network_ipv4_cidr]
    },
    {
      description = "Allow cluster-internal UDP"
      direction   = "in"
      protocol    = "udp"
      port        = "any"
      source_ips  = [local.network_ipv4_cidr]
    },
    {
      # port = "" (empty string), not null and not omitted: extra_firewall_rules
      # is list(any), so every element must share the same attribute set or
      # OpenTofu's type unification fails at plan time (confirmed: omitting the
      # key errors "all list elements must have the same type"). null instead
      # of "" fails differently and later: the module's own firewall.tf builds
      # a dedup key via format("%s-%s-%s", direction, protocol, port), and
      # format()'s %s verb rejects an actual null outright ("unsupported value
      # for %s: null value cannot be formatted") — confirmed at `tofu plan`,
      # past what `validate` catches. "" satisfies both: a real string keeps
      # format() happy, and empty is functionally "no port" same as null once
      # it reaches the actual hcloud_firewall resource.
      description = "Allow cluster-internal ICMP"
      direction   = "in"
      protocol    = "icmp"
      port        = ""
      source_ips  = [local.network_ipv4_cidr]
    },
  ]

  # The module owns Cilium and the HCloud CCM permanently — both stay
  # deploy_*=true forever, applied as apply_only kubectl manifests (never
  # deleted by tofu, never reconciled by Flux). This is deliberate, not a
  # bootstrap-then-handoff step: the manifests carry no Helm ownership
  # metadata, so a Flux HelmRelease over the same objects would fail its own
  # install with an ownership conflict. To upgrade Cilium or the CCM, bump
  # cilium_version/hcloud_ccm_version (or cilium_values) here and re-apply —
  # same as any other tofu-managed resource. Flux still manages every other
  # cloud workload; it never touches kube-system's CNI/CCM.
  deploy_cilium  = true
  cilium_version = "1.19.5"
  cilium_values = [yamlencode({
    cluster = {
      name = "cloud"
      id   = 2
    }
    ipam = {
      mode = "kubernetes"
    }
    kubeProxyReplacement = true
    k8sServiceHost       = "127.0.0.1"
    k8sServicePort       = 7445
    bpf = {
      masquerade = true
    }
    loadBalancer = {
      acceleration = "best-effort"
    }
    routingMode    = "tunnel"
    tunnelProtocol = "vxlan"
    securityContext = {
      capabilities = {
        ciliumAgent      = ["CHOWN", "KILL", "NET_ADMIN", "NET_RAW", "IPC_LOCK", "SYS_ADMIN", "SYS_RESOURCE", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"]
        cleanCiliumState = ["NET_ADMIN", "SYS_ADMIN", "SYS_RESOURCE"]
      }
    }
    cgroup = {
      autoMount = {
        enabled = false
      }
      hostRoot = "/sys/fs/cgroup"
    }
    gatewayAPI = {
      enabled = true
    }
    # Explicitly off, matching the module's own default. Supplying cilium_values
    # at all replaces that default wholesale, and the chart's own default for
    # hubble is ENABLED — so omitting this silently turns Hubble on. That
    # matters because the module renders Cilium client-side with
    # `data "helm_template"`, where hubble's genSelfSignedCert regenerates the
    # CA and server certificate on every single render, so an untouched cluster
    # still plans cert churn every time. Nothing here consumes Hubble — there is
    # no relay and no UI deployed — so turning it off drops that noise and the
    # three unused objects (cilium-ca, hubble-server-certs, hubble-peer).
    #
    # Note this does NOT make `tofu plan` clean for this root. The module
    # declares alias_ips = [] on the control-plane server (server.tf, citing
    # hetznercloud/terraform-provider-hcloud#650) while Talos claims the
    # control-plane VIP 172.30.1.100 on that same interface at runtime via the
    # Hetzner API. tofu therefore reports a permanent in-place diff wanting to
    # strip the alias. Applying it removes the VIP until Talos re-claims it, so
    # treat any cloud apply as briefly disrupting the internal control-plane
    # endpoint — see "Known limitations" in the README.
    hubble = {
      enabled = false
    }
  })]
  # Pinned rather than left null (which resolves to the chart's "latest" at
  # every apply, drifting silently). Bump deliberately, same as cilium_version.
  deploy_hcloud_ccm  = true
  hcloud_ccm_version = "1.36.0"
  tailscale = {
    enabled  = true
    auth_key = local.secrets["tailscale_auth_key"]
  }
}

resource "random_id" "cloud_talos_backup_bucket" {
  byte_length = 4
}

resource "aws_s3_bucket" "cloud_talos_backups" {
  bucket        = "${var.cluster_name}-etcd-${random_id.cloud_talos_backup_bucket.hex}"
  force_destroy = false

  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "cloud_talos_backups" {
  bucket = aws_s3_bucket.cloud_talos_backups.id
  versioning_configuration {
    status = "Enabled"
  }
}

# NOT managed here: aws_s3_bucket_lifecycle_configuration against Hetzner
# Object Storage never converges with the AWS provider (tested live —
# every apply times out after 3m on the post-write verification GET,
# "context deadline exceeded"). This is a confirmed, currently-unfixed
# upstream bug: terraform-provider-aws#49019 / the still-open fix PR #49547
# — the resource's read-back verification depends on an AWS-only response
# header (x-amz-transition-default-minimum-object-size) that Hetzner's
# S3-compatible API doesn't send, so the provider can never confirm success
# even when the underlying write did go through.
#
# Snapshots WILL accumulate forever without a lifecycle policy (one object
# per backup-workflow run, every day, indefinitely). Verify whether the
# above attempt actually left a policy on the bucket
# (aws s3api get-bucket-lifecycle-configuration --endpoint-url
# https://fsn1.your-objectstorage.com --bucket <name>, or the Hetzner
# Console), and if not, set one manually there, or via the Hetzner Console,
# until the upstream fix ships:
#   rule: expire objects after 90 days; expire noncurrent versions after 30 days

output "cloud_talos_backup_bucket" {
  description = "Hetzner Object Storage bucket for Cloud Talos etcd snapshots"
  value       = aws_s3_bucket.cloud_talos_backups.bucket
}

output "talos_backup_s3_endpoint" {
  description = "S3 endpoint used by the Talos etcd backup workflow"
  value       = "https://${var.object_storage_location}.your-objectstorage.com"
}

output "cloud_ingress_ipv4" {
  description = "Public IPv4 address of the cloud control plane"
  value       = module.talos.public_ipv4_list[0]
}

output "cloud_ingress_ipv6" {
  description = "The cloud cluster is IPv4-only; retained for output compatibility"
  value       = null
}

output "cloud_cluster_kubeconfig" {
  description = "Kubeconfig for the cloud cluster"
  value       = module.talos.kubeconfig
  sensitive   = true
}

output "cloud_cluster_talosconfig" {
  description = "Talosconfig for the cloud cluster"
  value       = module.talos.talosconfig
  sensitive   = true
}