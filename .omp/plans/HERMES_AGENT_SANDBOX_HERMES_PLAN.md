# hermes-agent-sandbox — Hermes Gateway + Kubernetes Agent Sandbox on dedicated Kata worker `wk-main-sandbox`

## Context

Homelab must gain a production-minded Hermes Agent deployment whose every model-generated command runs in a
Kubernetes Agent Sandbox (SIG Apps) Pod on a dedicated, Kata-isolated Talos worker. OpenTofu provisions the
dedicated Proxmox VW/VM `wk-main-sandbox` (6 vCPU / 12,288 MiB) and joins it to the existing cluster; Flux owns all
Kubernetes resources (Agent Sandbox controller + authenticated Router, admission policy, sandbox runtime image,
SandboxTemplate + WarmPool, hardened single-replica Hermes gateway, custom `agent_sandbox` terminal plugin, Cilium
default-deny policies). Sandboxes get no cluster credentials and cannot reach the Kubernetes/Talos/Proxmox APIs,
OpenRouter, or arbitrary internal resources. Every requested outcome of the task maps to a phase below; nothing is
added beyond it.

Ground rules for the implementer (inviolable, from the request):
- No cloud-init, no SSH into Talos. Talos is API/config-driven.
- Hermes gets **no Kubernetes RBAC** (no pods/create|exec|attach|portforward, no secrets, no CRD/template/warmpool
  mutation, no wildcards). It talks only to the Router over an authenticated internal HTTP API.
- Router is never deployed with `ALLOW_UNAUTHENTICATED_ROUTER=true`; ClusterIP only; ingress only from `hermes` NS.
- Never set `GATEWAY_ALLOW_ALL_USERS=true`; explicit user ID allowlists only.
- Kata is the only sandbox runtime; a silent `runc` fallback is a STOP condition. gVisor only after explicit user
  approval. Never weaken admission/PSA/RBAC/Cilium to make a check pass.
- All committed Secrets SOPS-encrypted via the repo's existing `age` workflow; plaintext purged after encryption.
- No public HTTP route in v1. Hermes API server (port 8642) enabled privately with mandatory `API_SERVER_KEY`.
- Commit with explicit paths only (never `git add -A`), never force-push, `task check` passes before each push.

## Phase 0 — Repository grounding (gates all later phases; ~1h)

Every path in later phases is stated against the expected three-layer layout
(`tofu/home/`, `kubernetes/clusters/home/`, `kubernetes/infrastructure/home/`). Confirm before use; if reality
differs, **use the actual layout — never create a parallel root**.

Run and record findings in the session:
1. `glob tofu/**/*.tf` and `read` the worker-node module (expected `tofu/home/modules/<proxmox-talos-worker>-like`):
   record exact module name, its inputs (cores/sockets/memory/disk/bridge/vlan/datastore/VMID strategy/`node`),
   how it derives the installer image (expected `talos_image_factory_url` data), and how machine config is applied
   (expected `talos_machine_configuration_apply` + `talos_machine_configuration` with `talos_machine_secrets`).
2. `grep -n "talos_version|kubernetes_version|schematic" tofu/home` → record pinned Talos, Kubernetes, provider
   versions and the existing schematic resource location.
3. `glob kubernetes/clusters/home/**` → record Flux Kustomization naming/`path`/`dependsOn`/`healthChecks`/`wait`/
   `interval`/`prune` conventions and the working example (authentik). Record how `postBuild.substitute` and the
   `kustomize.toolkit.fluxcd.io/substitute: disabled` annotation are used.
4. `grep -rn "CiliumNetworkPolicy" kubernetes/infrastructure/home --files-with-matches` → copy the existing
   default-deny + DNS-policy pattern (do not invent a second style).
5. `grep -rn "storageClassName" kubernetes/infrastructure/home | sort | uniq -c` → shortlist RWO storage classes
   in use; prefer one backed by NAS snapshots (`truenas-fast-nfs` if present). Record kubectl cluster version
   (`kubectl version`).
6. Image registry is the in-cluster **zot** registry and builds use the cluster's **buildkit builder**
   (user-confirmed). Phase 0 records how the repo reaches them, not whether to use them: zot host/service URL +
   port (from existing image refs, e.g. in-cluster DNS), zot push-credential pattern, the existing buildkit
   invocation (task targets / `docker buildx` remote builder config in repo), and how Talos node machine config
   carries the zot registry mirror/TLS block other workers already render (`machine.registries`/mirror config in
   the existing worker config generator — the new worker must render the same, verify in Phase 1). Also
   `glob images/**` for the existing image-dir convention (flat Dockerfile dirs if any).
7. `.sops.yaml` + `read .sops.yaml` → age recipients and creation-rule regexps; confirm the new paths
   (`hermes`, `agent-sandbox`) are covered, extend regexps if not, keeping existing rules.
8. `grep -rn "OPENROUTER" kubernetes tofu --files-with-matches` → locate the existing SOPS-encrypted OpenRouter
   secret (hindsight/llmkube) and the approved model slug (recorded expectation: `deepseek/deepseek-v4-flash`).
9. `grep -rn "matrix|discord|synapse" kubernetes/infrastructure/home -i` → homelab chat platforms.
10. Renovate config (`glob **/renovate*`), observability stack (victory-metrics-k8s-stack → PrometheusRule usage),
    backup mechanism for PVCs (k8up/velero/nas snapshots — record what exists).
11. File-naming: confirm where the `.local.yaml` suffix convention applies; apply it to every locally-authored
    Kubernetes manifest below unless the discovered tree says otherwise.
12. Proxmox: from module vars (`pm_*`), record target node, bridges, VLANs, datastores, VMID allocation strategy
    (next-free-rule), and whether node IPs are DHCP-reservation or static. Record the hypervisor CPU vendor
    (Intel/AMD) for the nested-virt check.

Deliverable of Phase 0: a short implementation note (conversation artifact, not committed) listing every recorded
value; later phases reference it. No repo edits in Phase 0.

## Phase 1 — OpenTofu: nested-virt preflight, schematic extension, `wk-main-sandbox`

### 1a. Nested-virt preflight (blocks VM creation)
Add a Taskfile target (repository's existing `task check` sibling), `sandbox:preflight`:
```bash
ssh <pm-user>@<pm-target-node> 'cat /sys/module/kvm_intel/parameters/nested'   # Intel
ssh <pm-user>@<pm-target-node> 'cat /sys/module/kvm_amd/parameters/nested'     # AMD
```
Replace `<pm-user>`/`<pm-target-node>` with Phase-0 values. The target must print `Y` or `1`; command must exit
non-zero and print `NESTED VIRT NOT ENABLED on <node> — enable and reboot before tofu apply` otherwise. Halt the
whole effort unless it passes — Kata runs real QEMU VMs and needs guest KVM.
Pin the VM to the verified node: pass the module's target-node input explicitly (no auto-migration to unverified
hosts). Document the compatible migration set (only nodes where the same check passes) in the module README/docs.

### 1b. Schematic extension (single image lineage — do NOT create a second schematic)
In the existing schematic file (Phase-0 item 2), extend the **existing** schematic's extension list with
`siderolabs/kata-containers` (plus the extensions normal workers already have — `siderolabs/qemu-guest-agent`,
ucode/storage extensions as already present). Use the same `talos_image_factory_extensions_versions` data-source
schema the repo already pins; do not upgrade the Talos provider for this.
Consequence (intended, document it): the schematic ID changes, so existing workers' installer URLs change on their
next Talos upgrade. `tofu plan` must show no other node diffs beyond installer-image URL.
Record the new schematic ID and the factory installer URL (used by the worker module as today).

### 1c. Worker module inputs
Add to the existing worker module (do not copy the module): `cpu_type` (string, default = module's current default;
call site passes `"host"`), `node_labels` (map(string)), `node_taints` (map(string)), feeding the machine-config
patch below. Labels/taints live ONLY in Talos machine config (never in the Proxmox resource), so changing them
never recreates the VM. Keep `ignore_changes` semantics of the module untouched; do not add `prevent_destroy` unless
the module already owns that pattern for workers.

### 1d. Root files in the discovered tofu root (expected `tofu/home/`)
- `sandbox-worker.tf`: module call with the exact fixed properties:
  `name = "wk-main-sandbox"`, `role = "worker"`, `cores = 6`, `sockets = 1`, `memory_mb = 12288`,
  `cpu_type = "host"`, `disk_gb = 64` (or repo policy minimum, whichever is larger), bridge/VLAN/datastore/
  IPv4/gateway from Phase-0 locals, VMID from the existing allocation strategy, target node from 1a,
  `node_labels = { "workload.hermes.io/sandbox" = "true" }`,
  `node_taints = { "workload.hermes.io/sandbox" = "true:NoSchedule" }`, plus existing Proxmox tags + `sandbox`.
- Machine config: worker role only, using the cluster's existing `talos_machine_secrets`; patch:
  ```yaml
  machine:
    install:
      image: <schematic-derived factory installer URL, via tofu expression, never symbolic/committed placeholder>
    network:
      hostname: wk-main-sandbox
    nodeLabels:
      workload.hermes.io/sandbox: "true"
    nodeTaints:
      workload.hermes.io/sandbox: "true:NoSchedule"
  ```
  Applied with the repo's existing `talos_machine_configuration_apply` pattern.
  Registry inheritance: because the same config generator renders all workers, the zot mirror/TLS block other
  workers already carry is inherited — confirm the rendered config for `wk-main-sandbox` contains it (Phase-0 item
  6). End-to-end pullability from zot is proven in Phase 4 when the warm pool pulls the runtime image digest.
- `sandbox-worker-outputs.tf`: `vm_id`, `worker_ip`, `schematic_id`, `installer_image`, `k8s_node_name` (first 5
  non-secret); mark any output that could expose machine config/cloudinit/secrets `sensitive = true`. Never output
  Talos secrets.
- Static-vs-DHCP: record in output comment/docs whichever the existing subnet uses. Apply must wait for Talos to
  accept config (provider's apply resource); Kubernetes readiness is verified post-apply (Phase-1 verification).

Verification (Phase 1 gate):
```bash
task sandbox:preflight                                    # prints Y|1, exit 0
tofu fmt -check -recursive && tofu validate && tofu plan  # only new VM + schematic-related diffs; no other-node changes
tofu apply                                               # VM created, Talos accepts config
talosctl -n <wk-main-sandbox-ip> version                  # pinned Talos version
kubectl get node wk-main-sandbox --show-labels            # label workload.hermes.io/sandbox=true
kubectl get node wk-main-sandbox -o jsonpath='{.spec.taints}'  # NoSchedule taint present
talosctl -n <wk-main-sandbox-ip> get extensions           # siderolabs/kata-containers listed
talosctl -n <wk-main-sandbox-ip> dmesg | grep -i kvm      # KVM present in guest dmesg
```
Disk shrink is unsupported by Proxmox: guard module calls against lowering `disk_gb` below 64 (leave the repo's
existing handling; add explicit comment in the call file).

## Phase 2 — Kata RuntimeClass + smoke test

1. **Handler discovery (no guessing):** `talosctl -n <ip> list /etc/cri/conf.d/` → find the Kata part file
   (expected `20-kata.part`); `talosctl -n <ip> read /etc/cri/conf.d/<kata-part>` → extract the containerd
   `handler = "..."` value recorded in the rendered config. Expected `kata`; **use whatever is rendered**.
2. Commit `runtimeclass-kata.local.yaml` in `kubernetes/infrastructure/home/hermes-sandbox/`:
   ```yaml
   apiVersion: node.k8s.io/v1
   kind: RuntimeClass
   metadata: { name: kata }
   handler: <observed handler>
   scheduling:
     nodeSelector: { workload.hermes.io/sandbox: "true" }
     tolerations: [ { key: workload.hermes.io/sandbox, operator: Equal, value: "true", effect: NoSchedule } ]
   ```
3. Add `hack/kata-smoke-test.sh` (hack/ unless discovery shows a scripts/ convention): applies the exact smoke Pod
   from the request (namespace `hermes-sandbox`, `runtimeClassName: kata`, `restartPolicy: Never`,
   `automountServiceAccountToken: false`, `agnhost:2.53` uname probe), waits for completion, then asserts every
   acceptance check below and deletes the Pod.

Acceptance (script exits non-zero on any failure; **STOP and report if the Pod schedules with `runc` or never
reaches Running — no silent fallback under any circumstance**):
```bash
kubectl -n hermes-sandbox get pod kata-smoke-test -o wide                         # NODE == wk-main-sandbox
kubectl -n hermes-sandbox describe pod kata-smoke-test                             # runtimeClass: kata (NOT runc)
kubectl -n hermes-sandbox logs kata-smoke-test                                     # guest uname differs from Talos host kernel (compare with talosctl dmesg kernel line)
kubectl -n hermes-sandbox exec kata-smoke-test -- test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
kubectl -n hermes-sandbox exec kata-smoke-test -- ls /dev/kvm                     # kvm node visible where runtime requires
```

## Phase 3 — Agent Sandbox stack (vendor, controller, authenticated Router, admission, base policy)

Directory `kubernetes/infrastructure/home/agent-sandbox/`:
1. **Vendor & pin:** download the all-in-one `sandbox-with-extensions.yaml` of the **newest stable release tag** of
   kubernetes-sigs/agent-sandbox available at implementation time (never `latest`, never a moving ref). Write it
   verbatim to `upstream/sandbox-with-extensions.yaml`; keep upstream ownership labels; record in `README.md`:
   tag, source URL, retrieval date, `sha256sum`, and the Kubernetes compatibility requirement. If the pinned
   release requires a newer Kubernetes than the cluster runs, select the newest release tag that is compatible, and
   record that decision. `prune` stays `false` until CRD-upgrade behavior is validated (v1beta1 APIs).
   All local changes go in Kustomize patches — never edit the vendored file.
2. **Router authentication:** find the exact auth env/secret wiring in the vendored manifest
   (`grep -n "TOKEN\|AUTH\|API_KEY" upstream/sandbox-with-extensions.yaml`). Generate a token once
   (`openssl rand -hex 32`), SOPS-encrypt it as `agent-sandbox/router-secret.local.yaml` (per `.sops.yaml`
   creation rules), wire the Router deployment to read it via `secretKeyRef` (or the manifest's documented
   mechanism). Explicitly ensure `ALLOW_UNAUTHENTICATED_ROUTER` is absent **and** false. Verify: port-forward the
   Router service; an unauthenticated request gets 401/403, an authenticated one succeeds.
3. **Service + hardening patch:** ClusterIP only — patch away any NodePort/LoadBalancer. Dedicated ServiceAccount;
   namespace scope the Router's RBAC to `hermes-sandbox` (and `agent-sandbox-system` where truly required) if the
   upstream ships cluster-scoped roles; if the controller demonstrably breaks on namespace scoping, keep upstream
   scoping and record the deviation in the README after showing the breakage. Patch resources/limits,
   `runAsNonRoot`, `capabilities.drop: [ALL]`, `seccompProfile: RuntimeDefault`, read-only rootfs where compatible.
4. **Namespaces:** `agent-sandbox-system` with PSA per repo convention (baseline; restricted only if the vendored
   workloads run clean under it — test before enforcing). `hermes-sandbox` with the exact restricted labels from
   the request. Never place Hermes/Router/shared app secrets in `hermes-sandbox`.
5. **Admission policy** `agent-sandbox/policies/validating-admission-policy.local.yaml`: if the pinned release
   ships an upstream admission example (check `upstream/` and release docs first — vendor+adapt it), use it;
   otherwise author a `ValidatingAdmissionPolicy` whose CEL matchExpressions reject a Pod when ANY of: non-approved
   `runtimeClassName`; `automountServiceAccountToken != false`; privileged container; `hostNetwork|hostPID|hostIPC`;
   `hostPath` volume; `allowPrivilegeEscalation != false`; non-empty `capabilities.add`; any container `runAsUser`
   in {0} (or missing runAsNonRoot:true); projected SA-token or arbitrary secret volumes; nodeSelector missing
   `workload.hermes.io/sandbox: "true"`; missing resources limits; missing TTL/deadline (when claim CRD exposes it
   on Pod labels — otherwise check its absence on the Pod and enforce TTL on the claim path). Bind via
   `ValidatingAdmissionPolicyBinding` matching namespace `hermes-sandbox` (and namespaces labelled
   `app.kubernetes.io/part-of: hermes`). Every field name above is verified with `kubectl explain` against the
   installed CRDs before writing the manifest.
6. **Cilium:** default-deny ingress AND egress in `agent-sandbox-system` using the repo's copied DNS-policy pattern;
   allow ingress to the Router only from namespace `hermes`; egress cluster-DNS only.
7. **Flux:** Kustomization in `kubernetes/clusters/home/` (naming per Phase-0 convention; expected
   `agent-sandbox.yaml` at cluster root with `path: ./kubernetes/infrastructure/home/agent-sandbox`):
   `wait: true`, `healthChecks` for controller and Router Deployments, `prune: false`, interval per convention.

Verification (Phase 3 gate):
```bash
kubectl -n agent-sandbox-system get deploy,pods,svc      # all Ready; service TYPE=ClusterIP only
kubectl explain sandboxtemplate.spec && kubectl explain sandboxwarmpool.spec && kubectl explain sandboxclaim.spec
# Router auth: unauth 401/403, auth passes (via kubectl port-forward + curl)
# Admission: kubectl apply a violating pod in hermes-sandbox -> rejected with policy name; delete on success/failure
hubble observe --namespace agent-sandbox-system --verdict DROPPED   # hermes-only ingress observed enforced
```

## Phase 4 — Sandbox runtime image, SandboxTemplate, WarmPool

1. **Runtime image** `images/hermes-sandbox-runtime/` (co-locate with existing repo image builds; flat
   `Dockerfile` dir). Base = the runtime base image documented by the pinned Agent Sandbox release (check release
   README); if the release documents none, pre-decided fallback: `debian:bookworm-slim` pinned by digest,
   installing the documented runtime command API from the release docs (verify against release docs before coding —
   report if the release exposes no custom-runtime contract; do not invent one silently).
   Content: sh+coreutils, git, curl, jq, ripgrep, ca-certificates, Go toolchain. FORBIDDEN (absent): kubectl,
   talosctl, tofu/terraform, pvesh, cloud CLIs, docker/socket, sshd, long-lived git creds, provider credentials.
   Non-root UID/GID **10001**; writable only `/workspace` + `/tmp`. Build with the existing cluster **buildkit**
   builder workflow (Phase-0 item 6), push to in-cluster **zot**, **digest-pin** the zot ref (via repo
   image-automation if it exists; otherwise commit the digest; never `:latest`/tag-only). Sandbox egress needs no
   zot allowance in v1: containerd on the node pulls the image BEFORE the Kata guest boots — keep zot in the
   sandbox default-deny; add it to the egress allowlist only if a concrete in-sandbox pull appears (none in v1).
   Scan: `trivy image <zot-ref>` (or repo's scanner); CRITICAL/HIGH findings block proceeding. SBOM captured.
   Runtime API smoke test (checked into `tests/`): execute command, stream output, cancel a process, enforce
   timeout, upload/download file, return real exit code — run against a throwaway claim (Phase-4 verification).
2. **Manifests** `kubernetes/infrastructure/home/hermes-sandbox/`: namespace (restricted PSA), `sandbox-template`,
   `sandbox-warm-pool`, Cilium policy. Template/warmpool YAML exactly per the request's intent blocks, with every
   field pre-verified against `kubectl explain` output of the installed CRDs (never invent fields): runtimeClass
   kata, `automountServiceAccountToken: false`, nodeSelector+toleration for the sandbox node, `runAsUser/Group`
   10001 + fsGroup, RuntimeDefault seccomp, RO rootfs, drop ALL, requests 1 CPU/4Gi, limits 4 CPU/5Gi, emptyDir
   `/workspace` 20Gi + `/tmp` 2Gi; image = digest-pinned registry ref substituted per repo pattern. WarmPool
   `replicas: 1` referencing `hermes-go` template (exact `spec.sandboxTemplateRef.name` field per CRD).
   Lifecycle (exact field names from CRD at implementation time): hard max lifetime 4 h; idle timeout 30 min where
   the Router/plugin support it; cleanup 10 min after success, immediate after failure/cancel; orphan reaper
   deletes claims past deadline. If a field does not exist in the pinned CRD, enforce it in the plugin instead and
   note it in README — do not invent manifest fields.
   Capacity guard: refresh policy must never allow two simultaneous 5 GiB-limit sandboxes to exceed node allocatable
   (12 GiB on the node); keep replenishment concurrency at 1. One warm sandbox max.
3. **Cilium (`hermes-sandbox`)**: default-deny ingress+egress. Ingress allow: Router (`agent-sandbox-system`) to the
   runtime API port + required node probes. Egress allow ONLY: cluster DNS, approved Git hosts (internal mirror if
   it exists else `github.com:443`), approved Go proxy (`proxy.golang.org` or internal mirror), package mirror if
   one is configured. Explicit labeled DENY rules (so drops are observable/alertable): `toEntities:
   kube-apiserver|kubelet|host`, `toCIDR: [169.254.169.254/32, RFC1918 blocks except allowlisted services]`,
   FQDN denies for `openrouter.ai` + any model providers. No `entity: world` egress; responses are conntracked.

Verification (Phase 4 gate):
```bash
kubectl -n hermes-sandbox get sandboxwarmpool,sandboxtemplate,events    # pool ready 1
# Manual claim lifecycle: create claim -> Sandbox pod on wk-main-sandbox, runtimeClass kata -> run a command via
#   Router API (authenticated curl through port-forward) -> real exit code -> delete claim -> pool replenishes to 1
hubble observe --namespace hermes-sandbox --verdict DROPPED             # k8s API/Talos/Proxmox/OpenRouter attempts dropped
```
From inside a disposable sandbox (script in `hack/sandbox-security-assert.sh`) each MUST fail:
```bash
test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
curl --fail --max-time 2 --insecure https://kubernetes.default.svc          # must fail
curl --fail --max-time 2 https://openrouter.ai                              # must fail
curl --fail --max-time 2 https://169.254.169.254                            # must fail
# + every Proxmox/Talos management address discovered in Phase 0: must fail
```

## Phase 5 — `agent_sandbox` Hermes terminal plugin + derived Hermes image

Package `images/hermes-agent-sandbox-plugin/` (repo image convention) plus plugin sources; package layout per the
request: `src/hermes_agent_sandbox/{__init__,provider,environment,client,config,errors,redaction}.py`,
`plugin.yaml` (`name: agent_sandbox`, `version: 0.1.0`, `kind: backend`), `pyproject.toml`, `tests/{unit,integration}`.

1. **Pin & mirror the real Hermes plugin API first.** `pip download hermes==<pinned> --no-deps` (version pinned at
   implementation time; record it) and grep the sdist for `class TerminalEnvironmentProvider`,
   `register_terminal_environment_provider`, and the concrete environment base class + execution result objects.
   Implement against the actual signatures found; the request's pseudocode is intent only. `create_environment(...)`
   MUST accept `**kwargs` unknown arguments for forward compatibility. Register as `agent_sandbox` via `register(ctx)`.
2. **Provider contract:** name `agent_sandbox`; `is_available()` true only when Router URL + credentials present;
   environment scoped per `task_id`; only the `hermes-go` template + `hermes-sandbox` namespace are ever addressable
   (hardcoded allowlist in `config.py`, not user-overridable). Never forward provider/messaging/k8s credentials into
   sandbox envs — `redaction.py` implements the explicit env allowlist and strips all secret-typed names.
3. **Environment lifecycle** (as specified): sanitized claim name from task_id + random suffix; create claim against
   `hermes-go` only; lifecycle deadline via supported CRD fields; bounded readiness wait; record claim/sandbox ids +
   creation time; cwd `/workspace`; upload repo content only after readiness; on timeout → cancel remote process
   before returning; on close/cancel/shutdown/error → finish/delete claim; on gateway restart → reconcile/delete
   orphaned claims after deadlines.
4. **Command semantics:** argv-safe API where the SDK supports it; else `/bin/bash -lc` quoting only the cwd prefix;
   reject any cwd outside `/workspace`; cap duration at min(hermes timeout, 300 s); cap output at 1 MiB with a
   truncation indicator string `\n... [output truncated]`; never log env vars; real exit code propagation; result
   shape matches the pinned Hermes environment contract exactly (verified by running the pinned Hermes terminal
   tool's own tests against a stub first, then live).
5. **Plugin config:** only the 10 env vars from the request (`AGENT_SANDBOX_*`), Router token mounted as a FILE
   (`AGENT_SANDBOX_ROUTER_TOKEN_FILE`) — never in env or pod spec inline.
6. **Image:** built with the cluster **buildkit** builder, pushed to **zot**, digest-pinned in the StatefulSet;
   derived from pinned Hermes image (`FROM <pinned-hermes-digest>`); install the pinned
   `k8s-agent-sandbox` SDK package (or the official SDK package name actually documented by the pinned release —
   confirm from release docs), ca-certificates, and the plugin at the plugin path the pinned image expects
   (verify inside the container: `docker run --rm <hermes-img> env` for `HERMES_HOME`/`HOME` and `ls` the effective
   plugins dir; the doc says `~/.hermes/plugins/` with `plugin.yaml`+`register(ctx)` — verify, never assume).
   **Gate (image is not shippable until):** `docker run --rm <img> hermes plugins list` shows `agent_sandbox`
   enabled AND `docker run --rm <img> hermes doctor` passes.
7. **Tests.** Unit (pytest, in `tests/unit/`): config validation; missing-credential unavailability;
   claim-name sanitization; template/namespace allowlisting; readiness timeout; success + non-zero exit;
   stdout/stderr; output truncation; timeout+cancellation; cleanup on success/failure/exception; orphan
   reconciliation; secret redaction; cwd-outside-`/workspace` rejection. Integration (`tests/integration/`, run
   against the live cluster with port-forwarded Router): real warm-pool sandbox; `go version` + a small
   `go test` workload; upload/download; kill long-running process; claim deletion; no SA token; blocked
   k8s/Talos/Proxmox/OpenRouter access.

Verification (Phase 5 gate): `pytest tests/unit` green; integration suite green against cluster; image gates pass.

## Phase 6 — Hermes gateway (hardened StatefulSet)

Directory `kubernetes/infrastructure/home/hermes/`:
- `namespace.local.yaml` (PSA per repo convention), `service-account.local.yaml` with
  `automountServiceAccountToken: false` — Hermes holds NO k8s credentials. (Fallback rule from the request: a
  direct-SDK SandboxClaim create/get/watch/delete RBAC role only after a separate security review; v1 does NOT
  grant it — the plugin goes through the Router.)
- `configmap.local.yaml`: exact keys per the pinned Hermes config schema (verify with the image's `hermes doctor`
  and embedded docs — the request's YAML is intent): `provider: openrouter`, model = the homelab's approved slug
  recorded in Phase 0 (expected `deepseek/deepseek-v4-flash`), terminal `backend: agent_sandbox`, `cwd: /workspace`,
  `timeout: 300`, `persistent_shell: false`; explicit non-empty user ID allowlists; `GATEWAY_ALLOW_ALL_USERS`
  absent/false. Add a startup-validation note (ConfigMap comment) tying each key to the pinned schema.
- `secret.sops.yaml` (SOPS-encrypted, one Secret, only enabled features): `OPENROUTER_API_KEY` (reuse the existing
  homelab OpenRouter key VALUE from Phase-0 item 8 — decrypt temp, re-encrypt here, purge temp),
  `API_SERVER_KEY` (independent `openssl rand -hex 32`), `AGENT_SANDBOX_ROUTER_TOKEN` (same value as router
  credential, encrypted separately; rotation updates both files — never shares Secret objects across namespaces).
  No messaging bot tokens until a platform is chosen (v1 interaction path = private API server). Never mount this
  Secret into `hermes-sandbox`.
- `statefulset.local.yaml`: replicas 1; image = digest-pinned zot ref of `hermes-agent-sandbox-plugin`; PVC 10 GiB mounted at `/opt/data` on the Phase-0 RWO storage class
  (NAS-backed, snapshot-able; NOT local-path if avoidable); `fsGroup` set to the image's data UID (verify effective
  owner: run the image once, `id` the `/opt/data` user) so PVC ownership matches; securityContext exactly per the
  request block (`runAsNonRoot`, RuntimeDefault seccomp, drop ALL, `readOnlyRootFilesystem: true`), `/tmp` as
  size-limited emptyDir; no hostPath/sockets/kubeconfig; requests 250m/512Mi, limits 2 CPU/2 GiB (tune later from
  metrics); startup/readiness TCP probes on 8642 (API server IS enabled in v1); `terminationGracePeriodSeconds: 300`
  to flush session state. Update strategy: ordered single-instance (no HPA; Recreate-style singleton behavior —
  reuse the repo's anti-deadlock convention). PDB: **omitted** (single-node cluster; a maxUnavailable:0 PDB would
  block planned maintenance) — document expected interruption in the ops doc.
- `service.local.yaml`: ClusterIP on 8642, name `hermes`. No headless service (not needed). No Gateway/Ingress/
  public route in v1 — that is a later, separately reviewed step (Authentik + cert-manager + private-only).
- API server: `API_SERVER_HOST=0.0.0.0` in-pod, `API_SERVER_KEY` required, CORS disabled/empty (no wildcard ever).
- `network-policy.local.yaml` (Cilium, copied repo pattern): default-deny in+egress; allow: cluster DNS; Router
  service in `agent-sandbox-system` (its auth'd port); FQDN `openrouter.ai` (or internal agent gateway if the repo
  has one — Phase 0 decides); observability push endpoints per the repo's existing pattern; explicit labeled DENY
  for `toEntities: kube-apiserver|host` and management CIDRs so violations are Hubble-visible. No messaging
  endpoints (no adapters enabled).
- Flux Kustomization in `kubernetes/clusters/home/` (expected `hermes.yaml`): `dependsOn: [hermes-sandbox,
  agent-sandbox]`, `wait: true`, `healthChecks` on the StatefulSet, interval per convention. Chain per request:
  agent-sandbox-controller → Router+runtimeclass+policies → hermes-sandbox (template+warmpool+policies) → hermes.

Verification (Phase 6 gate):
```bash
kubectl -n hermes get statefulset,pod,svc,pvc                                     # 1 replica Ready, PVC Bound
kubectl -n hermes exec statefulset/hermes -- hermes plugins list                  # agent_sandbox enabled
kubectl -n hermes exec statefulset/hermes -- hermes doctor                        # passes
kubectl -n hermes exec statefulset/hermes -- test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token  # no SA token
kubectl -n hermes get sa hermes -o yaml | grep automountServiceAccountToken      # false
# API auth: from a throwaway pod in-cluster, curl the service 8642 without key -> 401; with API_SERVER_KEY -> 200
```

## Phase 7 — Functional end-to-end (the acceptance test)

Script `hack/hermes-e2e.sh` (idempotent, re-runnable):
1. Approved-user request via the private API (bearer `API_SERVER_KEY`): a simple Go coding task in a fresh
   directory.
2. Assert claim appears (`kubectl -n hermes-sandbox get sandboxclaim`) and sandbox Pod runs on `wk-main-sandbox`
   with `runtimeClass: kata`.
3. Task executes `go test ./...` in the sandbox; assert stdout/stderr/exit-code correct in the Hermes response.
4. Post-completion: claim deleted; warm pool back to 1; no leftover sandbox Pods.
5. Cancel/timeout path: submit a long task, cancel; assert remote process killed and claim removed.
6. Gateway restart-orphan path: restart StatefulSet mid-task; after restart, orphaned claim owned by this gateway is
   reconciled/deleted after its deadline (assert no unbounded survivors under `hermes-sandbox`).
7. Capacity: never >1 warm + 1 active sandbox simultaneously.
8. Hubble: `hubble observe --namespace hermes --verdict DROPPED` shows no surprise; sandbox namespace drops for
   k8s/Talos/Proxmox/OpenRouter observed during the task's dependency fetches.

STOP/decision point if any assertion fails: report the failing check verbatim; never weaken policy/RBAC/PSA to pass.

## Phase 8 — Operations, backup, Renovate, docs

- **Backup:** Hermes PVC backed up via the repo's existing PVC backup path; if NAS-snapshot-capable storage is
  used, document the snapshot schedule + restore runbook. Sandbox `emptyDir`s, warm pools, runtime caches: never
  backed up.
- **Renovate:** add docker datasource entries for `hermes-agent-sandbox-plugin`, `hermes-sandbox-runtime`, the
  pinned Hermes base image, upstream Agent Sandbox release (custom/regex manager if the repo convention requires),
  `packageRules` requiring manual review (major) for Hermes, Agent Sandbox, Kata/Talos (docs note). CRD upgrades
  get a manual-review rule plus the existing rollback/upgrade doc.
- **Alerts** (PrometheusRule per repo's victory-metrics-k8s-stack convention): node `wk-main-sandbox` NotReady;
  warm pool unavailable >10 min; claims stuck Pending/Terminating >10 min; repeated Router auth failures;
  sandbox-origin attempts to management networks (Hubble denied-flow metric); node memory/disk pressure; Hermes
  crashloop/PVC mount errors. Metrics come from existing kube-state-metrics/controller instrumentation where the
  pinned release exposes them; where it doesn't, alert on CR counts via kube-state-metrics customresource metrics
  — recorded in the ops doc, never invented exports.
- **Dashboard:** one Grafana dashboard only if discovery shows an existing per-app dashboards directory pattern;
  datasource pinned to uid `VictoriaMetrics` per repo rule. Otherwise skip (alerts are the acceptance item).
- **Docs** `docs/hermes-agent-sandbox.md`: bootstrap order (tofu apply → node/Kata verify → Flux chain), operation
  runbook, upgrade procedure (incl. CRD review for v1beta1), rollback (the request's 10 steps verbatim-in-spirit),
  troubleshooting table, capacity math (6 vCPU/12 GiB, one warm + one active max, increase VM RAM before
  concurrency), threat-model summary, secret inventory + rotation table (matrix from the request).

Final repo-wide gate (before final push):
```bash
task check                                            # yamllint, tofu fmt, kustomize, sops decryptability — all green
git status                                            # review; commit explicit paths only, per-concern commits:
```
Commit sequence (8, why-split per request): (1) tofu worker+schematic+preflight; (2) kata runtimeclass + smoke;
(3) agent-sandbox stack + admission + policies; (4) sandbox runtime image + template + warm pool; (5) plugin
package + tests; (6) hermes image + gateway manifests; (7) e2e+hack scripts; (8) ops/backup/renovate/docs.
No force-push; flux syncs after push.

## Contingencies (pre-decided; implementer never stalls)
- Nested virt not `Y|1` on any node → STOP before VM creation; report the node list. Nothing else proceeds.
- Kata pods fall back to `runc` or fail scheduling → STOP after Phase 2 smoke; no workaround without user sign-off;
  the only pre-approved alternative is gVisor AFTER explicit user approval (documented), never silent.
- Cluster Kubernetes too old for the newest Agent Sandbox release → pin the newest compatible tag (recorded).
- Talos-rendered kata handler ≠ `kata` → use the observed handler everywhere; record it in the README.
- Pinned Hermes/Router/SDK field or signature differs from the request's illustrative snippets → implement against
  the PINNED release's real schema/API (verified via `kubectl explain`/sdist grep); never invent fields, never
  weaken constraints to match the snippets.
- Registry/build tooling is settled: in-cluster zot + cluster buildkit builder. If zot push requires auth and
  the repo has no credential pattern for it, create per-image zot push creds with the existing SOPS workflow.
  If the worker's rendered config lacks the zot mirror/TLS block other nodes have, add that block to the worker
  config generator exactly as existing workers carry it — never invent a second registry or mirror scheme.
- No NAS-backed RWO class available → use the repo's default RWO class and record the PVC-backup gap in the ops
  doc (never silently pick local-path; doc requires backup).
- Messaging platform absent in repo (expected) → v1 ships with the private authenticated API server as the only
  interaction path; messaging adapters are a follow-up once the user names a platform. This satisfies "an approved
  user can request a simple Go task" via the bearer-authenticated API with explicit user IDs.
```