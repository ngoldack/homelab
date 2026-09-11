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
      gatewayClass = {
        # Must be the STRING "true", not a bool (the chart's schema rejects a
        # boolean: "got boolean, want string"), and not the "auto" default:
        # "auto" gates creation on a .Capabilities cluster lookup, which is
        # both fragile and silently unsatisfiable under client-side rendering.
        create = "true"
      }
      hostNetwork = {
        # Envoy binds the listener directly on the node instead of waiting for
        # a LoadBalancer Service address that nothing would ever assign — there
        # is no CiliumLoadBalancerIPPool here and an hcloud LB is a paid
        # resource we do not need when the ingress node already has a public IP.
        enabled = true
        nodes = {
          # Binds 80/443 ONLY on the Hetzner ingress node. This selector FAILS
          # OPEN: if it matches nothing — a typo, a renamed label, a node that
          # never got the label — Cilium binds 443 on every node in the
          # cluster, including the LAN ones, and logs nothing about it. The
          # label is set in ingress.tf via machine.nodeLabels. Verify after any
          # change with:
          #   kubectl -n kube-system get cm cilium-config \
          #     -o jsonpath='{.data.gateway-api-hostnetwork-nodelabelselector}'
          #   nmap -p 443 10.30.0.10 10.30.0.21 10.30.0.22 10.30.0.23   # silent
          matchLabels = {
            "node.homelab/role" = "ingress"
          }
        }
      }
    }

    # Required for the above: in host-network mode Envoy binds the listener
    # port on the node itself, and 443 is privileged. BOTH halves are needed —
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
    # Roll the agents when this changes, rather than leaving nodes on a stale
    # MTU until something else restarts them.
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
    #
    # Shared ClusterMesh CA, identical on both clusters (see tofu/cloud/cloud.tf's
    # cilium_values) — passed as a Helm value rather than a pre-created
    # "cilium-ca" Kubernetes Secret, because cloud's Cilium is rendered
    # client-side (`helm_template`, no live-cluster lookup) and could never
    # detect/reuse a pre-existing secret there; a values-based CA works
    # identically for both a real helm_release (here) and a client-side
    # render (cloud), with no apply-ordering dependency either way.
    ca = {
      cert = base64encode(local.secrets["cilium_ca_crt"])
      key  = base64encode(local.secrets["cilium_ca_key"])
    }
  })]
}
