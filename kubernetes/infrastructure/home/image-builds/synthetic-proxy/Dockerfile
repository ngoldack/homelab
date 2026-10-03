# synthetic-proxy — the per-key health/quota guard in front of Synthetic
# (image-builds/synthetic-proxy/go/, package syntheticproxy, stdlib only).
# The binary fronts api.synthetic.new for the agentgateway data plane: it reads
# the Authorization header the gateway injects, keys per-caller state on a
# SHA-256 of it (never the raw key), and answers 429/503 when Synthetic's
# quota endpoint says the key is rate-limited or the upstream is down — which is
# exactly what the gateway's eviction policy acts on.
#
# WHY golang:1.27-alpine — same Go line and build flags as the sibling
# hermes-egress-guard build. The module declares `go 1.27`;
# GOTOOLCHAIN=local keeps the build resolving entirely from the pinned public
# tags below. CGO_ENABLED=0 yields a static binary, so the runtime layer needs
# no libc beyond busybox.
#
# WHY alpine:3.21 — the binary is stdlib-only (no interpreter, no venv), so the
# runtime is just the binary plus a minimal userland. Explicit tag, never
# :latest.
#
# WHY the build context is the REPO ROOT: the COPY paths below are
# repo-root-relative, the convention every Dockerfile in image-builds/ follows
# (the BuildKit Job passes the repo at a pinned commit as --opt=context).
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src/go

# The module + the library package. The *_test.go sibling is deliberately NOT
# copied: it guards the verdict logic on a developer machine and is not built
# here (the same rule the hermes-egress-guard build follows).
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/go.mod ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/config.go ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/state.go ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/quota.go ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/health.go ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/http.go ./
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/metrics.go ./
# The entrypoint: package main importing the root package by module path.
COPY kubernetes/infrastructure/home/image-builds/synthetic-proxy/go/cmd/synthetic-proxy/main.go ./cmd/synthetic-proxy/main.go

# A single static binary; -trimpath and -s -w keep build paths and symbols out
# of the runtime layer.
RUN go build -trimpath -ldflags="-s -w" -o /out/synthetic-proxy ./cmd/synthetic-proxy

# Build-time gate — the artifact must be a real runnable program, and the
# fail-fast configuration contract must hold: an unparseable env value has to
# exit non-zero (that is the CrashLoopBackOff contract a bad deployment relies
# on). A library archive, or a binary that silently accepts a bad LPORT, fails
# the build here instead of shipping.
RUN set -e; \
    if ! [ -x /out/synthetic-proxy ]; then echo "artifact is not executable"; exit 1; fi; \
    if LPORT=banana /out/synthetic-proxy >/dev/null 2>&1; then echo "bad LPORT exited 0; fail-fast config broken"; exit 1; fi; \
    if UNHEALTHY_AFTER=banana /out/synthetic-proxy >/dev/null 2>&1; then echo "bad UNHEALTHY_AFTER exited 0; fail-fast config broken"; exit 1; fi; \
    echo "synthetic-proxy: runnable program with fail-fast config"

# PSA-restricted runtime shape: unprivileged uid, no writes to the rootfs
# (readOnlyRootFilesystem + a /tmp emptyDir in the Deployment). The process runs
# as 10001 with no capabilities, so it cannot bind <1024, touch the host, or
# persist anything into the image.
FROM alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507
RUN addgroup -g 10001 sp && adduser -D -H -u 10001 -G sp -s /sbin/nologin sp
COPY --from=build /out/synthetic-proxy /synthetic-proxy
USER 10001:10001
ENTRYPOINT ["/synthetic-proxy"]
