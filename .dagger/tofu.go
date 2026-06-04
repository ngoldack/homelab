package main

import (
	"context"
	"fmt"

	"dagger/homelab/internal/dagger"
)

// liveBase extends the offline toolchain with the binaries needed to read and
// mutate live infrastructure: sops (decrypt secrets) and flux (reconcile).
//
// It does NOT manage network connectivity. The caller is responsible for
// establishing access to the private cluster network (e.g. a NetBird connect
// step before the `dagger call`, or a local mesh connection); the Dagger
// engine NATs outbound traffic through the host, so a host-side mesh
// connection is reachable from inside these containers.
func (m *Homelab) liveBase() *dagger.Container {
	v := ver()
	return m.base().
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /usr/local/bin/sops \
			  "https://github.com/getsops/sops/releases/download/v%[1]s/sops-v%[1]s.linux.${ARCH}"
			chmod +x /usr/local/bin/sops
		`, v.Sops))).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/flux.tar.gz \
			  "https://github.com/fluxcd/flux2/releases/download/v%[1]s/flux_%[1]s_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/flux.tar.gz -C /usr/local/bin flux
			rm /tmp/flux.tar.gz
		`, v.Flux)))
}

// tofuContainer prepares a live container in tofu/ with the SOPS age key.
func (m *Homelab) tofuContainer(sopsAgeKey *dagger.Secret) *dagger.Container {
	return m.liveBase().
		WithWorkdir("/src/tofu").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey)
}

// runTofu decrypts the state passphrase, initialises the backend and executes
// the supplied tofu command, returning combined stdout. Network access to the
// cluster is assumed to be provided by the caller's environment.
func runTofu(ctx context.Context, c *dagger.Container, tofuCmd string) (string, error) {
	script := `
		echo "==> Decrypting state-encryption passphrase"
		passphrase="$(sops -d secret.sops.yaml | yq '.state_encryption_passphrase')"
		export TF_ENCRYPTION="key_provider \"pbkdf2\" \"statekey\" { passphrase = \"${passphrase}\" }"

		echo "==> tofu init"
		tofu init -lockfile=readonly
	` + tofuCmd
	return c.
		WithExec([]string{"bash", "-euc", script}).
		Stdout(ctx)
}

// TofuPlan runs a read-only OpenTofu plan against the live infrastructure.
func (m *Homelab) TofuPlan(
	ctx context.Context,
	// The SOPS age private key (env://SOPS_AGE_KEY).
	sopsAgeKey *dagger.Secret,
) (string, error) {
	return runTofu(ctx, m.tofuContainer(sopsAgeKey), `
		echo "==> tofu plan"
		tofu plan -no-color -input=false -out=tofu.tfplan
	`)
}

// TofuApply applies the OpenTofu configuration to the live infrastructure.
func (m *Homelab) TofuApply(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
) (string, error) {
	return runTofu(ctx, m.tofuContainer(sopsAgeKey), `
		echo "==> tofu apply"
		tofu apply -auto-approve -no-color -input=false
	`)
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
	return runTofu(ctx, m.tofuContainer(sopsAgeKey), `
		echo "==> tofu destroy"
		tofu destroy -auto-approve -no-color -input=false
	`)
}
