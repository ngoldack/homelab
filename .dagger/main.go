// Homelab provides a single, container-native pipeline for this GitOps repo.
//
// Every function runs inside a pinned container, so the exact same commands
// execute locally (`dagger call ...`) and in CI (`dagger/dagger-for-github`).
// It replaces the previous Taskfile + hand-written GitHub Actions workflows.
//
// Offline gates (no secrets) live in offline.go. Each is an independently
// callable function (`dagger call lint`, `dagger call kube-validate`, ...) so
// CI runs them as a matrix of separate checks; `check` aggregates them for a
// single local run. Live, mutating infrastructure operations (tofu
// plan/apply/destroy, flux reconcile) live in tofu.go / flux.go and take Dagger
// secrets as arguments. They assume network connectivity to the cluster is
// already established by the caller (e.g. a NetBird step before the
// `dagger call`, or your local mesh connection).
//
// All tool versions come from versions.json — the single source of truth. Each
// CLI is copied as a static binary out of its pinned upstream image, so there
// is no curl/unzip/arch handling and Renovate tracks plain container tags.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"dagger/homelab/internal/dagger"
)

// versions.json is the single source of truth for every pinned tool image. Each
// value is a fully-qualified `image:tag` reference.
//
//go:embed versions.json
var versionsJSON []byte

// toolImages holds the pinned `image:tag` reference for the base image and each
// CLI copied into the toolchain.
type toolImages struct {
	Debian      string `json:"debian"`
	Tofu        string `json:"tofu"`
	Kustomize   string `json:"kustomize"`
	Kubeconform string `json:"kubeconform"`
	Sops        string `json:"sops"`
	Actionlint  string `json:"actionlint"`
	Trivy       string `json:"trivy"`
	Flux        string `json:"flux"`
}

func images() toolImages {
	var v toolImages
	if err := json.Unmarshal(versionsJSON, &v); err != nil {
		panic(fmt.Sprintf("invalid versions.json: %v", err))
	}
	return v
}

type Homelab struct {
	// The repository root, mounted into every tool container.
	Source *dagger.Directory
}

// New constructs the module bound to the repository root. The directory is
// passed automatically by Dagger (defaultPath="/") so local and CI invocations
// behave identically; noisy/secret paths are ignored to keep the cache stable.
func New(
	// The repository root to operate on.
	// +defaultPath="/"
	// +ignore=[".git", ".dagger/internal", ".dagger/dagger.gen.go", "kubeconfig.yaml", "talosconfig.yaml", "age.key", ".env", "**/.terraform", "**/*.tfplan", "**/plan.txt"]
	source *dagger.Directory,
) *Homelab {
	return &Homelab{Source: source}
}

// Versions prints the pinned tool images (the contents of versions.json).
func (m *Homelab) Versions() string {
	return string(versionsJSON)
}

// base returns the minimal toolchain container: a pinned Debian with CA
// certificates and the repository mounted at /src. Individual gates layer on
// only the binaries they need via the withX helpers below, keeping each
// container small and the cache sharp.
func (m *Homelab) base() *dagger.Container {
	return dag.Container().
		From(images().Debian).
		WithEnvVariable("DEBIAN_FRONTEND", "noninteractive").
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends", "ca-certificates"}).
		WithExec([]string{"rm", "-rf", "/var/lib/apt/lists"}).
		WithWorkdir("/src").
		WithMountedDirectory("/src", m.Source)
}

// copyBin copies a single static binary out of an upstream image into the
// toolchain at /usr/local/bin. The upstream images are all multi-arch, so this
// works unchanged on amd64 (CI) and arm64 (local) with no arch juggling.
func copyBin(image, srcPath, name string) dagger.WithContainerFunc {
	return func(c *dagger.Container) *dagger.Container {
		bin := dag.Container().From(image).File(srcPath)
		return c.WithFile("/usr/local/bin/"+name, bin, dagger.ContainerWithFileOpts{Permissions: 0o755})
	}
}

func (m *Homelab) withTofu(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Tofu, "/usr/local/bin/tofu", "tofu"))
}

func (m *Homelab) withKustomize(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Kustomize, "/app/kustomize", "kustomize"))
}

func (m *Homelab) withKubeconform(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Kubeconform, "/kubeconform", "kubeconform"))
}

func (m *Homelab) withTrivy(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Trivy, "/usr/local/bin/trivy", "trivy"))
}

func (m *Homelab) withActionlint(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Actionlint, "/usr/local/bin/actionlint", "actionlint"))
}

func (m *Homelab) withSops(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Sops, "/usr/local/bin/sops", "sops"))
}

func (m *Homelab) withFlux(c *dagger.Container) *dagger.Container {
	return c.With(copyBin(images().Flux, "/usr/local/bin/flux", "flux"))
}

// withYamllint installs yamllint (a Python tool with no static binary to copy)
// from Debian's package repository.
func (m *Homelab) withYamllint(c *dagger.Container) *dagger.Container {
	return c.
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends", "yamllint"}).
		WithExec([]string{"rm", "-rf", "/var/lib/apt/lists"})
}
