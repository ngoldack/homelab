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

# Install the pinned agent-sandbox python SDK and the plugin package into a
# SEPARATE plain venv, NOT the base image's uv-managed /opt/hermes/.venv:
# uv pip refuses to resolve against that venv (uv-managed venv + project
# coupling in /opt/hermes/pyproject.toml) and the base image has no pip on
# PATH. python3-venv is guaranteed by the base Dockerfile. The plugin venv's
# site-packages is exposed to Hermes via PYTHONPATH below; entry-point
# discovery (hermes_agent.plugins) reads the dist-info off sys.path.
# NOTE: buildkit runs this Dockerfile with the REPO ROOT as context, so COPY
# sources below are repo-root-relative.
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/pyproject.toml /tmp/plugin-build/
COPY kubernetes/infrastructure/home/image-builds/hermes-agent-sandbox-plugin/src /tmp/plugin-build/src/
RUN python3 -m venv /opt/hermes/plugin-venv \
    && /opt/hermes/plugin-venv/bin/pip install --no-cache-dir \
        "k8s-agent-sandbox==1.0.2" /tmp/plugin-build

# Expose the plugin venv's packages to the Hermes process (same 3.13
# interpreter family; pure-python plugin + prebuilt wheels).
ENV PYTHONPATH=/opt/hermes/plugin-venv/lib/python3.13/site-packages

USER hermes

# ENTRYPOINT override: run the gateway binary DIRECTLY instead of the base
# image's s6-overlay supervisor. s6's preinit demands /run owned by the
# container uid and cannot run as a non-root uid under PSA restricted;
# Kubernetes (StatefulSet restartPolicy) supervises the single gateway
# process instead. The hermes shim is the base image's own wrapper at
# /opt/hermes/bin/hermes.
ENTRYPOINT ["/opt/hermes/bin/hermes"]
CMD ["gateway", "run"]