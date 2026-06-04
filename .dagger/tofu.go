package main

import (
	"context"
	"fmt"

	"dagger/homelab/internal/dagger"
)

// liveBase extends the offline toolchain with the binaries needed to reach and
// mutate live infrastructure: sops (decrypt secrets), flux (reconcile) and the
// netbird client (join the private mesh).
func (m *Homelab) liveBase() *dagger.Container {
	return m.base().
		With(installTool("sops", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /usr/local/bin/sops \
			  "https://github.com/getsops/sops/releases/download/v`+sopsVersion+`/sops-v`+sopsVersion+`.linux.${ARCH}"
			chmod +x /usr/local/bin/sops
		`)).
		With(installTool("flux", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/flux.tar.gz \
			  "https://github.com/fluxcd/flux2/releases/download/v`+fluxVersion+`/flux_`+fluxVersion+`_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/flux.tar.gz -C /usr/local/bin flux
			rm /tmp/flux.tar.gz
		`)).
		With(installTool("netbird", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/netbird.tar.gz \
			  "https://github.com/netbirdio/netbird/releases/download/v`+netbirdVersion+`/netbird_`+netbirdVersion+`_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/netbird.tar.gz -C /usr/local/bin netbird
			rm /tmp/netbird.tar.gz
		`))
}

// connectNetbird emits the shell prologue that joins the NetBird mesh. It must
// run in an exec with InsecureRootCapabilities so the wireguard interface and
// tun device can be created.
//
// NOTE (spike): the in-container NetBird bring-up is unverified against the live
// cluster (greenfield). If tun creation proves unreliable in CI, fall back to
// joining the mesh on the runner and running these steps outside Dagger.
func connectNetbird() string {
	return `
		echo "==> Connecting to NetBird mesh"
		mkdir -p /var/run/netbird
		netbird service install || true
		netbird service start || (netbird service run >/tmp/netbird.log 2>&1 &)
		sleep 3
		netbird up --setup-key "$NB_SETUP_KEY" ${NB_MANAGEMENT_URL:+--management-url "$NB_MANAGEMENT_URL"}
		sleep 5
		netbird status || true
	`
}

// tofuContainer prepares a live container in tofu/ wired with NetBird + SOPS.
func (m *Homelab) tofuContainer(sopsAgeKey, netbirdSetupKey *dagger.Secret, managementURL string) *dagger.Container {
	c := m.liveBase().
		WithWorkdir("/src/tofu").
		WithSecretVariable("SOPS_AGE_KEY", sopsAgeKey).
		WithSecretVariable("NB_SETUP_KEY", netbirdSetupKey)
	if managementURL != "" {
		c = c.WithEnvVariable("NB_MANAGEMENT_URL", managementURL)
	}
	return c
}

// runTofu executes a tofu command inside the live container with the elevated
// capabilities NetBird needs, returning combined stdout.
func runTofu(ctx context.Context, c *dagger.Container, tofuCmd string) (string, error) {
	script := connectNetbird() + `
		echo "==> Decrypting state-encryption passphrase"
		passphrase="$(sops -d secret.sops.yaml | yq '.state_encryption_passphrase')"
		export TF_ENCRYPTION="key_provider \"pbkdf2\" \"statekey\" { passphrase = \"${passphrase}\" }"

		echo "==> tofu init"
		tofu init -lockfile=readonly
	` + tofuCmd
	return c.
		WithExec([]string{"sh", "-euc", script},
			dagger.ContainerWithExecOpts{InsecureRootCapabilities: true}).
		Stdout(ctx)
}

// TofuPlan runs a read-only OpenTofu plan against the live infrastructure.
func (m *Homelab) TofuPlan(
	ctx context.Context,
	// The SOPS age private key (env://SOPS_AGE_KEY).
	sopsAgeKey *dagger.Secret,
	// The NetBird setup key used to join the mesh (env://NETBIRD_SETUP_KEY).
	netbirdSetupKey *dagger.Secret,
	// Optional NetBird management URL (self-hosted). Empty uses NetBird default.
	// +optional
	managementURL string,
) (string, error) {
	c := m.tofuContainer(sopsAgeKey, netbirdSetupKey, managementURL)
	return runTofu(ctx, c, `
		echo "==> tofu plan"
		tofu plan -no-color -input=false -out=tofu.tfplan
	`)
}

// TofuApply applies the OpenTofu configuration to the live infrastructure.
func (m *Homelab) TofuApply(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
	netbirdSetupKey *dagger.Secret,
	// +optional
	managementURL string,
) (string, error) {
	c := m.tofuContainer(sopsAgeKey, netbirdSetupKey, managementURL)
	return runTofu(ctx, c, `
		echo "==> tofu apply"
		tofu apply -auto-approve -no-color -input=false
	`)
}

// TofuDestroy tears down all managed infrastructure. The confirm argument must
// be exactly "destroy-production" or the function aborts before touching state.
func (m *Homelab) TofuDestroy(
	ctx context.Context,
	sopsAgeKey *dagger.Secret,
	netbirdSetupKey *dagger.Secret,
	// Safety guard: must equal "destroy-production".
	confirm string,
	// +optional
	managementURL string,
) (string, error) {
	if confirm != "destroy-production" {
		return "", fmt.Errorf("confirmation string did not match; expected \"destroy-production\", got %q", confirm)
	}
	c := m.tofuContainer(sopsAgeKey, netbirdSetupKey, managementURL)
	return runTofu(ctx, c, `
		echo "==> tofu destroy"
		tofu destroy -auto-approve -no-color -input=false
	`)
}
