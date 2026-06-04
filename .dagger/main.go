// Homelab provides a single, container-native pipeline for this GitOps repo.
//
// Every function runs inside a pinned container, so the exact same commands
// execute locally (`dagger call ...`) and in CI (`dagger/dagger-for-github`).
// It replaces the previous Taskfile + hand-written GitHub Actions workflows.
//
// Offline gates (no secrets) live in offline.go and are aggregated by `check`,
// which is the required branch-protection entrypoint. Live, mutating
// infrastructure operations (tofu plan/apply/destroy, flux reconcile) live in
// tofu.go / flux.go and take Dagger secrets as arguments. They assume network
// connectivity to the cluster is already established by the caller (e.g. a
// NetBird step before the `dagger call`, or your local mesh connection).
//
// All tool versions come from versions.json — the single source of truth.
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"

	"dagger/homelab/internal/dagger"
)

// versions.json is the single source of truth for every pinned tool version.
//
//go:embed versions.json
var versionsJSON []byte

type toolVersions struct {
	Debian      string `json:"debian"`
	Tofu        string `json:"tofu"`
	Kustomize   string `json:"kustomize"`
	Kubeconform string `json:"kubeconform"`
	Sops        string `json:"sops"`
	Yq          string `json:"yq"`
	Actionlint  string `json:"actionlint"`
	Trivy       string `json:"trivy"`
	Flux        string `json:"flux"`
}

func ver() toolVersions {
	var v toolVersions
	if err := json.Unmarshal(versionsJSON, &v); err != nil {
		panic(fmt.Sprintf("invalid versions.json: %v", err))
	}
	return v
}

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

// Versions prints the pinned tool versions (the contents of versions.json).
func (m *Homelab) Versions() string {
	return string(versionsJSON)
}

// base returns the offline toolchain container: everything required by the
// validation gates (no secrets, no cluster access).
func (m *Homelab) base() *dagger.Container {
	v := ver()
	return dag.Container().
		From("debian:"+v.Debian).
		WithEnvVariable("DEBIAN_FRONTEND", "noninteractive").
		WithExec([]string{"apt-get", "update"}).
		WithExec([]string{"apt-get", "install", "-y", "--no-install-recommends",
			"ca-certificates", "curl", "wget", "git", "unzip", "tar", "gzip",
			"bash", "yamllint", "age"}).
		WithExec([]string{"rm", "-rf", "/var/lib/apt/lists"}).
		// Arch-aware tool installs (works on amd64 in CI and arm64 locally).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/tofu.zip \
			  "https://github.com/opentofu/opentofu/releases/download/v%[1]s/tofu_%[1]s_linux_${ARCH}.zip"
			unzip -o /tmp/tofu.zip tofu -d /usr/local/bin
			rm /tmp/tofu.zip
		`, v.Tofu))).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/kustomize.tar.gz \
			  "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%%2Fv%[1]s/kustomize_v%[1]s_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/kustomize.tar.gz -C /usr/local/bin kustomize
			rm /tmp/kustomize.tar.gz
		`, v.Kustomize))).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/kubeconform.tar.gz \
			  "https://github.com/yannh/kubeconform/releases/download/v%[1]s/kubeconform-linux-${ARCH}.tar.gz"
			tar -xzf /tmp/kubeconform.tar.gz -C /usr/local/bin kubeconform
			rm /tmp/kubeconform.tar.gz
		`, v.Kubeconform))).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /usr/local/bin/yq \
			  "https://github.com/mikefarah/yq/releases/download/v%[1]s/yq_linux_${ARCH}"
			chmod +x /usr/local/bin/yq
		`, v.Yq))).
		With(installTool(fmt.Sprintf(`
			ARCH="$(dpkg --print-architecture)"
			curl -fsSL -o /tmp/actionlint.tar.gz \
			  "https://github.com/rhysd/actionlint/releases/download/v%[1]s/actionlint_%[1]s_linux_${ARCH}.tar.gz"
			tar -xzf /tmp/actionlint.tar.gz -C /usr/local/bin actionlint
			rm /tmp/actionlint.tar.gz
		`, v.Actionlint))).
		With(installTool(fmt.Sprintf(`
			case "$(dpkg --print-architecture)" in
			  amd64) T=64bit ;;
			  arm64) T=ARM64 ;;
			  *) T=64bit ;;
			esac
			curl -fsSL -o /tmp/trivy.tar.gz \
			  "https://github.com/aquasecurity/trivy/releases/download/v%[1]s/trivy_%[1]s_Linux-${T}.tar.gz"
			tar -xzf /tmp/trivy.tar.gz -C /usr/local/bin trivy
			rm /tmp/trivy.tar.gz
		`, v.Trivy))).
		WithWorkdir("/src").
		WithMountedDirectory("/src", m.Source)
}

// installTool runs a shell snippet as a single cached layer.
func installTool(script string) dagger.WithContainerFunc {
	return func(c *dagger.Container) *dagger.Container {
		return c.WithExec([]string{"bash", "-euc", script})
	}
}
