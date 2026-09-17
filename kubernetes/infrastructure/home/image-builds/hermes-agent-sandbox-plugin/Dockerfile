# hermes-agent-sandbox — Hermes Agent image with the agent_sandbox terminal backend.
#
# Base: the pinned Hermes release image. The mutable tag is pinned by the
# multi-arch index digest below — the build Job records the digest of the
# image this Dockerfile PRODUCES, not of its base, so the base pin must live
# here (the repo's buildkit Job builds this path with --opt=filename=... and
# the repo root as context). Digest resolved 2026-09-17, identical from two
# methods:
#   skopeo inspect --raw docker://docker.io/nousresearch/hermes-agent:v2026.9.7 | sha256sum
#   Docker Hub v2 manifests API Docker-Content-Digest header for the same tag
#
# WHY this structure:
#   * We install the agent-sandbox python SDK (grpc extra) and the plugin
#     package into a venv the gateway interpreter imports, so the plugin's
#     lazy imports resolve at runtime without an env PYTHONPATH or any file
#     placement under $HERMES_HOME.
#   * Plugin discovery is by ENTRY POINT (group hermes_agent.plugins ->
#     hermes_agent_sandbox.provider, see pyproject.toml), which reads the
#     dist-info off the gateway interpreter's sys.path.
#   * /opt/data/config.yaml (plugins+terminal backend seed, template) is the
#     gateway pod's initContainer concern (hermes/statefulset.yaml), not this
#     image's: a PVC mounts over /opt/data at runtime.
#   * We run as the unprivileged hermes uid (10000), matching the gateway
#     StatefulSet securityContext (runAsNonRoot).

FROM docker.io/nousresearch/hermes-agent:v2026.9.7@sha256:63bfb6d732f49a55d453e801057273785cc61e0f6ee43db3fa2f2a79846301b7

USER root

# ca-certificates: ensure the TLS trust store is present for the gateway's own
# tooling (git/curl). No homelab CA is layered in here.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install the pinned agent-sandbox python SDK (with its grpc extra) and the
# plugin package into a SEPARATE plain venv, NOT the base image's uv-managed
# /opt/hermes/.venv: uv pip refuses to resolve against that venv (uv-managed
# venv + project coupling in /opt/hermes/pyproject.toml) and the base image has
# no pip on PATH. python3-venv is guaranteed by the base Dockerfile; the
# dependency set is pinned in this directory's pyproject.toml (single
# authority). README.md is copied because pyproject declares
# `readme = "README.md"`.
# NOTE: buildkit runs this Dockerfile with the REPO ROOT as context, so COPY
# sources below are repo-root-relative.
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/pyproject.toml /tmp/plugin-build/
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/README.md /tmp/plugin-build/README.md
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/src /tmp/plugin-build/src/
# langfuse==4.15.4 — the observability/langfuse plugin the base image already
# ships (plugins/observability/langfuse) imports `Langfuse` +
# `propagate_attributes` and calls `client.create_trace_id` /
# `start_as_current_observation` / `flush` / `shutdown`: the v4 SDK surface.
# Without the SDK the plugin's hooks fail open (no trace, no error), which is
# why it is installed here rather than lazily: the runtime venv is read-only and
# the container runs with HERMES_DISABLE_LAZY_INSTALLS=1.
# 4.15.4 = latest release on PyPI at 2026-09-17, verified against the shipped
# plugin's call sites under this image's interpreter before pinning.
RUN python3 -m venv /opt/hermes/plugin-venv \
    && /opt/hermes/plugin-venv/bin/pip install --no-cache-dir \
        "k8s-agent-sandbox[grpc]==1.0.2" "langfuse==4.15.4" /tmp/plugin-build

# Make the plugin venv importable by the interpreter that runs the gateway,
# without hardcoding a python3.X path. `python3` IS that interpreter: the base
# image's PATH starts with /opt/hermes/bin:/opt/hermes/.venv/bin (its config
# Env), and /opt/hermes/bin/hermes — the shim entrypoint.sh execs — runs the
# uv-managed venv's python. Both site-packages trees are therefore derived from
# the interpreters themselves at build time.
# The venv's purelib gets a .pth that APPENDS the plugin venv's packages
# (site.py appends .pth path entries), so the base image's own pinned
# dependencies keep precedence — an ENV PYTHONPATH entry would be prepended and
# could shadow them. Interpreter parity is asserted because installed wheels
# are interpreter-ABI specific.
RUN set -eu; \
    hermes_site="$(python3 -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')"; \
    plugin_site="$(/opt/hermes/plugin-venv/bin/python3 -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')"; \
    test "$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')" = \
         "$(/opt/hermes/plugin-venv/bin/python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"; \
    printf '%s\n' "$plugin_site" > "$hermes_site/agent_sandbox_plugin.pth"; \
    echo "agent_sandbox: $plugin_site appended via $hermes_site/agent_sandbox_plugin.pth for $(python3 -c 'import sys; print(sys.executable)')"

# Build-time gate: the plugin must be importable AND discoverable by that same
# interpreter (through the .pth, with no PYTHONPATH), otherwise a package
# rename or an entry-point edit would silently disable the agent_sandbox
# terminal backend at runtime. `grpc` proves the SDK's grpc extra resolved to
# ABI-compatible wheels for this interpreter.
RUN python3 -c 'import importlib.metadata as md, grpc, hermes_agent_sandbox.provider as p; assert callable(getattr(p, "register", None)), "hermes_agent_sandbox.provider.register is not callable"; eps = {e.value for e in md.entry_points(group="hermes_agent.plugins")}; assert "hermes_agent_sandbox.provider" in eps, "entry point missing; found %r" % sorted(eps); print("agent_sandbox: provider importable + entry point discoverable")'

# Build-time gate (observability): same interpreter, no PYTHONPATH. `langfuse`
# resolves through the .pth above; the version and the four call sites the
# shipped plugin uses are asserted so an upgraded/ABI-broken SDK fails the build
# instead of silently disabling tracing at runtime (the plugin fails open).
RUN python3 -c 'import sys, langfuse; from langfuse import Langfuse, propagate_attributes; assert langfuse.__version__ == "4.15.4", "unexpected langfuse %s" % langfuse.__version__; assert callable(propagate_attributes), "propagate_attributes missing"; assert all(callable(getattr(Langfuse, a, None)) for a in ("create_trace_id", "start_as_current_observation", "flush", "shutdown")), "langfuse v4 client API surface incomplete"; print("langfuse %s: import + v4 API surface OK for %s" % (langfuse.__version__, sys.executable))'

# ENTRYPOINT override: run the gateway binary DIRECTLY instead of the base
# image's s6-overlay supervisor. s6's preinit demands /run owned by the
# container uid and cannot run as a non-root uid under PSA restricted;
# Kubernetes (StatefulSet restartPolicy) supervises each container instead.
# The wrapper is argv-transparent (entrypoint.sh): the gateway container
# passes `gateway run`, and the dashboard runs as its own container in the
# same pod, with its own probes.
# The hermes shim is the base image's own wrapper at /opt/hermes/bin/hermes.
# Installed while still root: /opt/hermes/bin is root-owned.
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/entrypoint.sh /opt/hermes/bin/agent-sandbox-entrypoint.sh
RUN chmod 0755 /opt/hermes/bin/agent-sandbox-entrypoint.sh
ENTRYPOINT ["/opt/hermes/bin/agent-sandbox-entrypoint.sh"]
CMD []

USER hermes
