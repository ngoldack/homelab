package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// Check runs every offline validation gate concurrently. This is the required
// branch-protection entrypoint and needs no secrets or cluster access.
func (m *Homelab) Check(ctx context.Context) (string, error) {
	type result struct {
		name string
		out  string
		err  error
	}

	gates := map[string]func(context.Context) (string, error){
		"lint":          m.Lint,
		"sops-check":    m.SopsCheck,
		"tofu-validate": m.TofuValidate,
		"tofu-security": m.TofuSecurity,
		"kube-validate": m.KubeValidate,
	}

	results := make([]result, 0, len(gates))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, fn := range gates {
		wg.Add(1)
		go func(name string, fn func(context.Context) (string, error)) {
			defer wg.Done()
			out, err := fn(ctx)
			mu.Lock()
			results = append(results, result{name, out, err})
			mu.Unlock()
		}(name, fn)
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

// Lint runs yamllint over the repo and actionlint over the workflows.
func (m *Homelab) Lint(ctx context.Context) (string, error) {
	return m.base().
		WithExec([]string{"bash", "-euc", `
			echo "==> yamllint"
			yamllint -c .yamllint .
			echo "==> actionlint"
			# .git is excluded from the mounted source, so actionlint cannot
			# auto-detect the project root; point it at the workflow files.
			files="$(find .github/workflows -type f \( -name '*.yml' -o -name '*.yaml' \))"
			actionlint $files
		`}).
		Stdout(ctx)
}

// SopsCheck asserts every *.sops.yaml file (except the .sops.yaml config) is
// actually encrypted: it must contain a top-level sops: block and an ENC[ value.
func (m *Homelab) SopsCheck(ctx context.Context) (string, error) {
	return m.base().
		WithExec([]string{"bash", "-euc", `
			unencrypted=0
			while IFS= read -r -d '' f; do
			  if [ "$(basename "$f")" = ".sops.yaml" ]; then continue; fi
			  if ! grep -q '^sops:' "$f" || ! grep -q 'ENC\[' "$f"; then
			    echo "ERROR: unencrypted or malformed SOPS file: $f"
			    unencrypted=$((unencrypted + 1))
			  fi
			done < <(find . -type f -name '*.sops.yaml' -print0)
			if [ "$unencrypted" -gt 0 ]; then
			  echo "Security validation failed: $unencrypted file(s) not properly encrypted."
			  exit 1
			fi
			echo "All *.sops.yaml files are securely encrypted."
		`}).
		Stdout(ctx)
}

// TofuValidate runs offline OpenTofu checks: fmt, init (no backend) and validate.
func (m *Homelab) TofuValidate(ctx context.Context) (string, error) {
	return m.base().
		WithWorkdir("/src/tofu").
		// pbkdf2 key provider builds its key eagerly during init, so supply a
		// throwaway passphrase. No real state is read or written.
		WithEnvVariable("TF_ENCRYPTION", `key_provider "pbkdf2" "statekey" { passphrase = "ci-offline-validate-dummy-passphrase" }`).
		WithExec([]string{"sh", "-euc", `
			echo "==> tofu fmt -check"
			tofu fmt -check -recursive
			echo "==> tofu init (no backend)"
			tofu init -backend=false -lockfile=readonly
			echo "==> tofu validate"
			tofu validate
		`}).
		Stdout(ctx)
}

// TofuSecurity runs a Trivy IaC config scan over the tofu directory and fails
// on CRITICAL findings (ignoring unfixed issues), matching the CI policy.
func (m *Homelab) TofuSecurity(ctx context.Context) (string, error) {
	return m.base().
		WithEnvVariable("TRIVY_CACHE_DIR", "/root/.cache/trivy").
		WithMountedCache("/root/.cache/trivy", dag.CacheVolume("trivy-cache")).
		WithExec([]string{
			"trivy", "config", "tofu",
			"--severity", "CRITICAL",
			"--exit-code", "1",
		}).
		Stdout(ctx)
}

// KubeValidate builds every Kustomize overlay, schema-validates the output with
// kubeconform, and asserts the Flux Kustomization invariants.
func (m *Homelab) KubeValidate(ctx context.Context) (string, error) {
	paths := []string{
		"kubernetes/clusters/production",
		"kubernetes/infrastructure/controllers",
		"kubernetes/infrastructure/configs",
		"kubernetes/apps/production",
	}
	script := `
		validate() {
		  echo "==== Building & validating $1 ===="
		  kustomize build "$1" \
		    | kubeconform -strict -ignore-missing-schemas -summary -verbose=false
		}
	`
	for _, p := range paths {
		script += fmt.Sprintf("validate %s\n", p)
	}
	script += `
		infra=kubernetes/clusters/production/infrastructure.yaml
		apps=kubernetes/clusters/production/apps.yaml
		assert() {
		  actual="$(yq "select(.metadata.name == \"$2\") | $3" "$1")"
		  if [ "$actual" != "$4" ]; then
		    echo "ERROR: $1 [$2] $5 (got '$actual', expected '$4')"
		    exit 1
		  fi
		  echo "OK: $1 [$2] - $5"
		}
		for name in infra-controllers infra-configs; do
		  assert "$infra" "$name" '.spec.decryption.provider' 'sops' 'decryption.provider must be sops'
		  assert "$infra" "$name" '.spec.decryption.secretRef.name' 'sops-age' 'secretRef.name must be sops-age'
		  assert "$infra" "$name" '.spec.prune' 'true' 'prune must be enabled'
		done
		assert "$apps" 'apps' '.spec.decryption.provider' 'sops' 'decryption.provider must be sops'
		assert "$apps" 'apps' '.spec.decryption.secretRef.name' 'sops-age' 'secretRef.name must be sops-age'
		assert "$apps" 'apps' '.spec.prune' 'true' 'prune must be enabled'
		assert "$infra" 'infra-configs' '.spec.dependsOn[] | select(.name == "infra-controllers") | .name' 'infra-controllers' 'infra-configs must dependsOn infra-controllers'
		assert "$apps" 'apps' '.spec.dependsOn[] | select(.name == "infra-configs") | .name' 'infra-configs' 'apps must dependsOn infra-configs'
	`
	return m.base().
		WithExec([]string{"sh", "-euc", script}).
		Stdout(ctx)
}
