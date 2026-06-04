package main

import (
	"context"

	"dagger/homelab/internal/dagger"
)

// stateContainer runs in tofu/ with the SOPS age key available and the state
// passphrase exported. It reads the committed, encrypted state but needs no
// network access, so it does not join the NetBird mesh.
func (m *Homelab) stateContainer(sopsAgeKey *dagger.Secret) *dagger.Container {
	return m.liveBase().
		WithWorkdir("/src/tofu").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey)
}

// stateInitPrologue decrypts the passphrase and runs `tofu init` (no backend
// reconfigure), suitable for read-only state access.
const stateInitPrologue = `
	passphrase="$(sops -d secret.sops.yaml | yq '.state_encryption_passphrase')"
	export TF_ENCRYPTION="key_provider \"pbkdf2\" \"statekey\" { passphrase = \"${passphrase}\" }"
	tofu init -backend=false -lockfile=readonly >/dev/null
`

// TofuOutput prints all OpenTofu outputs (sensitive values redacted by tofu).
func (m *Homelab) TofuOutput(
	ctx context.Context,
	// The SOPS age private key (env://SOPS_AGE_KEY).
	sopsAgeKey *dagger.Secret,
) (string, error) {
	return m.stateContainer(sopsAgeKey).
		WithExec([]string{"sh", "-euc", stateInitPrologue + "tofu output"}).
		Stdout(ctx)
}

// Kubeconfig extracts the cluster kubeconfig from OpenTofu state as a file.
func (m *Homelab) Kubeconfig(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) *dagger.File {
	return m.stateContainer(sopsAgeKey).
		WithExec([]string{"sh", "-euc", stateInitPrologue + "tofu output -raw kubeconfig > /tmp/kubeconfig.yaml"}).
		File("/tmp/kubeconfig.yaml")
}

// Talosconfig extracts the Talos client config from OpenTofu state as a file.
func (m *Homelab) Talosconfig(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) *dagger.File {
	return m.stateContainer(sopsAgeKey).
		WithExec([]string{"sh", "-euc", stateInitPrologue + "tofu output -raw talosconfig > /tmp/talosconfig.yaml"}).
		File("/tmp/talosconfig.yaml")
}

// SecretsDecrypt decrypts a single SOPS file and returns its plaintext. The
// path is relative to the repository root, e.g. "tofu/secret.sops.yaml".
func (m *Homelab) SecretsDecrypt(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
	// Path to the *.sops.yaml file, relative to the repo root.
	path string,
) (string, error) {
	return m.liveBase().
		WithWorkdir("/src").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey).
		WithExec([]string{"sops", "-d", path}).
		Stdout(ctx)
}
