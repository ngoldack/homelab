package main

import (
	"context"

	"dagger/homelab/internal/dagger"
)

// initState injects the state-encryption passphrase and runs a backend-less
// `tofu init`, suitable for read-only state access. It reuses tofuContainer
// (tofu/ workdir + SOPS age key); reading the committed encrypted state needs
// no network access.
func (m *Homelab) initState(ctx context.Context, sopsAgeKey *dagger.Secret) (*dagger.Container, error) {
	c := m.tofuContainer(sopsAgeKey)
	enc, err := m.stateEncryption(ctx, c)
	if err != nil {
		return nil, err
	}
	return c.
		WithSecretVariable("TF_ENCRYPTION", enc).
		WithExec([]string{"tofu", "init", "-backend=false", "-lockfile=readonly"}), nil
}

// TofuOutput prints all OpenTofu outputs (sensitive values redacted by tofu).
func (m *Homelab) TofuOutput(
	ctx context.Context,
	// The SOPS age private key (env://SOPS_AGE_KEY).
	sopsAgeKey *dagger.Secret,
) (string, error) {
	c, err := m.initState(ctx, sopsAgeKey)
	if err != nil {
		return "", err
	}
	return c.WithExec([]string{"tofu", "output"}).Stdout(ctx)
}

// Kubeconfig extracts the cluster kubeconfig from OpenTofu state as a file.
func (m *Homelab) Kubeconfig(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) (*dagger.File, error) {
	c, err := m.initState(ctx, sopsAgeKey)
	if err != nil {
		return nil, err
	}
	return c.
		WithExec([]string{"sh", "-c", "tofu output -raw kubeconfig > /tmp/kubeconfig.yaml"}).
		File("/tmp/kubeconfig.yaml"), nil
}

// Talosconfig extracts the Talos client config from OpenTofu state as a file.
func (m *Homelab) Talosconfig(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) (*dagger.File, error) {
	c, err := m.initState(ctx, sopsAgeKey)
	if err != nil {
		return nil, err
	}
	return c.
		WithExec([]string{"sh", "-c", "tofu output -raw talosconfig > /tmp/talosconfig.yaml"}).
		File("/tmp/talosconfig.yaml"), nil
}

// SecretsDecrypt decrypts a single SOPS file and returns its plaintext. The
// path is relative to the repository root, e.g. "tofu/secret.sops.yaml".
func (m *Homelab) SecretsDecrypt(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
	// Path to the *.sops.yaml file, relative to the repo root.
	path string,
) (string, error) {
	return m.withTools(m.base(), "sops").
		WithWorkdir("/src").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey).
		WithExec([]string{"sops", "-d", path}).
		Stdout(ctx)
}
