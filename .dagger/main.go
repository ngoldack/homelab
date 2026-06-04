// Homelab provides a single, container-native pipeline for this GitOps repo.
//
// Every function runs inside a pinned container, so the exact same commands
// execute locally (`dagger call ...`) and in CI (`dagger/dagger-for-github`).
// It replaces the previous Taskfile + hand-written GitHub Actions workflows.
//
// Offline gates (no secrets) live in offline.go and are aggregated by `check`,
// which is the required branch-protection entrypoint. Live, mutating
// infrastructure operations (tofu plan/apply/destroy, flux reconcile) live in
// tofu.go / flux.go and take Dagger secrets as arguments.
package main

import (
	"dagger/homelab/internal/dagger"
)

// Pinned tool versions. Keep these in sync with .github/workflows and AGENTS.md.
const (
	debianImage      = "debian:bookworm-20250520-slim"
	tofuVersion      = "1.7.2"
	kubeconformVer   = "0.6.7"
	kustomizeVersion = "5.4.3"
	sopsVersion      = "3.9.1"
	yqVersion        = "4.44.3"
	actionlintVer    = "1.7.1"
	trivyVersion     = "0.71.0"
	fluxVersion      = "2.3.0"
	netbirdVersion   = "0.28.4"
)

type Homelab struct {
	// The repository root, mounted read-only into every tool container.
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

// base returns the offline toolchain container: everything required by the
// validation gates (no secrets, no network access to the cluster).
func (m *Homelab) base() *dagger.Container {
	return dag.Container().
		From(debianImage).
		WithEnvVariable("DEBIAN_FRONTEND", "noninteractive").
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends",
			"ca-certificates", "curl", "wget", "git", "unzip", "tar", "gzip",
			"yamllint", "age"}).
		WithExec([]string{"rm", "-rf", "/var/lib/apt/lists"}).
		// Arch-aware tool installs (works on amd64 in CI and arm64 locally).
		With(installTool("tofu", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/tofu.zip \
			  "https://github.com/opentofu/opentofu/releases/download/v`+tofuVersion+`/tofu_`+tofuVersion+`_linux_${ARCH}.zip"
			unzip -o /tmp/tofu.zip tofu -d /usr/local/bin
			rm /tmp/tofu.zip
		`)).
		With(installTool("kustomize", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/kustomize.tar.gz \
			  "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv`+kustomizeVersion+`/kustomize_v`+kustomizeVersion+`_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/kustomize.tar.gz -C /usr/local/bin kustomize
			rm /tmp/kustomize.tar.gz
		`)).
		With(installTool("kubeconform", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/kubeconform.tar.gz \
			  "https://github.com/yannh/kubeconform/releases/download/v`+kubeconformVer+`/kubeconform-linux-${ARCH}.tar.gz"
			tar -xzf /tmp/kubeconform.tar.gz -C /usr/local/bin kubeconform
			rm /tmp/kubeconform.tar.gz
		`)).
		With(installTool("yq", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /usr/local/bin/yq \
			  "https://github.com/mikefarah/yq/releases/download/v`+yqVersion+`/yq_linux_${ARCH}"
			chmod +x /usr/local/bin/yq
		`)).
		With(installTool("actionlint", `
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/actionlint.tar.gz \
			  "https://github.com/rhysd/actionlint/releases/download/v`+actionlintVer+`/actionlint_`+actionlintVer+`_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/actionlint.tar.gz -C /usr/local/bin actionlint
			rm /tmp/actionlint.tar.gz
		`)).
		With(installTool("trivy", `
			case "$(dpkg --print-architecture)" in
			  amd64) T=64bit ;;
			  arm64) T=ARM64 ;;
			  *) T=64bit ;;
			esac
			curl -fsSL -o /tmp/trivy.tar.gz \
			  "https://github.com/aquasecurity/trivy/releases/download/v`+trivyVersion+`/trivy_`+trivyVersion+`_Linux-${T}.tar.gz"
			tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy
			rm /tmp/trivy.tar.gz
		`)).
		WithWorkdir("/src").
		WithMountedDirectory("/src", m.Source)
}

// installTool runs a shell snippet as a single cached layer.
func installTool(name, script string) dagger.WithContainerFunc {
	return func(c *dagger.Container) *dagger.Container {
		return c.WithExec([]string{"sh", "-euc", script})
	}
}
