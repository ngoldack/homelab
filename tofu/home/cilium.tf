# Cilium, tofu-owned, matching how tofu/cloud owns Cilium permanently for
# that cluster (see tofu/cloud/cloud.tf). Talos ships with no CNI, so this is
# what Flux's own controllers need already running before they can schedule
# anything — previously a manual, undocumented-outside-README `helm install`
# step; now it comes up as part of `tofu apply` like everything else, no
# adoption dance needed since there's nothing pre-existing to adopt.

resource "helm_release" "cilium" {
  # cilium-operator's Gateway API check is one-shot with no retry, so the CRDs
  # must exist before it starts — see gateway-api-crds.tf.
  depends_on = [helm_release.gateway_api_crds]

  name             = "cilium"
  namespace        = "kube-system"
  create_namespace = false
  repository       = "https://helm.cilium.io"
  chart            = "cilium"
  version          = "1.19.5"

  # The helm provider's default is 300s, which is not enough to roll the agent
  # DaemonSet across every node — especially with rollOutCiliumPods enabled,
  # where each agent is restarted in turn and has to re-establish its datapath
  # before the next one goes. A values change here timed out at 5m with the
  # rollout only half done; the rollout itself was healthy and finished on its
  # own, but tofu had already given up and reported failure.
  timeout = 900

  values = [yamlencode({
    cluster = {
      name = "home"
      id   = 1
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
    # No Cilium pod-to-pod encryption: Talos KubeSpan already encrypts all
    # node-to-node traffic at the OS layer, so Cilium's WireGuard/IPsec
    # datapath encryption is left disabled (its default) to avoid double
    # encryption. tunnelProtocol=vxlan is just the overlay encapsulation,
    # not a VPN.
    #
    # Public ingress. There is no separate cloud cluster any more: the Hetzner
    # ingress node (see ingress.tf) is a worker of THIS cluster, so a Gateway
    # here can route to any pod on any node through ordinary in-cluster
    # networking — no ClusterMesh, no global Services, no cross-cluster
    # underlay.
    gatewayAPI = {
      enabled = true
      # Local, not the chart's default of Cluster. This fixes intermittent 503s.
      #
      # cilium-envoy is a DaemonSet, so it runs on EVERY node including the
      # Hetzner ingress node. Under Cluster policy the node that receives the
      # VIP traffic load-balances it across all of them, so roughly one request
      # in five was handed to the Envoy on the Hetzner node — which cannot
      # reach pods on the LAN nodes at all (the same host-netns limitation that
      # made the original public-ingress design unworkable) and answers 503.
      #
      # Local keeps the request on the node that received it. Only home nodes
      # answer ARP for the VIP (CiliumL2AnnouncementPolicy is restricted to
      # topology.homelab/site=home), so the receiving node is always one that
      # can reach the backends. It also preserves the client source IP, which
      # Cluster policy SNATs away.
      externalTrafficPolicy = "Local"
      gatewayClass = {
        # Must be the STRING "true", not a bool (the chart's schema rejects a
        # boolean: "got boolean, want string"), and not the "auto" default:
        # "auto" gates creation on a .Capabilities cluster lookup, which is
        # both fragile and silently unsatisfiable under client-side rendering.
        create = "true"
      }
      hostNetwork = {
        # OFF. This used to bind Envoy's listener directly on the Hetzner
        # ingress node, because that node had a public IP and nothing here
        # could assign a LoadBalancer address.
        #
        # That design could not serve home workloads, and not merely slowly:
        # Envoy ran in the ingress node's HOST network namespace, and from
        # there it could not reach pods on the LAN nodes at all. Every request
        # to Immich returned 503 with the backend healthy and answering 200
        # in-cluster, and Cilium on the ingress node listing the right endpoint
        # as active. Traffic also crossed the WAN twice for a service in the
        # same room as the client.
        #
        # The replacement is a LoadBalancer address on the LAN, handed out by
        # CiliumLoadBalancerIPPool and announced with ARP by
        # CiliumL2AnnouncementPolicy (both in
        # kubernetes/infrastructure/home/network/). Envoy then runs as an
        # ordinary pod on a home node and reaches backends over normal pod
        # networking.
        #
        # Note this flag is GLOBAL — the chart offers no per-Gateway override,
        # so it cannot be on for a public Gateway and off for an internal one
        # at the same time. Turning it on again would break the LAN VIP.
        enabled = false
      }
    }

    # Envoy still binds 443, which is privileged, so these stay even though
    # host-network mode is now off — the bind just happens in the pod's own
    # network namespace instead of the node's. BOTH halves are needed —
    # the chart's values.yaml is explicit that keepCapNetBindService applies
    # "in addition to granting the capability to the container", and setting
    # only one leaves the bind failing with
    # "cannot bind '0.0.0.0:443': Permission denied". NET_ADMIN and SYS_ADMIN
    # are the chart's own defaults, restated because Helm replaces lists
    # wholesale rather than merging them.
    envoy = {
      securityContext = {
        capabilities = {
          envoy                 = ["NET_ADMIN", "SYS_ADMIN", "NET_BIND_SERVICE"]
          keepCapNetBindService = true
        }
      }
    }

    # This cluster now spans the LAN and one internet node, and traffic to that
    # node rides inside Talos KubeSpan's WireGuard tunnel (MTU 1420, confirmed
    # live via `talosctl get kubespanconfig`). Cilium's VXLAN adds ~50 bytes on
    # top, so the pod MTU has to come down to 1370 or large payloads blackhole
    # across the WAN leg while small ones pass — the failure mode where ping
    # works and TLS handshakes hang.
    MTU = 1370

    # L2 announcements: a home node answers ARP for the LoadBalancer VIPs
    # handed out by CiliumLoadBalancerIPPool, so services get a real, stable
    # address on the LAN (10.30.0.0/24) that any client on the network can
    # reach — including remote clients arriving over UniFi Teleport, which
    # lands them on the LAN like any other local device.
    #
    # This is what replaces gatewayAPI.hostNetwork above. Leadership is per
    # service and fails over between eligible nodes, so the VIP survives a
    # single node reboot rather than being tied to one machine's IP.
    l2announcements = {
      enabled = true
    }

    # L2 announcements lease-renew through the Kubernetes API, and Cilium's
    # default client rate limit is low enough that the agents start logging
    # client-side throttling once leases are in play. Raised per Cilium's own
    # L2-announcement guidance; without it leadership flaps under load and the
    # VIP briefly stops answering ARP.
    k8sClientRateLimit = {
      qps   = 20
      burst = 100
    }

    # Roll the agents when values change, rather than leaving nodes on a stale
    # MTU or datapath config until something else restarts them.
    rollOutCiliumPods = true

    # rollOutCiliumPods covers only the AGENT DaemonSet, not the operator
    # Deployment — Helm rewrites the cilium-config ConfigMap but the operator's
    # own pod spec is unchanged, so it keeps running with whatever config it
    # started with, potentially for weeks.
    #
    # That is not cosmetic. Enabling gatewayAPI made the agents wait on
    # ciliumenvoyconfigs/ciliumclusterwideenvoyconfigs CRDs, which only the
    # OPERATOR creates — and the 18-hour-old operator had been started before
    # the flag existed, so it never created them. The agents sat in
    # CrashLoopBackOff ("Still waiting for Cilium Operator to register CRDs")
    # and the rollout wedged at 2/4 until the operator was restarted by hand.
    # This makes that restart part of the apply.
    operator = {
      rollOutPods = true
    }
    # NOTE: there is deliberately no CA pinned here. A top-level `ca = {cert,
    # key}` block used to sit at this spot, meant to give both clusters an
    # identical ClusterMesh CA. It never did anything: the chart reads the CA
    # from `tls.ca.*`, not the top level, so Helm silently ignored it as an
    # unknown value and Cilium generated its own CA anyway. Both reasons to
    # have it are now gone — ClusterMesh was dropped when the two clusters
    # were consolidated into this one, so there is no peer to share a CA with,
    # and a self-generated CA is the right default for a single cluster.
    # cilium_ca_crt/cilium_ca_key remain in secret.sops.yaml, unused.
  })]
}
