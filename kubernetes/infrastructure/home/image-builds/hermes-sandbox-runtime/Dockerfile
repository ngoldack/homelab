# hermes-sandbox-runtime — the runtime image for hermes-go sandbox pods.
#
# WHY: sandboxd (agent-sandbox v1.0.2, KEP-539.2 runtime standardization) is
# the daemon every sandbox pod runs. It serves the hybrid runtime API a task
# session needs — gRPC ProcessService on 9090 (streaming process execution)
# and the REST FilesystemService on 8080 (files + probes), both bound to
# 0.0.0.0 so the sandbox-router can reach them. The ProcessService executes
# commands inside THIS image's filesystem, so every tool a coding session uses
# must be layered in here.
#
# GO VERSION — checked against the module, not guessed: the module owning the
# sandboxd main is packages/sandboxd (it has no go.mod of its own); its build
# contract is upstream packages/sandboxd/Dockerfile, which pins golang:1.26.5
# (repo root go.mod agrees: `go 1.26.0` / `toolchain go1.26.4`). Build with
# the 1.26.5-BOOKWORM variant: same Go version as upstream, and a glibc ABI
# that matches the bookworm-slim runtime stage (the plain 1.26.5 tag is
# trixie-based and would build against a newer glibc).
#
# BASE PINS: both FROM stages are pinned by their multi-arch index digest
# (resolved and cross-checked 2026-09-17: `skopeo inspect --raw <ref> |
# sha256sum` vs. the registry's Docker-Content-Digest header). The build Job
# records the digest of the image this Dockerfile PRODUCES, so a mutable base
# tag must never reach the build.
#
# VERSION STAMPING: the build links upstream's own -ldflags for
# sigs.k8s.io/agent-sandbox/internal/version.*, so sandboxd's startup log and
# `sandboxd --version` report the pinned source instead of "unknown".
#
# FORBIDDEN CONTENT — keep absent: kubectl, talosctl, tofu/terraform, pvesh,
# sshd, cloud CLIs, docker CLI/socket, git credentials, provider credentials.
# Sandboxes get no cluster or management-plane access path and no long-lived
# credentials by construction. /workspace and /tmp are the ONLY writable
# directories (emptyDir mounts the SandboxTemplate creates at runtime).
#
# No HEALTHCHECK: Kubernetes probes observe readiness (controller/claim path).

# ---- Stage 1: build sandboxd from the pinned upstream commit ----
FROM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS sandboxd

# Check out the exact release the installed CRDs and extensions came from: the
# v1beta1 API surface must match what sandbox-router and the SDK speak.
# Pinned by COMMIT, not by tag — v1.0.2 resolves to
# 9a85153590e54cb980f3241f9e7a9228449412c9 (resolved 2026-09-17 with
# `git ls-remote https://github.com/kubernetes-sigs/agent-sandbox 'refs/tags/v1.0.2^{}'`).
# The fetch refspec names the tag so the shallow fetch has a ref to negotiate
# over, but the checkout is by SHA: a moved upstream tag then fails the build
# loudly instead of silently building different code.
ARG GIT_VERSION=v1.0.2
ARG GIT_SHA=9a85153590e54cb980f3241f9e7a9228449412c9
# Stamped build date: the pinned commit's date keeps two builds of this context
# byte-identical (no wall-clock in the binary). Pass
# `--opt=build-arg:BUILD_DATE=...` when per-build provenance is wanted instead.
ARG BUILD_DATE=2026-09-10T08:36:34Z
RUN git init -q /src \
    && git -C /src remote add origin https://github.com/kubernetes-sigs/agent-sandbox.git \
    && git -C /src fetch -q --depth 1 origin "refs/tags/${GIT_VERSION}:refs/tags/${GIT_VERSION}" \
    && git -C /src checkout -q --detach "${GIT_SHA}"
WORKDIR /src

# Predownload dependencies from the root go.mod before building (same split
# upstream's packages/sandboxd/Dockerfile uses) so subsequent rebuilds skip
# the network.
RUN go mod download

# CGO_ENABLED=0: plain `go build` of just the sandboxd main package, no CGO
# orchestration — verbatim the upstream build contract for this binary. The
# -ldflags mirror upstream's stamping of sigs.k8s.io/agent-sandbox/internal/
# version.{gitVersion,gitSHA,buildDate}, which sandboxd logs at startup and
# prints for `sandboxd --version`.
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X sigs.k8s.io/agent-sandbox/internal/version.gitVersion=${GIT_VERSION} -X sigs.k8s.io/agent-sandbox/internal/version.gitSHA=${GIT_SHA} -X sigs.k8s.io/agent-sandbox/internal/version.buildDate=${BUILD_DATE}" \
    -o /out/sandboxd ./packages/sandboxd/cmd/sandboxd

# ---- Stage 2: minimal runtime ----
# debian:bookworm-slim pinned by its index digest (resolved 2026-09-17 as noted
# at the top of this file).
FROM debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171

# procps (ps/kill) is required by the ProcessService for process management;
# git/curl/jq/ripgrep/coreutils are the command surface of a coding session;
# util-linux and ca-certificates keep tooling and TLS reproducible.
# --no-install-recommends keeps both attack surface and size down.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates procps curl jq ripgrep git coreutils util-linux \
    && rm -rf /var/lib/apt/lists/*

# The ENTIRE Go toolchain, copied from the pinned golang stage — deliberately
# NOT golang:latest, whose newer compiler would rebuild dependencies
# differently than the version sandboxd's go.sum was verified against. This
# copy is what makes `go test ./...` work inside the sandbox.
COPY --from=sandboxd /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH

COPY --from=sandboxd /out/sandboxd /usr/local/bin/sandboxd

# sandboxd runs as UID/GID 10001 (matching the template's runAsUser/fsGroup);
# /workspace is its --root-dir default and the only writable mount.
# HOME points at /tmp (a writable emptyDir at runtime): go/python toolchains
# cache under $HOME/.cache, and /home/sandbox is read-only in this image —
# without this, `go test` dies with "mkdir /home/sandbox: read-only file
# system" on the first build-cache write.
RUN groupadd --gid 10001 sandbox \
    && useradd --uid 10001 --gid 10001 --no-create-home \
        --shell /bin/sh sandbox \
    && mkdir --parents /workspace \
    && chown 10001:10001 /workspace

ENV HOME=/tmp

USER sandbox

# Informational only: 8080 REST FilesystemService, 9090 gRPC ProcessService.
EXPOSE 8080 9090

ENTRYPOINT ["sandboxd"]