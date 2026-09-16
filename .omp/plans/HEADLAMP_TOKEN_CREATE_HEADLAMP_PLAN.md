# Create Headlamp login token (`headlamp-token`)

## Context
User needs a fresh Kubernetes bearer token for the Headlamp UI login (Headlamp at headlamp.ngoldack.de sits behind authentik edge auth; the cluster token is entered in Headlamp's own login page). Principal: ServiceAccount `headlamp-headlamp`, namespace `headlamp`. Prior convention: 24h validity, token stored outside the repo at `~/headlamp.token`.

## Approach
1. **Point kubectl at the homelab cluster.** Default context `kind-local-dev` targets a dead local kind cluster (verified this session: connection refused to 127.0.0.1:53663). Use the repo-root kubeconfig `kubeconfig-home.yaml` via `--kubeconfig` on every command. If the file is missing/stale, regenerate first with `task kubeconfig:home:export` from the repo root.
2. **Confirm the principal exists (read-only).** `kubectl --kubeconfig kubeconfig-home.yaml get serviceaccount headlamp-headlamp -n headlamp` — must return the SA, else stop and report.
3. **Mint the token.** `kubectl --kubeconfig kubeconfig-home.yaml create token headlamp-headlamp -n headlamp --duration=24h` (TokenRequest; nothing persisted in-cluster, expires in 24h).
4. **Store and stage it securely.** Write the raw token to `~/headlamp.token` with mode 600 (no-secrets-in-tree rule — never inside the repo), copy it to the clipboard via `pbcopy`, and print only a masked preview (first 8 chars) plus the expiry timestamp to the chat — never the full token.
5. **Verify the token authenticates.** `kubectl --kubeconfig kubeconfig-home.yaml --token="$(cat ~/headlamp.token)" get namespaces` — expected: succeeds and lists the cluster's namespaces (prior session: 28 namespaces). A `401`/`Unauthorized` here means the mint failed; re-run step 3 before reporting failure.

## Critical files & anchors
- `kubeconfig-home.yaml` (repo root) — homelab cluster credentials; gitignored, may need regeneration via `task kubeconfig:home:export`.
- `~/headlamp.token` — output location, mode 600, overwrite in place.

## Verification
- `stat -f '%Lp' ~/headlamp.token` → `600`.
- `kubectl --kubeconfig kubeconfig-home.yaml --token="$(cat ~/headlamp.token)" get namespaces` → table of ~28 namespaces, no auth error.
- User pastes the clipboard token into the Headlamp login at headlamp.ngoldack.de (after authentik login) → dashboard loads with cluster data.

## Assumptions & contingencies
- 24h duration matches prior convention; say so when handing off the expiry time.
- If `kubeconfig-home.yaml` is missing or stale: run `task kubeconfig:home:export`, then retry; if export fails, report the task error verbatim and stop.
- If the SA is absent (renamed/removed since last session): stop and report — do not mint a token for a different principal without asking.
