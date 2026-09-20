package guard

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func TestEnvStrDefaultsAndEmpty(t *testing.T) {
	const name = "GUARD_TEST_ENV_STR"
	os.Unsetenv(name)

	if got := EnvStr(name, "fallback"); got != "fallback" {
		t.Fatalf("unset: got %q, want %q", got, "fallback")
	}
	t.Setenv(name, "")
	if got := EnvStr(name, "fallback"); got != "fallback" {
		t.Fatalf("empty: got %q, want %q", got, "fallback")
	}
	t.Setenv(name, "explicit")
	if got := EnvStr(name, "fallback"); got != "explicit" {
		t.Fatalf("set: got %q, want %q", got, "explicit")
	}
	if got := EnvStr(name, ""); got != "explicit" {
		t.Fatalf("set with empty default: got %q, want %q", got, "explicit")
	}
}

func TestEnvIntParsesAndFallsBack(t *testing.T) {
	const name = "GUARD_TEST_ENV_INT"
	t.Setenv(name, "")
	if got := EnvInt(name, 86400); got != 86400 {
		t.Fatalf("empty: got %d, want 86400", got)
	}
	t.Setenv(name, "-7")
	if got := EnvInt(name, 86400); got != -7 {
		t.Fatalf("set: got %d, want -7", got)
	}
	// Python bytes-level parity: int() tolerates surrounding whitespace.
	t.Setenv(name, " 42\n")
	if got := EnvInt(name, 86400); got != 42 {
		t.Fatalf("whitespace: got %d, want 42", got)
	}
}

func TestEnvFloatParsesAndFallsBack(t *testing.T) {
	const name = "GUARD_TEST_ENV_FLOAT"
	t.Setenv(name, "")
	if got := EnvFloat(name, 1.5); got != 1.5 {
		t.Fatalf("empty: got %v, want 1.5", got)
	}
	t.Setenv(name, "2.25")
	if got := EnvFloat(name, 1.5); got != 2.25 {
		t.Fatalf("set: got %v, want 2.25", got)
	}
	t.Setenv(name, "\t3.5 ")
	if got := EnvFloat(name, 1.5); got != 3.5 {
		t.Fatalf("whitespace: got %v, want 3.5", got)
	}
}

// configTestChildMarker selects which config helper runs in the re-exec'd test binary.
const configTestChildMarker = "GUARD_CONFIG_TEST_CHILD"

// configTestHelperExitPath is the shared child body: in the child process the marker is
// set, and this dispatches to the fail-fast helper under test. In the parent
// process the marker is absent, so this is a no-op.
func configTestHelperExitPath() {
	switch os.Getenv(configTestChildMarker) {
	case "required":
		EnvRequired("GUARD_TEST_REQUIRED_UNSET")
	case "int":
		EnvInt("GUARD_TEST_INT", 0)
	case "float":
		EnvFloat("GUARD_TEST_FLOAT", 0)
	}
}

// runConfigChildExit re-executes the test binary with only `child` selected, sets the
func runConfigChildExit(t *testing.T, child, markerValue string, extraEnv ...string) int {
	t.Helper()
	if os.Getenv(configTestChildMarker) != "" {
		// Already inside a re-exec'd child: configTestHelperExitPath has done its work.
		return 0
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+child+"$")
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if configTestEnvPrefix(kv, configTestChildMarker) {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, configTestChildMarker+"="+markerValue)
	env = append(env, extraEnv...)
	cmd.Env = env
	err := cmd.Run()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	t.Fatalf("child process failed to run: %v", err)
	return -1
}

func configTestEnvPrefix(kv, name string) bool {
	return len(kv) > len(name) && kv[:len(name)+1] == name+"="
}

func TestEnvRequiredExitsOneWhenUnset(t *testing.T) {
	configTestHelperExitPath()
	if code := runConfigChildExit(t, "TestEnvRequiredExitsOneWhenUnset", "required"); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestEnvIntExitsOneOnNonInt(t *testing.T) {
	configTestHelperExitPath()
	if code := runConfigChildExit(t, "TestEnvIntExitsOneOnNonInt", "int", "GUARD_TEST_INT=not-an-int"); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

func TestEnvFloatExitsOneOnNonNumber(t *testing.T) {
	configTestHelperExitPath()
	if code := runConfigChildExit(t, "TestEnvFloatExitsOneOnNonNumber", "float", "GUARD_TEST_FLOAT=not-a-number"); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}

// TestEnvRequiredSucceedsWhenSetAndSelectedAsChild is the child body for a
// success case: marker "none" runs nothing, so exit 0 proves the harness.
func TestEnvRequiredSucceedsWhenSetAndSelectedAsChild(t *testing.T) {
	configTestHelperExitPath()
	if code := runConfigChildExit(t, "TestEnvRequiredSucceedsWhenSetAndSelectedAsChild", "none"); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
}
