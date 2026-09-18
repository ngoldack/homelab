# hermes-egress-guard — the ONE image behind both guard services (plan Units
# 3.2/3.3). Sources are stdlib-only Python (no pip install, no lock file): the
# package is copied verbatim and each service selects itself by the MODULE
# PATH passed as argv (egress_guard.authorizer | egress_guard.reaper).
#
# Base: pinned CPython 3.12 slim (multi-arch index digest resolved from the
# Docker Hub registry API, 2026-09-18 — same method the
# hermes-agent-sandbox-plugin Dockerfile documents for its base). WHY slim:
# the guard installs NOTHING, so python3 + the vendored stdlib is the whole
# runtime; slim keeps the attack surface and pull cost minimal.
# WHY buildkit runs with the REPO ROOT as context: COPY paths below are
# repo-root-relative (same convention as every Dockerfile in image-builds/).
FROM docker.io/library/python:3.12-slim@sha256:78387bc3881b8273120a12ebe6c1ab22b018ccc2c9adf565ae1ac9b536e184ea

# PSA-restricted runtime shape: unprivileged uid, no writes to the rootfs
# (read-only rootfs + /tmp emptyDir in the Deployments). The process runs as
# 10001 with no caps, so the guard cannot bind <1024 or touch the host.
RUN groupadd -g 10001 guard && useradd -u 10001 -g 10001 -m -s /usr/sbin/nologin guard

# The whole package: stdlib only, every runtime import must resolve from
# CPython itself (egress_guard/__init__.py contract).
COPY kubernetes/infrastructure/home/image-builds/hermes-egress-guard/src/ /opt/egress-guard/

# Build-time gate: every module imports under the runtime interpreter and the
# package is executable in both roles (config fail-fast surfaces as a
# non-zero exit, which is exactly the CrashLoopBackOff contract the
# Deployments rely on for a misconfigured guard).
RUN python3 -c "import sys; sys.path.insert(0, '/opt/egress-guard'); import egress_guard, egress_guard.authorizer, egress_guard.reaper, egress_guard.k8sapi, egress_guard.auth, egress_guard.policy, egress_guard.http, egress_guard.metrics, egress_guard.config; print('egress_guard %s: all modules importable (both roles)' % egress_guard.__version__)"

ENV PYTHONPATH=/opt/egress-guard
ENV EGRESS_LISTEN_PORT=8080

# Entrypoint: the Deployment passes the role MODULE as argv
# (args: [egress_guard.authorizer] | [egress_guard.reaper]); python -c execs
# that module's main() — both roles define main(argv=None). NOTE the dispatch
# form: a bare `python3 -c` with the module name as argv would execute that
# NAME as source (NameError); the -c script below imports it and calls main().
# No default module: a missing/wrong argv fails loudly (IndexError/ImportError)
# instead of guessing.
USER 10001
WORKDIR /opt/egress-guard
ENTRYPOINT ["python3", "-c", "import importlib, sys; sys.exit(importlib.import_module(sys.argv[1]).main())"]
CMD []
