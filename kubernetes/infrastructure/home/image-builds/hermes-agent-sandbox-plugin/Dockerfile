# hermes-agent-sandbox — Hermes Agent image with the agent_sandbox terminal backend.
#
# Base: the pinned Hermes release image (docker.io/nousresearch/hermes-agent
# v2026.9.7). The tag was verified against the Docker Hub tags API before this
# Dockerfile was authored; the exact tag digest is recorded in the build Job
# (the repo's buildkit Job later builds this path with --opt=filename=... so
# this file stays a plain FROM-referencing Dockerfile).
#
# WHY this structure:
#   * We install the agent-sandbox python SDK and the plugin package so the
#     plugin's lazy imports resolve at runtime without relying on PYTHONPATH.
#   * We place the plugin under $HERMES_HOME/plugins/agent_sandbox/ (default
#     /opt/data/plugins/...) which is the directory-discovery root Hermes
#     scans for plugin.yaml manifests (hermes_cli/plugins.py scan_directory).
#   * /opt/data/config.yaml is shipped ONLY as an initial template enabling
#     plugins:[agent_sandbox] + terminal backend agent_sandbox; a PVC mounts
#     over /opt/data at runtime and must not be clobbered, so a .template is
#     materialized once by the gateway setup step.
#   * We run as the unprivileged hermes uid (10000), matching the gateway
#     StatefulSet securityContext (runAsNonRoot).

FROM docker.io/nousresearch/hermes-agent:v2026.9.7

USER root

# ca-certificates: register the homelab CA bundle so the gateway talks to the
# internal router/registry over TLS the base image does not trust.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install the pinned agent-sandbox python SDK (verified on PyPI:
# k8s-agent-sandbox 1.0.2) and the plugin package itself.
# NOTE: buildkit runs this Dockerfile with the REPO ROOT as context (the
# --opt=filename path is repo-relative), so every COPY source below is
# repo-root-relative, mirroring how the runtime image's Dockerfile is built.
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/pyproject.toml /tmp/plugin-build/
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/src /tmp/plugin-build/src/
RUN pip install --no-cache-dir "k8s-agent-sandbox==1.0.2" \
    && pip install --no-cache-dir /tmp/plugin-build

# Plugin discovery root: place the package under $HERMES_HOME/plugins/agent_sandbox/
# then copy plugin.yaml next to it (Hermes scans this directory for a manifest).
RUN mkdir -p /opt/data/plugins/agent_sandbox \
    && cp -r /tmp/plugin-build/src/hermes_agent_sandbox /opt/data/plugins/agent_sandbox/
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/plugin.yaml /opt/data/plugins/agent_sandbox/plugin.yaml

# Initial config template (never clobbered at runtime — a PVC overlays /opt/data).
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/config-tpl.yaml /opt/data/config.yaml.template

USER hermes

# ENTRYPOINT/CMD intentionally unchanged: the base image's s6 overlay owns
# process supervision and plugin loading.