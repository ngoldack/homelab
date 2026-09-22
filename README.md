# homelab

A Talos + Cilium homelab: **one** Kubernetes cluster spanning two sites —
Proxmox **home** VMs on the LAN, plus a single Hetzner Cloud worker joined
over Talos KubeSpan. Provisioned with OpenTofu, run by Flux for GitOps.
Services are reached on a LAN LoadBalancer VIP that Cilium announces with
ARP; DNS for it lives in the public Hetzner zone.

```text
Proxmox host (pmx-main)                 Hetzner Cloud
├── cp-main            control plane     └── home-talos-ingress-fsn1 (ARM64)
├── wk-main-efficiency general + iGPU        public IP, KubeSpan member,
└── wk-main-performance Kata + image builds  tainted for ingress only
          \                |                /
           \    Talos KubeSpan (WireGuard) /
            ──── one Kubernetes cluster ────
```

- Proxmox hosts declared as a map (`proxmox_nodes`)
- Talos cluster provisioned via OpenTofu (hand-rolled, including the
  Hetzner ingress worker — no registry module)
- Kubernetes bootstrapped with Cilium, owned by tofu
- Secrets stored with SOPS + age
- Flux used as the GitOps layer for everything except the CNI
- Talos VMs declared as individual `nodes` map entries (name, host, cores,
  affinity pin, memory, disk, role)
- Cilium default-deny per namespace, authentik at the edge, Kata microVMs for
  Hermes agent workloads, VictoriaMetrics for monitoring

There used to be a second, independent Hetzner Cloud edge cluster acting
as the sole public ingress point. It was destroyed (commit `bf444f7`);
its former ingress role is now the LAN VIP described above.

## Documentation

The root README is deliberately an index; the detail lives in `docs/`.

| Document | What it answers |
| --- | --- |
| [`docs/overview.md`](docs/overview.md) | The full walkthrough: repository layout, prerequisites, network layout, secret handling, OpenTofu flow, remote encrypted state, Flux bootstrap, storage safety, topology, validation, ingress, etcd backups, known follow-ups |
| [`docs/architecture.md`](docs/architecture.md) | Physical and logical topology, trust boundaries, failure domains, the Kata observability blind spot |
| [`docs/service-catalog.md`](docs/service-catalog.md) | Every service: owner, exposure, state, backup, RPO/RTO, dependencies |
| [`docs/data-protection.md`](docs/data-protection.md) | Backup layers, retention, and what restore evidence exists |
| [`docs/disaster-recovery.md`](docs/disaster-recovery.md) | What has actually been restored, what is only designed, and the drill procedure |
| [`docs/hermes-agent-sandbox.md`](docs/hermes-agent-sandbox.md) | The Kata-isolated agent execution boundary and its capability model |
| [`docs/llm-gateway-evaluation.md`](docs/llm-gateway-evaluation.md) | The LLM gateway options that were evaluated and why one was picked |
| [`docs/policy-exceptions.md`](docs/policy-exceptions.md) | Every deliberate Kyverno/admission-policy exception, with owner and review date |

Deep links into the walkthrough, since they are the sections people arrive for:

- [Repository layout](docs/overview.md#repository-layout) · [Prerequisites](docs/overview.md#prerequisites) · [Network layout](docs/overview.md#network-layout)
- [Secret handling](docs/overview.md#secret-handling) · [OpenTofu flow](docs/overview.md#opentofu-flow) · [Remote encrypted state](docs/overview.md#remote-encrypted-state)
- [Flux bootstrap](docs/overview.md#flux-bootstrap) · [Topology](docs/overview.md#topology) · [Ingress](docs/overview.md#ingress)
- [Storage safety policy](docs/overview.md#storage-safety-policy) · [Validation](docs/overview.md#validation) · [Talos etcd backups](docs/overview.md#talos-etcd-backups)
- [Known limitations / follow-ups](docs/overview.md#known-limitations--follow-ups)

## Quick start

Everything is driven by [`Taskfile.yml`](Taskfile.yml) — run `task` to list the
targets. The short path for a workstation that already has the tools from
[Prerequisites](docs/overview.md#prerequisites):

```bash
task kubeconfig:home:export   # writes kubeconfig-home.yaml (gitignored)
task check                    # fmt, yaml lint, tofu validate, helm validate, kustomize render, SOPS check
```

Changes ship by merging to `main`: the in-cluster GitHub runner reconciles Flux
after the push, so no laptop needs cluster credentials to deploy.
