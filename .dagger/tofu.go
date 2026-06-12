package main

import (
	"context"
	"fmt"
	"strings"

	"dagger/homelab/internal/dagger"

	"gopkg.in/yaml.v3"
)

// liveBase extends the offline toolchain with the binaries needed to read and
// mutate live infrastructure: tofu, sops (decrypt secrets) and flux (reconcile).
//
// It does NOT manage network connectivity. The caller is responsible for
// establishing access to the private cluster network (e.g. a NetBird connect
// step before the `dagger call`, or a local mesh connection); the Dagger
// engine NATs outbound traffic through the host, so a host-side mesh
// connection is reachable from inside these containers.
func (m *Homelab) liveBase() *dagger.Container {
	return m.withTools(m.base(), "tofu", "sops", "flux")
}

// tofuContainer prepares a live container in tofu/ with the SOPS age key.
func (m *Homelab) tofuContainer(sopsAgeKey *dagger.Secret) *dagger.Container {
	return m.liveBase().
		WithWorkdir("/src/tofu").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey)
}

// stateEncryption decrypts tofu/secret.sops.yaml, extracts the state-encryption
// passphrase, and returns it as a Dagger secret holding the full TF_ENCRYPTION
// expression. Keeping it a secret means the passphrase is never interpolated
// into a shell string or exposed in a build layer.
func (m *Homelab) stateEncryption(ctx context.Context, c *dagger.Container) (*dagger.Secret, error) {
	decrypted, err := c.
		WithExec([]string{"sops", "-d", "secret.sops.yaml"}).
		Stdout(ctx)
	if err != nil {
		return nil, fmt.Errorf("decrypting tofu/secret.sops.yaml: %w", err)
	}

	var secrets struct {
		Passphrase string `yaml:"state_encryption_passphrase"`
	}
	if err := yaml.Unmarshal([]byte(decrypted), &secrets); err != nil {
		return nil, fmt.Errorf("parsing decrypted secrets: %w", err)
	}
	if strings.TrimSpace(secrets.Passphrase) == "" {
		return nil, fmt.Errorf("state_encryption_passphrase missing from tofu/secret.sops.yaml")
	}

	expr := fmt.Sprintf(`key_provider "pbkdf2" "statekey" { passphrase = %q }`, secrets.Passphrase)
	return dag.SetSecret("tf-encryption", expr), nil
}

// initTofu returns the container with the state passphrase injected and the
// backend initialised, ready to run a tofu subcommand.
func (m *Homelab) initTofu(ctx context.Context, c *dagger.Container) (*dagger.Container, error) {
	enc, err := m.stateEncryption(ctx, c)
	if err != nil {
		return nil, err
	}
	return c.
		WithSecretVariable("TF_ENCRYPTION", enc).
		WithExec([]string{"tofu", "init", "-lockfile=readonly"}), nil
}

// TofuPlan runs a read-only OpenTofu plan against the live infrastructure.
func (m *Homelab) TofuPlan(
	ctx context.Context,
	// The SOPS age private key (env://SOPS_AGE_KEY).
	sopsAgeKey *dagger.Secret,
) (string, error) {
	c, err := m.initTofu(ctx, m.tofuContainer(sopsAgeKey))
	if err != nil {
		return "", err
	}
	return c.
		WithExec([]string{"tofu", "plan", "-no-color", "-input=false"}).
		Stdout(ctx)
}

// TofuApply applies the OpenTofu configuration to the live infrastructure.
func (m *Homelab) TofuApply(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) (string, error) {
	c, err := m.initTofu(ctx, m.tofuContainer(sopsAgeKey))
	if err != nil {
		return "", err
	}
	return c.
		WithExec([]string{"tofu", "apply", "-auto-approve", "-no-color", "-input=false"}).
		Stdout(ctx)
}

// TofuDestroy tears down all managed infrastructure. The confirm argument must
// be exactly "destroy-production" or the function aborts before touching state.
func (m *Homelab) TofuDestroy(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
	// Safety guard: must equal "destroy-production".
	confirm string,
) (string, error) {
	if confirm != "destroy-production" {
		return "", fmt.Errorf("confirmation string did not match; expected \"destroy-production\", got %q", confirm)
	}
	c, err := m.initTofu(ctx, m.tofuContainer(sopsAgeKey))
	if err != nil {
		return "", err
	}
	return c.
		WithExec([]string{"tofu", "destroy", "-auto-approve", "-no-color", "-input=false"}).
		Stdout(ctx)
}
