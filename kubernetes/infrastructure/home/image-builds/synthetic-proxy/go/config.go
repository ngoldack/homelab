// Package syntheticproxy is a stdlib-only reverse proxy that fronts the
// Synthetic LLM API (api.synthetic.new) for the agentgateway data plane.
//
// WHY it exists: Synthetic's 429s are three different conditions — a transient
// parallel-limit (recover as in-flight calls finish; must NOT fail over), a
// generic rate-limit (only clears after a long no-poke window; MUST fail over)
// and a hard quota exhaustion (MUST fail over). Discriminating them from
// response bodies inside the gateway is fragile. This proxy instead asks
// Synthetic's authoritative /v2/quotas endpoint with the SAME API key the
// gateway injects, keeps per-key state (reachable? quota left?), and fails the
// request itself when it has positive evidence of exhaustion, so the gateway's
// existing eviction policy fails the chain over to OpenRouter.
//
// It holds NO secret: the gateway injects the Authorization header; the proxy
// only hashes it (SHA-256) for its state map and forwards the raw value.
package syntheticproxy

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the fully-resolved runtime configuration. Every field has a safe
// default; a malformed override is a hard error (see LoadConfig) so a typo is
// a CrashLoopBackOff, never a silently misconfigured proxy.
type Config struct {
	// Upstream is the Synthetic API base ("https://api.synthetic.new:443").
	Upstream string
	// QuotaPath is the authoritative balance endpoint ("/v2/quotas").
	QuotaPath string
	// HealthPath is the cheap reachability probe ("/v1/models").
	HealthPath string
	// PollInterval throttles the background per-key quota poller.
	PollInterval time.Duration
	// Bind is the listener address ("0.0.0.0").
	Bind string
	// Port is the listener port (8080).
	Port int
	// UnhealthyFor is how long a key stays refused after a bad verdict.
	UnhealthyFor time.Duration
	// UpstreamHeaderTimeout bounds time-to-first-byte from Synthetic. It is
	// what stops a connection that is accepted but never answered from hanging
	// a request indefinitely (and holding a gateway retry slot with no status
	// code to retry on). It bounds ONLY the wait for headers, so a streaming
	// SSE completion is unaffected however long it runs.
	UpstreamHeaderTimeout time.Duration
	// StoreCap bounds the per-key state map.
	StoreCap int
}

// LoadConfig resolves Config from the environment, applying defaults and
// returning an error for any unparseable or out-of-range value.
func LoadConfig() (Config, error) {
	var c Config
	var err error

	if c.Upstream, err = envStr("SYNTHETIC_UPSTREAM", "https://api.synthetic.new:443"); err != nil {
		return Config{}, err
	}
	if c.QuotaPath, err = envStr("SYNTHETIC_QUOTA_PATH", "/v2/quotas"); err != nil {
		return Config{}, err
	}
	if c.HealthPath, err = envStr("HEALTH_PROBE_PATH", "/v1/models"); err != nil {
		return Config{}, err
	}
	if c.Bind, err = envStr("LBIND", "0.0.0.0"); err != nil {
		return Config{}, err
	}

	pollS, err := envInt("QUOTA_POLL_SECONDS", 300)
	if err != nil {
		return Config{}, err
	}
	if pollS < 1 {
		return Config{}, fmt.Errorf("QUOTA_POLL_SECONDS must be >= 1, got %d", pollS)
	}
	c.PollInterval = time.Duration(pollS) * time.Second

	if c.Port, err = envInt("LPORT", 8080); err != nil {
		return Config{}, err
	}
	if c.Port < 1 || c.Port > 65535 {
		return Config{}, fmt.Errorf("LPORT must be 1..65535, got %d", c.Port)
	}

	if c.UnhealthyFor, err = envDur("UNHEALTHY_AFTER", 10*time.Minute); err != nil {
		return Config{}, err
	}
	if c.UnhealthyFor <= 0 {
		return Config{}, fmt.Errorf("UNHEALTHY_AFTER must be > 0, got %s", c.UnhealthyFor)
	}

	if c.UpstreamHeaderTimeout, err = envDur("UPSTREAM_HEADER_TIMEOUT", 60*time.Second); err != nil {
		return Config{}, err
	}
	if c.UpstreamHeaderTimeout <= 0 {
		return Config{}, fmt.Errorf("UPSTREAM_HEADER_TIMEOUT must be > 0, got %s", c.UpstreamHeaderTimeout)
	}

	if c.StoreCap, err = envInt("STATE_CAP", 1024); err != nil {
		return Config{}, err
	}
	if c.StoreCap < 1 {
		return Config{}, fmt.Errorf("STATE_CAP must be >= 1, got %d", c.StoreCap)
	}

	return c, nil
}

// envStr returns the env value or def when unset/empty.
func envStr(key, def string) (string, error) {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v, nil
	}
	return def, nil
}

// envInt parses an integer env value, falling back to def when unset.
func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

// envDur parses a time.Duration env value ("10m", "90s"), falling back to def.
func envDur(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}
