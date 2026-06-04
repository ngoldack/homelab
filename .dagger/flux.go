package main

import (
	"context"

	"dagger/homelab/internal/dagger"
)

// FluxReconcile forces Flux to reconcile the git source and all three
// Kustomizations in dependency order.
//
// Network access to the cluster API server (reachable only over the private
// mesh) must be established by the caller before invoking this function — e.g.
// a NetBird connect step in the workflow, or a local mesh connection. The
// Dagger engine NATs outbound traffic through the host, so a host-side mesh
// connection is sufficient.
func (m *Homelab) FluxReconcile(
	ctx context.Context,
	// A kubeconfig granting access to the cluster (env://KUBECONFIG or file).
	kubeconfig *dagger.Secret,
) (string, error) {
	return m.liveBase().
		WithMountedSecret("/root/.kube/config", kubeconfig).
		WithEnvVariable("KUBECONFIG", "/root/.kube/config").
		WithExec([]string{"flux", "reconcile", "source", "git", "flux-system"}).
		WithExec([]string{"flux", "reconcile", "kustomization", "infra-controllers"}).
		WithExec([]string{"flux", "reconcile", "kustomization", "infra-configs"}).
		WithExec([]string{"flux", "reconcile", "kustomization", "apps"}).
		Stdout(ctx)
}
