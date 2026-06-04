package main

import (
	"context"

	"dagger/homelab/internal/dagger"
)

// FluxReconcile forces Flux to reconcile the git source and all three
// Kustomizations in dependency order. It joins the NetBird mesh first because
// the cluster API server is only reachable over the private network.
//
// This fixes the previous workflow bug which reconciled non-existent
// Kustomizations named "infrastructure"/"apps"; the real names are
// infra-controllers, infra-configs and apps.
func (m *Homelab) FluxReconcile(
	ctx context.Context,
	// A kubeconfig granting access to the cluster (env://KUBECONFIG or file).
	kubeconfig *dagger.Secret,
	// The NetBird setup key used to join the mesh (env://NETBIRD_SETUP_KEY).
	netbirdSetupKey *dagger.Secret,
	// +optional
	managementURL string,
) (string, error) {
	c := m.liveBase().
		WithMountedSecret("/root/.kube/config", kubeconfig).
		WithEnvVariable("KUBECONFIG", "/root/.kube/config").
		WithSecretVariable("NB_SETUP_KEY", netbirdSetupKey)
	if managementURL != "" {
		c = c.WithEnvVariable("NB_MANAGEMENT_URL", managementURL)
	}
	script := connectNetbird() + `
		echo "==> Reconciling Flux"
		flux reconcile source git flux-system
		flux reconcile kustomization infra-controllers
		flux reconcile kustomization infra-configs
		flux reconcile kustomization apps
	`
	return c.
		WithExec([]string{"sh", "-euc", script},
			dagger.ContainerWithExecOpts{InsecureRootCapabilities: true}).
		Stdout(ctx)
}
