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
# FORBIDDEN CONTENT — keep absent: kubectl, talosctl, tofu/terraform, pvesh,
# sshd, cloud CLIs, docker CLI/socket, git credentials, provider credentials.
# Sandboxes get no cluster or management-plane access path and no long-lived
# credentials by construction. /workspace and /tmp are the ONLY writable
# directories (emptyDir mounts the SandboxTemplate creates at runtime).
#
# No HEALTHCHECK: Kubernetes probes observe readiness (controller/claim path).

# ---- Stage 1: build sandboxd from the pinned upstream tag ----
FROM golang:1.26.5-bookworm AS sandboxd

# Clone the exact release the installed CRDs and extensions came from: the
# v1beta1 API surface must match what sandbox-router and the SDK speak.
RUN git clone --depth 1 --branch v1.0.2 \
    https://github.com/kubernetes-sigs/agent-sandbox.git /src
WORKDIR /src

# Predownload dependencies from the root go.mod before building (same split
# upstream's Dockerfile uses) so subsequent rebuilds skip the network.
RUN go mod download

# CGO_ENABLED=0: plain `go build` of just the sandboxd main package, no CGO
# orchestration — verbatim the upstream build contract for this binary.
RUN CGO_ENABLED=0 go build -o /out/sandboxd \
    ./packages/sandboxd/cmd/sandboxd

# ---- Stage 2: minimal runtime ----
FROM debian:bookworm-slim

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