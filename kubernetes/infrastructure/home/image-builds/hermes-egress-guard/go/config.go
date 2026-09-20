// Environment plumbing shared by both roles (port of
// src/egress_guard/config.py).
//
// Configuration is fail-fast: a missing required value or an unparseable
// integer aborts startup with a non-zero exit, which surfaces as a
// CrashLoopBackOff instead of a silently misconfigured guard (the plan's
// "wrong key must fail loudly" rule applied to our own config).
package guard

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// EnvStr returns the environment value, or the default when unset or empty.
func EnvStr(name, defaultValue string) string {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		return defaultValue
	}
	return value
}

// EnvRequired returns the environment value, exiting 1 when unset or empty.
func EnvRequired(name string) string {
	value, ok := os.LookupEnv(name)
	if !ok || value == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", name)
		os.Exit(1)
	}
	return value
}

// EnvInt parses the environment value as an integer, exiting 1 on a non-int.
func EnvInt(name string, defaultValue int) int {
	value := EnvStr(name, "")
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be an integer, got %q\n", name, value)
		os.Exit(1)
	}
	return parsed
}

// EnvFloat parses the environment value as a number, exiting 1 on a non-number.
func EnvFloat(name string, defaultValue float64) float64 {
	value := EnvStr(name, "")
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s must be a number, got %q\n", name, value)
		os.Exit(1)
	}
	return parsed
}
