# Agent Sandbox (kubernetes-sigs) — vendored upstream

Vendored manifest for the upstream Kubernetes SIG Apps Agent Sandbox controller
(core `Sandbox` + `extensions` CRDs: `SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`).

## Vendor facts

| Field | Value |
|---|---|
| Upstream repo | https://github.com/kubernetes-sigs/agent-sandbox |
| Release tag | `v1.0.2` (newest stable at retrieval time; published 2026-09-11T00:19:54Z) |
| Release URL | https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v1.0.2 |
| Manifest URL | https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.2/sandbox-with-extensions.yaml |
| Retrieved (UTC) | 2026-09-13T18:19Z |
| `sha256sum` | `e3dda8359fee2b1bb6f4a9448d7004888286482b17670c581dee976f370fd165` |
| Size | 427,915 bytes / 9,014 lines / 12 YAML documents |
| Local path | `upstream/sandbox-with-extensions.yaml` (verbatim, unmodified) |

## Kubernetes compatibility

No explicit minimum Kubernetes server version is stated at tag `v1.0.2` (release
notes, repo README, install-prerequisites docs, and the Helm chart `kubeVersion`
are all silent). Observed at the tag:

* `go.mod` builds against `k8s.io/api`/`apimachinery`/`client-go` **v0.37.0** and
  `sigs.k8s.io/controller-runtime` **v0.25.0** (Kubernetes 1.37 client-library line).
* The shipped admission example (`examples/policy/vap/README.md`) requires
  **Kubernetes v1.30+** for `ValidatingAdmissionPolicy`.
* Client-vs-server skew: v0.37 client-go against a 1.36 API server (one minor
  behind, this cluster's version) is within the usual Kubernetes skew envelope,
  but the controller against a 1.36 server must be proven by a live apply/run —
  verify with the Phase-3 gate (`kubectl apply` + controller Ready). If the
  controller fails on 1.36, pin the newest compatible release tag per the plan's
  contingency and record the decision here.

## Content of the vendored all-in-one manifest

Core + extensions controller, its RBAC, and all four CRDs. **The Router is NOT
included** — upstream ships it separately in `sandbox-router/deploy/`
(namespace `agent-sandbox-system`, Service `sandbox-router-svc`, image
`registry.k8s.io/agent-sandbox/sandbox-router-go:v1.0.2`; note the example
`deployment.yaml` at the tag still says `:latest` — pin it). See the
field-reference report produced with this vendoring for exact names/line
numbers.

| Document | Name | Line |
|---|---|---|
| Namespace | `agent-sandbox-system` | 2 |
| CRD | `sandboxclaims.extensions.agents.x-k8s.io` (v1beta1) | 7 |
| CRD | `sandboxes.agents.x-k8s.io` (v1beta1) | 269 |
| CRD | `sandboxtemplates.extensions.agents.x-k8s.io` (v1beta1) | 4418 |
| CRD | `sandboxwarmpools.extensions.agents.x-k8s.io` (v1beta1) | 8688 |
| ServiceAccount | `agent-sandbox-controller` (`agent-sandbox-system`) | 8779 |
| ClusterRole | `agent-sandbox-controller` | 8787 |
| ClusterRole | `agent-sandbox-controller-extensions` | 8847 |
| ClusterRoleBinding | `agent-sandbox-controller` | 8934 |
| ClusterRoleBinding | `agent-sandbox-controller-extensions` | 8947 |
| Service | `agent-sandbox-controller` (ClusterIP, :8080 metrics) | 8960 |
| Deployment | `agent-sandbox-controller` (`registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.2`, args `--leader-elect=true --extensions`) | 8976 |

## Vendor rule

**`upstream/sandbox-with-extensions.yaml` is verbatim upstream content and MUST
NOT be edited.** All local changes (image pinning, resource limits, security
context, namespaces, RBAC scoping, Cilium policies, admission policies, Router
deployment) go into Kustomize patches / separate manifests layered on top of
this directory. Never apply `latest`/moving refs; re-vendor new releases by
replacing `upstream/` and updating this README (tag, URL, date, sha256, sizes,
compat notes).

`kustomization.yaml` in this directory keeps `prune` semantics of the Flux
Kustomization in mind: upstream `sandboxes.agents.x-k8s.io` / extension CRDs are
v1beta1 — validate CRD-upgrade behavior before ever enabling `prune` on
`upstream/` content (see plan Phase 3).

SDK package for the pinned release: `pip install k8s-agent-sandbox==1.0.2`
(the Python SDK import name is `k8s_agent_sandbox`). Upstream Python router
image lineage: `us-central1-docker.pkg.dev/k8s-staging-images/agent-sandbox/sandbox-router`.