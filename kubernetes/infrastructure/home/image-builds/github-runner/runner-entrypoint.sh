#!/usr/bin/env bash
# Registers this pod as a repo-scoped GitHub Actions runner and runs exactly
# one job (--ephemeral): no long-lived runner registration and no state that
# outlives a job. When the job ends, the runner deregisters itself and exits 0,
# so the kubelet restarts the container, which registers a fresh runner.
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
exec ./run.sh
