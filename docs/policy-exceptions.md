# Policy exceptions register

**This is a derived artifact. Do not hand-edit anything below the "Derivation"
heading.**

The source of truth for every entry here is
`kubernetes/infrastructure/home/kyverno-policies/**`: the
`homelab.ngoldack.de/policy-exceptions` annotation on each ClusterPolicy (owner,
reason, review date) and the `exclude` blocks in each policy's `spec.rules[*]`
(the actual enforcement carve-out). Change those, then regenerate this table in
the same commit. A row that cannot be traced to one of those two places is stale
and must be deleted, not preserved.

Related, deliberately out of scope here (they live in other directories and carry
their own rationale comments):

- `kubernetes/infrastructure/home/agent-sandbox/policies/validating-admission-policy.yaml`
  — the `secure-hermes-sandbox` ValidatingAdmissionPolicy that hardens sandbox
  pods (this is why `hermes-sandbox` is excluded from the Pod-shape policies here).
- `kubernetes/infrastructure/home/security-baseline/default-service-accounts.yaml`
  — the `automountServiceAccountToken: false` roll-out and its skip list.
- `kubernetes/infrastructure/home/security-baseline/kustomization.yaml` — the
  recorded control gaps (no cluster-wide CCNP after the Gateway regression; no
  Kubernetes API audit policy, so PSA `audit:` labels record nothing).

## Derivation

```bash
cd kubernetes/infrastructure/home/kyverno-policies
# every policy that declares an exception, with owner/reason/review:
grep -n -B4 -A6 'homelab.ngoldack.de/policy-exceptions' policies.yaml
# every exclude block, i.e. the enforcement side of each exception:
grep -n -A24 'exclude:' policies.yaml
```

The table below was read from those two greps plus the live policy inventory
(`kubectl --kubeconfig kubeconfig-home.yaml get clusterpolicy -o custom-columns=NAME:.metadata.name,MODE:.spec.validationFailureAction`)
on **2026-09-18**. Every policy in the file is `validationFailureAction: Audit`;
nothing here is enforced yet, so an exception listed below currently only changes
what is *reported*.

Checked after writing: every policy carrying a
`homelab.ngoldack.de/policy-exceptions` annotation has exactly one row here, every
row corresponds to a policy that carries the annotation, the annotation set and
the set of policies with an `exclude` block are identical, and the namespace list
in legend A matches every `exclude` block in `policies.yaml` element for element.

## How each exception was verified

```bash
cd kubernetes/infrastructure/home/kyverno-policies
# 1. the policies render and the live Kyverno webhook accepts them (schema +
#    expression validation, no cluster mutation):
kustomize build . | kubectl --kubeconfig ../../../kubeconfig-home.yaml \
  apply --dry-run=server -f -
# 2. the A/B: same manifests with the exception annotations removed, to see what
#    the exceptions actually suppress (run only if the values are kept in Git):
kubectl get clusterpolicy -o yaml | yq 'del(.items[].metadata.annotations."homelab.ngoldack.de/policy-exceptions")'
# 3. the controls themselves, against the cluster's exact engine version
#    (kyverno CLI v1.19.1 in /tmp; fixture matrix + expected results in the
#    session log), including the apiCall rule:
kyverno apply policies.yaml --cluster --kubeconfig <kubeconfig>   # read-only
```

Review cadence: every entry carries its own review date (six months out,
`2027-03-18` for the initial set). At review, either the carve-out is still
justified and the date moves, or the excluded workloads are fixed and the
`exclude` entry is deleted from the policy.

## Legend A — `platform-namespaces`

The 21 namespaces every Pod-shape policy in this directory excludes, verbatim
from the `exclude` blocks (the same list, repeated per policy, so that a rule's
scope is readable in one place):

```
kube-system, kube-node-lease, kube-public, cilium-secrets, flux-system, kyverno,
monitoring, network, cert-manager, cnpg-system, clickhouse-operator,
seaweedfs-operator, valkey-operator-system, truenas-csi, nvidia-device-plugin,
buildkit, agent-sandbox-system, hermes-sandbox, media, tetragon, crowdsec
```

Why the list exists at all: these planes legitimately need what the policies
prohibit (host namespaces, hostPath, added capabilities, privileged/root), their
workloads are owned by upstream charts/operators rather than by app manifests, and
`flux-system`/`kyverno` must be able to write the policy objects themselves. The
full per-control reasoning is the header comment of each policy.

## Exceptions

| Policy | Scope | Owner | Reason | Review |
| --- | --- | --- | --- | --- |
| `disallow-latest-tag` | platform-namespaces (legend A) | platform | Platform planes ship image tags from upstream charts/operators (Cilium, Kyverno, CSI, device plugins); Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `restrict-host-namespaces` | platform-namespaces (legend A) | platform | CNI/CSI/device-plugin/monitoring planes need host namespaces by design; Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `disallow-host-path` | platform-namespaces (legend A) | platform | Monitoring, the image builder and the CNI/CSI/device-plugin planes mount host paths by design; Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `disallow-privileged-containers` | platform-namespaces (legend A) | platform | CNI/CSI/device-plugin/Kata/crowdsec planes are privileged by design and the media stack needs NET_ADMIN; Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `require-pod-non-root` | platform-namespaces (legend A) | platform | Platform planes legitimately run as root (CNI/CSI/device-plugin/monitoring/build/Kata); Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `require-seccomp-runtimedefault` | platform-namespaces (legend A) | platform | Platform planes need host namespaces/hostPath/privileged and have no seccomp floor of their own; Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `require-drop-all-capabilities` | platform-namespaces (legend A) | platform | Platform planes legitimately need added capabilities (media/NET_ADMIN, CNI, CSI, device plugins); Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `require-resource-requests` | platform-namespaces (legend A) | platform | Platform planes are managed by their charts/operators, whose requests are set upstream; Flux/Kyverno must write the policy objects. | 2027-03-18 |
| `disallow-default-serviceaccount` | platform-namespaces (legend A) | platform | Platform planes use the default ServiceAccount for kubelet-style duties and for cluster debug pods; Flux/Kyverno must write the policy objects. `kube-system` is the specific case that matters (its debug pods are the one place a default token may be wanted). | 2027-03-18 |
| `require-cilium-default-deny-floor` | platform-namespaces (legend A) | platform | System and platform namespaces are policed by Cilium and the platform charts themselves, and several have no workloads; Flux/Kyverno must write the policy objects. | 2027-03-18 |

Policies in this directory with **no** exceptions (nothing to register):

- `hermes-sandbox-profile-allowlist` — scoped by namespace match to
  `hermes-sandbox`, gates Agent Sandbox CRDs, and has no `exclude`.
- `hermes-session-quarantine` — matches `hermes-sandbox` only; its
  `failurePolicy: Fail` is a deliberate fail-closed choice, not an exception.
- `restrict-image-registries` — deliberately cluster-wide with no namespace
  carve-out (see its header); every image deployed on 2026-09-18 already passes
  the allowlist rule.

## Known findings that are NOT exceptions

These are reported by the audit policies — they have no `exclude`, so an audit
run keeps surfacing them. They are listed here so a reader of the reports knows
they are known, owned and dated, rather than assuming the policy is broken.

| Finding | Reported by | Population at 2026-09-18 census | Owner | Plan / review |
| --- | --- | --- | --- | --- |
| Containers without an effective `runAsNonRoot: true` | `require-pod-non-root` | 40 of 75 containers in covered namespaces | platform | Fix per workload, then flip the policy and that namespace's PSA `enforce=restricted` together. Review 2027-03-18. |
| Containers without an effective `RuntimeDefault` seccomp profile | `require-seccomp-runtimedefault` | 42 of 75 containers (none uses `Localhost`) | platform | Same flip as above. Review 2027-03-18. |
| Containers without an effective `drop: [ALL]` | `require-drop-all-capabilities` | 35 of 75 containers | platform | Same flip as above. Review 2027-03-18. |
| Containers missing `requests.cpu` and/or `requests.memory` | `require-resource-requests` | 16 of 75 containers | platform | Add requests workload-by-workload (CronJobs first). Review 2027-03-18. |
| Pods running as the `default` ServiceAccount | `disallow-default-serviceaccount` | 19 of 55 pods, in `authentik`, `langfuse`, `llmkube-system`, `paperless`, `talos-backup` | platform | Not a live credential exposure: `security-baseline/default-service-accounts.yaml` stops the token being projected in those namespaces and no binding in the cluster names a `default` subject. Fix by naming a dedicated SA per workload as each is touched. Review 2027-03-18. |
| First-party image pinned by tag | `restrict-image-registries` (`first-party-digest-pin`) | `registry.ngoldack.de/llama-p100:0.4.0-p100` in `llmkube-system` (6 of 8 first-party references were digest-pinned at census) | platform | Fixed in Git by Phase 4.1 (commit `4ac06c9`: the llmkube-models manifests pin `llama-p100@sha256:f5839c86…26aa6d`); the two llmkube-system pods still running the pre-change ReplicaSets are replaced on the next Flux reconcile, after which this finding should be empty — enforce rule 2 once a post-reconcile report is clean. Review 2027-03-18. |
| Namespaces without a Cilium default-deny floor | `require-cilium-default-deny-floor` | `default` and `reach-test` (non-Flux scratch namespaces, no pods at census time); `aigateway` is a retired lane with no resources | platform | No Flux-managed namespace is affected. Clean the scratch namespaces up or give them a pair. Review 2027-03-18. |
