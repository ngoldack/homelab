# Cilium, tofu-owned, matching how tofu/cloud owns Cilium permanently for
# that cluster (see tofu/cloud/cloud.tf). Talos ships with no CNI, so this is
# what Flux's own controllers need already running before they can schedule
# anything — previously a manual, undocumented-outside-README `helm install`
# step; now it comes up as part of `tofu apply` like everything else, no
# adoption dance needed since there's nothing pre-existing to adopt.

resource "helm_release" "cilium" {
  name             = "cilium"
  namespace        = "kube-system"
  create_namespace = false
  repository       = "https://helm.cilium.io"
  chart            = "cilium"
  version          = "1.19.5"

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
    # No gatewayAPI here: home has no Gateway/HTTPRoute of its own (ingress
    # only ever enters via the cloud cluster's Gateway — see README's "Cloud
    # ingress").
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
