# github-runner CI: repo-scoped self-hosted runner + Flux reconcile on push

## Context

The repo has no CI at all (`.github/` does not exist; `README.md:562` says "There is no CI", `.sops.yaml` records that the CI age recipient was dropped). Today the operator runs `flux reconcile source git flux-system` and then `flux reconcile kustomization <name>` by hand from a laptop after merges.

End state: one small self-hosted runner pod in the cluster, registered for `ngoldack/homelab` **only**, running a workflow that performs that reconciliation on `push` to `main` (and on manual dispatch). No GitHub-side secret, no kubeconfig on a laptop. Merging to `main` remains the only release mechanism — CI only removes the up-to-10-minute Flux poll delay and the manual nudge.

Facts this plan is built on (all read this session): cluster default PodSecurity is `baseline` with per-namespace overrides (`media/namespace.yaml`); apps that harden use `enforce/audit/warn: restricted` (`hermes/namespace.yaml`); every app namespace ships a Cilium default-deny + allowlist pair; images are built in-cluster by BuildKit Jobs in `kubernetes/infrastructure/home/image-builds/` (namespace `buildkit`, `moby/buildkit:v0.31.0`, mTLS secret `buildctl-client-tls`, push credentials `registry-credentials`, node selector `topology.homelab/site: home`) and deployed by manifest digest; the reconciled objects are GitRepository `flux-system` (branch `main`, 1m interval) and root Kustomization `flux-system`, both in `flux-system` (verified in `kubernetes/clusters/home/flux-system/gotk-sync.yaml`).

## Approach

### Step 1 — Runner image (built in-cluster, digest-pinned, no git-SHA chicken-and-egg)

The official `ghcr.io/actions/actions-runner` image has no `flux` CLI, and installing it at job time is impossible (PSA `restricted` blocks setuid/sudo). Build one small image. Ship the Dockerfile **and** the entrypoint as ConfigMap keys and use the ConfigMap directory as *both* build context and dockerfile local (`--local=context=... --local=dockerfile=...`) — the same ConfigMap trick `llama-p100.yaml` uses for its Dockerfile — so the Job needs no `https://github.com/ngoldack/homelab.git#<sha>` pin and the build can be committed in the same push as its inputs.

New files:

- `kubernetes/infrastructure/home/image-builds/github-runner/Dockerfile`
- `kubernetes/infrastructure/home/image-builds/github-runner/runner-entrypoint.sh`
- `kubernetes/infrastructure/home/image-builds/github-runner.yaml` (Job `build-github-runner-1` in namespace `buildkit`, modelled on `llama-p100.yaml`)
- edits to `kubernetes/infrastructure/home/image-builds/kustomization.yaml` (one `resources:` entry + one `configMapGenerator` entry)

Resolve the two base digests and the current flux CLI tag first (no `crane` needed):

```bash
gh api repos/fluxcd/flux2/releases/latest --jq .tag_name    # e.g. v2.7.5
tok=$(curl -s "https://ghcr.io/token?service=ghcr.io&scope=repository:actions/actions-runner:pull" | jq -r .token)
curl -sI -H "Authorization: Bearer $tok" \
  -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" \
  https://ghcr.io/v2/actions/actions-runner/manifests/latest | tr -d '\r' \
  | awk 'tolower($1)=="docker-content-digest:"{print $2}'
# repeat with scope repository:fluxcd/flux-cli and tag <flux-tag>
```

`github-runner/Dockerfile` (digests substituted; keep the header comment style of the sibling Dockerfiles):

```dockerfile
# syntax=docker/dockerfile:1
# The runner pod needs the flux CLI and curl+jq for the registration-token
# call; the upstream actions-runner image has neither, and nothing can be
# installed at job time under PSA restricted.
FROM ghcr.io/fluxcd/flux-cli:<flux-tag>@sha256:<flux-digest> AS flux
FROM ghcr.io/actions/actions-runner@sha256:<runner-digest>

# The image's own entrypoint runs an already-configured runner; registration
# must happen first, so the entrypoint is replaced wholesale.
USER root
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates jq \
 && rm -rf /var/lib/apt/lists/*
COPY --from=flux /usr/local/bin/flux /usr/local/bin/flux
COPY runner-entrypoint.sh /usr/local/bin/runner-entrypoint.sh
RUN chmod 0755 /usr/local/bin/runner-entrypoint.sh \
 && chown -R 1001:1001 /home/runner
USER 1001:1001
ENTRYPOINT ["/usr/local/bin/runner-entrypoint.sh"]
```

`github-runner/runner-entrypoint.sh` (exact content; all inputs come from pod env):

```bash
#!/usr/bin/env bash
# Registers this pod as a repo-scoped GitHub Actions runner and runs exactly
# one job (--ephemeral), so no long-lived runner registration and no state
# outlives a job.
set -euo pipefail

: "${RUNNER_REPO_URL:?set RUNNER_REPO_URL, e.g. https://github.com/ngoldack/homelab}"
: "${RUNNER_REPO_SLUG:?set RUNNER_REPO_SLUG, e.g. ngoldack/homelab}"
: "${RUNNER_PAT:?set RUNNER_PAT from the github-runner secret}"

token=$(curl -sSf -X POST \
  -H "Authorization: Bearer ${RUNNER_PAT}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/repos/${RUNNER_REPO_SLUG}/actions/runners/registration-token" | jq -r .token)
[[ -n "$token" && "$token" != "null" ]] || { echo "no registration token returned by GitHub" >&2; exit 1; }

cd /home/runner
./config.sh --unattended --ephemeral --replace \
  --url "$RUNNER_REPO_URL" \
  --token "$token" \
  --name "$(hostname)" \
  --labels "${RUNNER_LABELS:-homelab}" \
  --work "${RUNNER_WORKDIR:-/home/runner/_work}"
./run.sh
```

`github-runner.yaml` Job — copy `llama-p100.yaml` (same certs/docker volumes, `nodeSelector`, `backoffLimit: 2`, `restartPolicy: Never`) and change:

- `metadata.name: build-github-runner-1`
- args: drop the `--opt=context=...git#sha` pin, add `--local=context=/dockerfile` + `--local=dockerfile=/dockerfile` + `--opt=filename=Dockerfile` + `--opt=dockerfilekey=Dockerfile`
- `--output=type=image,name=registry.ngoldack.de/github-runner:1,push=true`
- cache refs both `registry.ngoldack.de/github-runner:buildcache` (`--export-cache ...,mode=max` and `--import-cache` identical)
- volumes: `github-runner-dockerfile` ConfigMap instead of `llama-p100-dockerfile`; keep the `certs` and `docker` secrets
- header comment: what the image is, that a rebuild means bumping Job name suffix + tag + base digests together (Jobs are immutable; Flux prunes the old one), and the `--local context` reason

`image-builds/kustomization.yaml` additions (both option blocks mirror the existing ones, including `kustomize.toolkit.fluxcd.io/substitute: disabled` — the Dockerfile and script contain `${...}` that Flux's postBuild substitution would otherwise mangle):

```yaml
  - github-runner.yaml            # under resources:
configMapGenerator:
  - name: github-runner-dockerfile
    namespace: buildkit
    files:
      - github-runner/Dockerfile
      - github-runner/runner-entrypoint.sh
    options:
      annotations:
        kustomize.toolkit.fluxcd.io/substitute: disabled
```
(`generatorOptions.disableNameSuffixHash` is already true for the directory; do not duplicate it.)

If `COPY --from=flux /usr/local/bin/flux` fails, the build fails loudly; fall back to a checksum-pinned release tarball in the Dockerfile (`curl -sSLo /tmp/flux.tar.gz https://github.com/fluxcd/flux2/releases/download/<tag>/flux_<ver>_linux_amd64.tar.gz`, checksum computed once with `curl -sSL <url> | sha256sum` and written into the file, then `sha256sum -c`).

### Step 2 — `github-runner` app directory

New directory `kubernetes/infrastructure/home/github-runner/`; note the Deployment arrives in a second push (its image digest does not exist yet — no placeholder digest is ever committed).

`namespace.yaml` — mirror `hermes/namespace.yaml` exactly (this cluster defaults to `baseline`; a runner holding cluster-write RBAC and a PAT must be `restricted`):
```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: github-runner
  labels:
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
```

`serviceaccount.yaml`:
```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ci-runner
  namespace: github-runner
```

`rbac.yaml` — exactly these rules, and nothing else, in `flux-system` only (the workflow's `flux`/`kubectl` calls use this SA through the pod's in-cluster token; `kubectl annotate --overwrite` is a patch, and `watch` is what `kubectl wait` needs):
```yaml
# The workflow runs with this SA (the pod's in-cluster service-account token),
# scoped to flux-system: the runner may nudge reconciliation and read state,
# it may not create or edit workload objects anywhere.
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ci-flux-reconcile
  namespace: flux-system
rules:
  - apiGroups: ["kustomize.toolkit.fluxcd.io"]
    resources: ["kustomizations"]
    verbs: ["get", "list", "watch", "patch"]
  - apiGroups: ["source.toolkit.fluxcd.io"]
    resources: ["gitrepositories"]
    verbs: ["get", "list", "watch", "patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ci-flux-reconcile
  namespace: flux-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ci-flux-reconcile
subjects:
  - kind: ServiceAccount
    name: ci-runner
    namespace: github-runner
```

`secret.sops.yaml` — create plaintext `secret.local.yaml` (gitignored by `*.local.yaml`) with:
```yaml
apiVersion: v1
kind: Secret
metadata:
  name: github-runner
  namespace: github-runner
type: Opaque
stringData:
  access-token: <classic PAT, `repo` scope, supplied by the user>
```
then `task sops:encrypt SOURCE=kubernetes/infrastructure/home/github-runner/secret.local.yaml FILE=kubernetes/infrastructure/home/github-runner/secret.sops.yaml`, verify with `task sops:decrypt FILE=… FILE=…secret.sops.yaml OUTPUT=…/secret.local.yaml` or `task sops:check:all`, and delete the plaintext afterwards. The PAT is the only non-derivable input: it needs `repo` scope to call `POST /repos/ngoldack/homelab/actions/runners/registration-token` (a fine-grained token with repository permission `Administration: write` also works).

`cilium-default-deny.yaml` — byte-for-byte the sibling shape from `media/` or `buildkit/`.

`cilium-allowlist.yaml` — mirror `agentgateway/cilium-allowlist.yaml`'s egress literals; for this namespace the rules are exactly:
```yaml
  egress:
    - toEndpoints:            # DNS
        - matchLabels:
            k8s:io.kubernetes.pod.namespace: kube-system
            k8s-app: kube-dns
      toPorts:
        - ports:
            - port: "53"
              protocol: UDP
            - port: "53"
              protocol: TCP
    - toEntities:              # the API server the job's flux/kubectl calls hit
        - kube-apiserver
      toPorts:
        - ports:
            - {port: "443", protocol: TCP}
            - {port: "6443", protocol: TCP}
    - toCIDRSet:
        - cidr: 10.30.0.10/32  # control-plane VIP (same three addressing forms as agentgateway)
      toPorts:
        - ports:
            - {port: "443", protocol: TCP}
            - {port: "6443", protocol: TCP}
    - toCIDRSet:
        - cidr: 172.21.0.0/16  # in-cluster Service CIDR
      toPorts:
        - ports:
            - {port: "443", protocol: TCP}
            - {port: "6443", protocol: TCP}
    # GitHub: job/action downloads span github.com, api.github.com,
    # codeload.github.com, objects.githubusercontent.com and Azure blob
    # storage, so a host allowlist would break silently — world:443 is the
    # deliberate grant, and the reason workflows may only come from this
    # repo's own main branch (no pull_request trigger in the workflow).
    - toEntities:
        - world
      toPorts:
        - ports:
            - {port: "443", protocol: TCP}
```
Ingress: none (nothing connects to a runner) — one comment line saying so, no rules.

`kustomization.yaml` — plain `resources:` list, sibling ordering (`cilium-default-deny.yaml`, `cilium-allowlist.yaml`, `namespace.yaml`, `serviceaccount.yaml`, `rbac.yaml`, `secret.sops.yaml`).

`deployment.yaml` (added in push 2, after the digest exists):
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: github-runner
  namespace: github-runner
spec:
  replicas: 1
  # One runner identity for this repo: never two pods registering the same
  # name at once.
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: github-runner
  template:
    metadata:
      labels:
        app.kubernetes.io/name: github-runner
    spec:
      serviceAccountName: ci-runner
      # Keep the runner on the home site, like the image-build Jobs: the
      # 2 GiB Hetzner worker has no business running CI.
      nodeSelector:
        topology.homelab/site: home
      securityContext:
        runAsNonRoot: true
        runAsUser: 1001
        runAsGroup: 1001
        fsGroup: 1001
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: runner
          image: registry.ngoldack.de/github-runner@sha256:<digest from the build Job log>
          env:
            - name: RUNNER_REPO_URL
              value: https://github.com/ngoldack/homelab
            - name: RUNNER_REPO_SLUG
              value: ngoldack/homelab
            - name: RUNNER_LABELS
              value: homelab
            - name: RUNNER_PAT
              valueFrom:
                secretKeyRef:
                  name: github-runner
                  key: access-token
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              cpu: "1"
              memory: 2Gi
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          # readOnlyRootFilesystem stays false: config.sh writes .runner,
          # _temp and _diag next to the install under /home/runner.
          volumeMounts:
            - name: work
              mountPath: /home/runner/_work
      volumes:
        - name: work
          emptyDir: {}
```
No probes: the entrypoint runs under `set -e`, so a bad PAT or failed registration exits the container (visible as `CrashLoopBackOff`) instead of a silently unready pod. After each job the ephemeral runner exits 0 and the kubelet restarts it, registering a fresh runner; jobs queue meanwhile.

### Step 3 — Cluster wiring

`kubernetes/clusters/home/github-runner.yaml` — copy `kubernetes/clusters/home/media.yaml` shape and change: `metadata.name: github-runner`, `path: ./kubernetes/infrastructure/home/github-runner`, **no `dependsOn`** (no CRDs, no Gateway, no datastore), keep `interval: 10m`, `prune: true`, `wait: false`, `timeout: 15m`, `retryInterval: 2m`, `sourceRef` GitRepository `flux-system`, `decryption` sops/`sops-age`, `postBuild.substituteFrom` ConfigMap `cluster-vars`. Header comment in the file's style: what the runner is, that RBAC is a Role in flux-system, and why the workflow is push-only.

Add `- github-runner.yaml` to `kubernetes/clusters/home/kustomization.yaml` with a comment ("Self-hosted GitHub Actions runner for this repo: performs the post-merge Flux reconcile, inside the cluster instead of on the operator's laptop."), placed after `media.yaml`.

### Step 4 — Workflow and README

`.github/workflows/flux-reconcile.yml` (create the `.github/workflows/` directory):

```yaml
# Post-merge Flux reconciliation, run on the in-cluster self-hosted runner:
# the runner pod carries a ServiceAccount bound to a flux-system Role, so the
# job reaches the API server directly and no laptop holds a kubeconfig.
#
# Manual nudge: gh workflow run flux-reconcile.yml
#
# Trigger is deliberately push-to-main plus manual dispatch only. The runner
# holds write access to Flux objects and its PAT is readable from its own
# process environment, so pull-request workflows must never run here.
name: flux reconcile

on:
  push:
    branches: [main]
  workflow_dispatch:

permissions:
  contents: read

concurrency:
  group: flux-reconcile
  cancel-in-progress: false

jobs:
  reconcile:
    runs-on: [self-hosted, homelab]
    timeout-minutes: 20
    steps:
      - name: Point the flux CLI at the in-cluster API server
        run: |
          mkdir -p "$HOME/.kube"
          cat > "$HOME/.kube/config" <<EOF
          apiVersion: v1
          kind: Config
          clusters:
            - name: in-cluster
              cluster:
                certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
                server: https://${KUBERNETES_SERVICE_HOST}:${KUBERNETES_SERVICE_PORT_HTTPS:-443}
          users:
            - name: ci-runner
              user:
                tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
          contexts:
            - name: in-cluster
              context:
                cluster: in-cluster
                user: ci-runner
          current-context: in-cluster
          EOF

      - name: Force a fetch of main
        run: flux reconcile source git flux-system -n flux-system

      - name: Reconcile the root Kustomization
        run: flux reconcile kustomization flux-system -n flux-system

      - name: Nudge every child Kustomization
        run: >-
          kubectl annotate kustomizations.kustomize.toolkit.fluxcd.io -n flux-system --all
          "reconcile.fluxcd.io/requestedAt=$(date -u +%Y-%m-%dT%H:%M:%SZ)" --overwrite

      - name: Wait for every Kustomization to be Ready
        run: >-
          kubectl wait kustomizations.kustomize.toolkit.fluxcd.io -n flux-system --all
          --for=condition=Ready --timeout=15m

      - name: Status
        if: always()
        run: flux get kustomizations -n flux-system
```
Step order is load-bearing: the source is refreshed before the children are annotated, so each child already sees the new revision and reconciles it immediately instead of waiting for its own 10m poll.

README edits:
- Replace the block at `README.md:562-565` ("There is no CI — …") with: `task check` is still the pre-push gate, plus a short paragraph on the self-hosted runner — registered for this repo only, workflow `.github/workflows/flux-reconcile.yml`, runs on push to `main` and via `gh workflow run flux-reconcile.yml`, holds a `flux-system` Role scoped to nudging reconciliation, authenticates to GitHub with a PAT stored in the SOPS secret `kubernetes/infrastructure/home/github-runner/secret.sops.yaml`.
- `README.md:283` ("There is no CI recipient any more"): keep the statement, add one clause that the CI runner never decrypts SOPS (it reads its PAT from the in-cluster Secret), so `.sops.yaml` is unchanged.
- Leave the historical etcd-backup paragraph (`README.md:795-812`) alone, and leave `.omp/plans/LANGFUSE_PHOENIX_OIDC_METRICS_PLAN.md` (the only other place in the repo that documents a manual `flux reconcile`) untouched — it is a dated record, not instructions. `grep -rn "no CI" README.md` must afterwards match only the reworded paragraph; the only other repo-wide hits are vendored `.terraform` provider changelogs and a "CIDR" false positive in `hermes/cilium-policy.yaml`.

### Step 5 — Landing order (two pushes)

1. Push 1: Step 1 files + Step 2 minus `deployment.yaml` + Step 3 + Step 4. Flux applies the app (no Deployment yet), the `image-builds` Kustomization runs `build-github-runner-1`.
2. Read the digest from the Job log (`kubectl -n buildkit logs job/build-github-runner-1 | grep -i "exporting manifest"` → `sha256:<digest>`; `registry.ngoldack.de/github-runner:1` is the tag).
3. Push 2: add `deployment.yaml` with that digest + add it to the app `kustomization.yaml`. Flux applies it, the runner registers, and this push also triggers the workflow (the end-to-end proof).

The reconciliation of children that `kubectl wait` checks may legitimately take minutes on the first sweep; that is the same work the operator used to trigger by hand.

## Critical files & anchors

- `kubernetes/infrastructure/home/image-builds/llama-p100.yaml` — the ConfigMap-referenced-Dockerfile Job to copy for structure (certs/docker volumes, nodeSelector, `--local=dockerfile` + `dockerfilekey` form).
- `kubernetes/infrastructure/home/image-builds/kustomization.yaml` — `configMapGenerator` shape, `disableNameSuffixHash`, the `substitute: disabled` annotation.
- `kubernetes/infrastructure/home/agentgateway/cilium-allowlist.yaml` — the exact egress literal style for DNS, `kube-apiserver`, the two `toCIDRSet` blocks and `world`.
- `kubernetes/infrastructure/home/hermes/namespace.yaml` — the `restricted` PSS label triple to copy.
- `kubernetes/clusters/home/media.yaml` + `kubernetes/clusters/home/kustomization.yaml` — per-app Kustomization shape and the registry that must list the new file.

## Verification

Prerequisites: `task kubeconfig:home:export` (gives `kubeconfig-home.yaml`) and an authenticated `gh` for `ngoldack/homelab`. All `kubectl` calls below use `--kubeconfig kubeconfig-home.yaml`.

1. Local gate before each push: `task check` → exit 0 (it renders every `kubernetes/**/kustomization.yaml` via `git ls-files`, including the new app dir, and decrypts every SOPS file).
2. Render the new trees: `kustomize build kubernetes/infrastructure/home/github-runner` → Role/RoleBinding carry `namespace: flux-system`, no Deployment in push 1; `kustomize build kubernetes/clusters/home` → contains `github-runner.yaml`.
3. Build ran: `kubectl -n buildkit get job build-github-runner-1 -o jsonpath='{.status.succeeded}'` → `1` (and `kubectl -n buildkit logs job/build-github-runner-1` shows `exporting manifest sha256:<digest>`). The digest in the Job log is the one written into `deployment.yaml`.
4. Runner registered and online:
   - `kubectl -n github-runner get deploy,pods` → `1/1`, pod `Running`.
   - `kubectl -n github-runner logs deploy/github-runner --tail=40` → `Runner successfully added` and `Listening for Jobs`, no 403/registration errors.
   - `kubectl -n github-runner get pod -l app.kubernetes.io/name=github-runner -o jsonpath='{.items[0].status.containerStatuses[0].imageID}'` → `registry.ngoldack.de/github-runner@sha256:<pinned digest>`.
   - `gh api repos/ngoldack/homelab/actions/runners --jq '.runners[] | {name,status,busy,labels:[.labels[].name]}'` → exactly one runner, `"status":"online"`, `"busy":false`, labels include `self-hosted` and `homelab`.
5. RBAC is scoped, not cluster-admin:
   - `kubectl auth can-i --as=system:serviceaccount:github-runner:ci-runner patch kustomizations.kustomize.toolkit.fluxcd.io -n flux-system` → `yes`
   - `kubectl auth can-i --as=system:serviceaccount:github-runner:ci-runner patch kustomizations.kustomize.toolkit.fluxcd.io -n media` → `no`
   - `kubectl auth can-i --as=system:serviceaccount:github-runner:ci-runner create deployments -n github-runner` → `no`
6. End-to-end, the actual acceptance test: push 2 triggers the workflow.
   - `gh run list --workflow=flux-reconcile.yml --limit 3` → the push-2 run is `completed`/`success`; `gh run view <id> --log` shows the final `flux get kustomizations -n flux-system` table with every row `True`.
   - Independent proof that CI did the work rather than the 10m poller: `kubectl get kustomizations.kustomize.toolkit.fluxcd.io -n flux-system -o custom-columns=NAME:.metadata.name,REV:.status.lastAppliedRevision,HANDLED:.status.lastHandledReconcileAt` → every `REV` equals the push-2 commit revision, and `HANDLED` timestamps fall inside the workflow run window.
7. Manual-dispatch path: `gh workflow list` → the `flux reconcile` workflow is registered, then `gh workflow run flux-reconcile.yml` and `gh run list --workflow=flux-reconcile.yml --limit 1` → a new `success` run on the self-hosted runner.
8. Ephemeral re-registration loop: `kubectl -n github-runner delete pod -l app.kubernetes.io/name=github-runner` → the replacement pod registers again, `gh api …/actions/runners` still shows exactly one online runner (the old registration is replaced because the name matches and `--replace` is passed).

## Assumptions & contingencies

- **PAT is user-supplied.** Ask the user for it before creating `secret.sops.yaml`; scope `repo` (classic) or `Administration: write` (fine-grained). A GitHub App instead of a PAT only changes that Secret (app-id/installation-id/private-key plus a JWT→installation-token exchange before the registration-token call) — nothing else in the plan moves.
- **403 from the registration-token endpoint** → the token lacks `Administration`/`repo`; symptom is pod `CrashLoopBackOff` with `no registration token returned by GitHub` in the logs. Fix the Secret, delete the pod.
- **`COPY --from=flux /usr/local/bin/flux` fails** → use the checksum-pinned tarball fallback in Step 1; the build failure is loud and nothing else is affected.
- **Base image uid is not 1001** → the Dockerfile's `chown -R 1001:1001 /home/runner` + `USER 1001:1001` makes 1001 authoritative, and the pod pins the same numeric uid; if PSA still rejects the pod, the reason appears in `kubectl -n github-runner get events`.
- **`kubectl wait --all --for=condition=Ready` on a CRD** → if the installed kubectl cannot do this, replace the step with an explicit poll: `kubectl get kustomizations -n flux-system -o jsonpath='{range .items[?(@.status.conditions[?(@.type=="Ready")].status!="True")]}{.metadata.name} {end}'` inside a bounded retry loop, failing after 15m. The contract (CI red when reconciliation does not converge) is unchanged.
- **A pre-existing broken Kustomization makes the workflow red.** Intended: the reconcile steps already ran, so the failure is surfaced rather than hidden; the wait step is the only part that can fail this way.
- **`task check` stays a local pre-push gate.** Running it inside CI would require the whole Taskfile toolchain (tofu, kustomize, sops, yamllint, helm) baked into the runner image; that is not part of this change.
