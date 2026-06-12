package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// gate is a single named validation gate. Every gate is also exported as its
// own Dagger function so CI can run them as a matrix of independent checks.
type gate struct {
	name string
	fn   func(context.Context) (string, error)
}

// orderedGates is the canonical gate list and render order.
func (m *Homelab) orderedGates() []gate {
	return []gate{
		{"yamllint", m.Yamllint},
		{"actionlint", m.Actionlint},
		{"sops-check", m.SopsCheck},
		{"tofu-validate", m.TofuValidate},
		{"tofu-security", m.TofuSecurity},
		{"kube-validate", m.KubeValidate},
	}
}

// Check runs every offline gate concurrently and aggregates the results. It is
// a convenience entrypoint for a single local run (`dagger call check`); CI runs
// each gate as its own matrix job so failures map to distinct GitHub checks.
func (m *Homelab) Check(ctx context.Context) (string, error) {
	type result struct {
		name string
		out  string
		err  error
	}

	gates := m.orderedGates()
	results := make([]result, len(gates))
	var wg sync.WaitGroup
	for i, g := range gates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := g.fn(ctx)
			results[i] = result{g.name, out, err}
		}()
	}
	wg.Wait()

	var b strings.Builder
	var failed []string
	for _, r := range results {
		status := "PASS"
		if r.err != nil {
			status = "FAIL"
			failed = append(failed, r.name)
		}
		fmt.Fprintf(&b, "==== %s: %s ====\n%s\n", r.name, status, strings.TrimSpace(r.out))
		if r.err != nil {
			fmt.Fprintf(&b, "error: %v\n", r.err)
		}
	}
	if len(failed) > 0 {
		return b.String(), fmt.Errorf("offline gates failed: %s", strings.Join(failed, ", "))
	}
	return b.String() + "\nAll offline gates passed.\n", nil
}

// Yamllint lints every YAML file in the repository against .yamllint.
func (m *Homelab) Yamllint(ctx context.Context) (string, error) {
	return m.withYamllint(m.base()).
		WithExec([]string{"yamllint", "-c", ".yamllint", "."}).
		Stdout(ctx)
}

// Actionlint statically checks the GitHub Actions workflows. The workflow file
// list is resolved here (the mounted source excludes .git, so actionlint cannot
// auto-detect the project root) and passed explicitly.
func (m *Homelab) Actionlint(ctx context.Context) (string, error) {
	files, err := m.workflowFiles(ctx)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "no workflow files found\n", nil
	}
	return m.withActionlint(m.base()).
		WithExec(append([]string{"actionlint"}, files...)).
		Stdout(ctx)
}

// workflowFiles returns the repo-relative paths of every GitHub Actions workflow.
func (m *Homelab) workflowFiles(ctx context.Context) ([]string, error) {
	var files []string
	for _, pattern := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml"} {
		matches, err := m.Source.Glob(ctx, pattern)
		if err != nil {
			return nil, err
		}
		files = append(files, matches...)
	}
	return files, nil
}

// SopsCheck asserts every *.sops.yaml file (except the .sops.yaml config) is
// actually encrypted: it must contain a top-level sops: block and an ENC[
// value. This is a pure-Go inspection of the source tree — no container.
func (m *Homelab) SopsCheck(ctx context.Context) (string, error) {
	matches, err := m.Source.Glob(ctx, "**/*.sops.yaml")
	if err != nil {
		return "", err
	}

	var b strings.Builder
	var bad []string
	for _, f := range matches {
		if path.Base(f) == ".sops.yaml" {
			continue // the SOPS config itself, not an encrypted payload
		}
		content, err := m.Source.File(f).Contents(ctx)
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", f, err)
		}
		if !sopsEncrypted(content) {
			fmt.Fprintf(&b, "ERROR: unencrypted or malformed SOPS file: %s\n", f)
			bad = append(bad, f)
			continue
		}
		fmt.Fprintf(&b, "OK: %s\n", f)
	}
	if len(bad) > 0 {
		return b.String(), fmt.Errorf("%d file(s) not properly encrypted: %s", len(bad), strings.Join(bad, ", "))
	}
	fmt.Fprintf(&b, "All %d *.sops.yaml file(s) are securely encrypted.\n", len(matches))
	return b.String(), nil
}

// sopsEncrypted reports whether a SOPS-managed YAML file is actually encrypted:
// it carries a top-level `sops:` metadata block and at least one ENC[ value.
func sopsEncrypted(content string) bool {
	hasBlock := strings.HasPrefix(content, "sops:") || strings.Contains(content, "\nsops:")
	return hasBlock && strings.Contains(content, "ENC[")
}

// TofuValidate runs offline OpenTofu checks: fmt, init (no backend) and validate.
func (m *Homelab) TofuValidate(ctx context.Context) (string, error) {
	return m.withTofu(m.base()).
		WithWorkdir("/src/tofu").
		// The pbkdf2 key provider builds its key eagerly during init, so supply
		// a throwaway passphrase. No real state is read or written here.
		WithEnvVariable("TF_ENCRYPTION", `key_provider "pbkdf2" "statekey" { passphrase = "ci-offline-validate-dummy-passphrase" }`).
		WithExec([]string{"tofu", "fmt", "-check", "-recursive"}).
		WithExec([]string{"tofu", "init", "-backend=false", "-lockfile=readonly"}).
		WithExec([]string{"tofu", "validate"}).
		Stdout(ctx)
}

// TofuSecurity runs a Trivy IaC config scan over the tofu directory and fails
// on CRITICAL findings.
func (m *Homelab) TofuSecurity(ctx context.Context) (string, error) {
	return m.withTrivy(m.base()).
		WithEnvVariable("TRIVY_CACHE_DIR", "/root/.cache/trivy").
		WithMountedCache("/root/.cache/trivy", dag.CacheVolume("trivy-cache")).
		WithExec([]string{"trivy", "config", "tofu", "--severity", "CRITICAL", "--exit-code", "1"}).
		Stdout(ctx)
}

// overlayPaths are the Kustomize entrypoints that must build and schema-validate.
var overlayPaths = []string{
	"kubernetes/clusters/production",
	"kubernetes/infrastructure/controllers",
	"kubernetes/infrastructure/configs",
	"kubernetes/apps/production",
}

// KubeValidate builds every Kustomize overlay, schema-validates the rendered
// manifests with kubeconform, and asserts the Flux Kustomization invariants
// (decryption, prune, dependency ordering) in native Go.
func (m *Homelab) KubeValidate(ctx context.Context) (string, error) {
	var b strings.Builder

	tools := m.withKubeconform(m.withKustomize(m.base()))
	for _, p := range overlayPaths {
		fmt.Fprintf(&b, "==== Building & validating %s ====\n", p)
		out, err := tools.
			WithExec([]string{"kustomize", "build", p, "-o", "/tmp/rendered.yaml"}).
			WithExec([]string{"kubeconform", "-strict", "-ignore-missing-schemas", "-summary", "-verbose=false", "/tmp/rendered.yaml"}).
			Stdout(ctx)
		b.WriteString(strings.TrimSpace(out))
		b.WriteString("\n")
		if err != nil {
			return b.String(), fmt.Errorf("kustomize/kubeconform failed for %s: %w", p, err)
		}
	}

	if err := m.assertFluxInvariants(ctx, &b); err != nil {
		return b.String(), err
	}
	return b.String(), nil
}

// fluxKustomization is the subset of a Flux Kustomization we assert on.
type fluxKustomization struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Prune      bool `yaml:"prune"`
		Decryption struct {
			Provider  string `yaml:"provider"`
			SecretRef struct {
				Name string `yaml:"name"`
			} `yaml:"secretRef"`
		} `yaml:"decryption"`
		DependsOn []struct {
			Name string `yaml:"name"`
		} `yaml:"dependsOn"`
	} `yaml:"spec"`
}

func (k fluxKustomization) dependsOn(name string) bool {
	for _, d := range k.Spec.DependsOn {
		if d.Name == name {
			return true
		}
	}
	return false
}

// assertFluxInvariants enforces the cluster's GitOps guardrails directly on the
// Flux Kustomization manifests: SOPS decryption must be wired up, pruning must
// be on, and the controllers -> configs -> apps ordering must hold.
func (m *Homelab) assertFluxInvariants(ctx context.Context, b *strings.Builder) error {
	infra, err := m.loadKustomizations(ctx, "kubernetes/clusters/production/infrastructure.yaml")
	if err != nil {
		return err
	}
	apps, err := m.loadKustomizations(ctx, "kubernetes/clusters/production/apps.yaml")
	if err != nil {
		return err
	}

	var problems []string
	check := func(cond bool, okMsg, failMsg string) {
		if cond {
			fmt.Fprintf(b, "OK: %s\n", okMsg)
			return
		}
		fmt.Fprintf(b, "ERROR: %s\n", failMsg)
		problems = append(problems, failMsg)
	}

	requireSopsAndPrune := func(set map[string]fluxKustomization, name string) {
		k, ok := set[name]
		if !ok {
			check(false, "", fmt.Sprintf("%s Kustomization not found", name))
			return
		}
		check(k.Spec.Decryption.Provider == "sops", name+" decryption.provider=sops", name+" decryption.provider must be sops")
		check(k.Spec.Decryption.SecretRef.Name == "sops-age", name+" secretRef.name=sops-age", name+" decryption.secretRef.name must be sops-age")
		check(k.Spec.Prune, name+" prune=true", name+" prune must be enabled")
	}

	requireSopsAndPrune(infra, "infra-controllers")
	requireSopsAndPrune(infra, "infra-configs")
	requireSopsAndPrune(apps, "apps")

	check(infra["infra-configs"].dependsOn("infra-controllers"),
		"infra-configs dependsOn infra-controllers",
		"infra-configs must dependsOn infra-controllers")
	check(apps["apps"].dependsOn("infra-configs"),
		"apps dependsOn infra-configs",
		"apps must dependsOn infra-configs")

	if len(problems) > 0 {
		return fmt.Errorf("flux invariant violations: %s", strings.Join(problems, "; "))
	}
	return nil
}

// loadKustomizations decodes a (possibly multi-document) Flux manifest into a
// map keyed by metadata.name.
func (m *Homelab) loadKustomizations(ctx context.Context, file string) (map[string]fluxKustomization, error) {
	content, err := m.Source.File(file).Contents(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	out := map[string]fluxKustomization{}
	dec := yaml.NewDecoder(strings.NewReader(content))
	for {
		var k fluxKustomization
		if err := dec.Decode(&k); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parsing %s: %w", file, err)
		}
		if k.Metadata.Name != "" {
			out[k.Metadata.Name] = k
		}
	}
	return out, nil
}
