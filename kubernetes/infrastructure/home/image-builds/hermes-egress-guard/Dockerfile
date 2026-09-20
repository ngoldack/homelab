# hermes-egress-guard — the ONE image behind both guard services (plan Units
# 3.2/3.3). This supersedes the Python build: the package was ported to Go
# (go/, package guard, stdlib only) and is compiled here into a SINGLE static
# binary, /guard. Each service selects itself by the FIRST argv
# (`authorizer` | `reaper`, dispatched in go/cmd/guard/main.go); a missing or
# unknown role exits non-zero, which is the CrashLoopBackOff contract the
# Deployments rely on for a misconfigured guard. There is NO EGRESS_ROLE env —
# the argv IS the selector.
#
# WHY golang:1.27-alpine — the cluster's Go line and the llama-kv-broker
# precedent (same build stage, same flags). The module declares `go 1.27`;
# GOTOOLCHAIN=local keeps the build from fetching any other toolchain, so the
# image resolves entirely from the pinned public tags below. CGO_ENABLED=0
# yields a static binary, so the runtime needs no libc beyond busybox.
#
# WHY alpine:3.21 — the binary is stdlib-only (no pip/venv/interpreter), so the
# runtime is just the binary plus a minimal userland; alpine keeps the attack
# surface and pull cost minimal. Explicit tag, never :latest.
#
# WHY buildkit runs with the REPO ROOT as context: the COPY paths below are
# repo-root-relative (same convention as every Dockerfile in image-builds/).
FROM golang:1.27-alpine AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src/go

# The module + the library package (package guard). The *_test.go siblings are
# deliberately NOT copied: they guard the port on a developer machine and are
# not built here (llama-kv-broker precedent).
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/go.mod ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/auth.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/authorizer.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/config.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/http.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/k8sapi.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/metrics.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/policy.go ./
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/reaper.go ./
# The entrypoint: package main importing the root package by module path
# (hermes-egress-guard) and dispatching AuthorizerMain / ReaperMain by argv[1].
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/go/cmd/guard/main.go ./cmd/guard/main.go

# A single static binary; -trimpath and -s -w keep build paths and symbols out
# of the runtime layer.
RUN go build -trimpath -ldflags="-s -w" -o /out/guard ./cmd/guard

# Build-time gate (replaces the Python image's import gate): the artifact must
# be a runnable program — an executable, not a library archive — and the argv
# contract must fail loudly on a missing/unknown role. A library archive would
# fail here instead of shipping.
RUN set -e; \
    if /out/guard >/dev/null 2>&1; then echo "guard with no role exited 0; argv contract broken"; exit 1; fi; \
    if /out/guard bogus-role >/dev/null 2>&1; then echo "guard with unknown role exited 0; argv contract broken"; exit 1; fi; \
    echo "guard: /out/guard is a runnable program that rejects a missing/unknown role"

# PSA-restricted runtime shape: unprivileged uid, no writes to the rootfs
# (readOnlyRootFilesystem + a /tmp emptyDir in the Deployments). The process
# runs as 10001 with no capabilities, so the guard cannot bind <1024, touch the
# host, or persist anything into the image.
FROM alpine:3.21
RUN addgroup -g 10001 guard && adduser -D -H -u 10001 -G guard -s /sbin/nologin guard
COPY --from=build /out/guard /guard
USER 10001:10001
# The Deployments pass the role as argv (`args: ["authorizer"]` /
# `["reaper"]`); no default role is compiled in, so a wrong or missing arg
# fails loudly at startup.
ENTRYPOINT ["/guard"]