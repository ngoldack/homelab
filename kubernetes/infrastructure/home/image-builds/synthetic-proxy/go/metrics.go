package syntheticproxy

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// Metrics is a small, dependency-free Prometheus registry exposed at /metrics.
//
// WHY hand-rolled: this module is deliberately stdlib-only (see the package doc
// and the Dockerfile), so pulling in the Prometheus client library would add a
// module graph to an image that today resolves entirely from two pinned base
// tags. The exposition is the plain text format, which is trivial to emit.
//
// CARDINALITY / SECRECY: no series is labelled by API key. The key hash is the
// state map's identity; a per-key label would be an unbounded series and would
// copy a credential-derived value into long-term storage. What the operator
// actually needs — how many requests each verdict produced, what the upstream
// answered, how the quota checks fared — is all label-free apart from small
// closed sets.
type Metrics struct {
	upstream string

	mu        sync.Mutex
	requests  map[string]uint64 // verdict -> count
	responses map[string]uint64 // upstream status code -> count
	quota     map[string]uint64 // quota-check result -> count
	probes    map[string]uint64 // probe result -> count
	r429      map[string]uint64 // upstream 429 reason -> count
	durSum    float64
	durCount  uint64
	durBucket []uint64 // aligned with durationBuckets
}

// durationBuckets are the LLM-request latency boundaries (seconds). They span
// "fast refusal" through "long streaming completion".
var durationBuckets = []float64{0.05, 0.25, 1, 5, 15, 60, 300}

// NewMetrics returns an empty registry for the given upstream identity.
func NewMetrics(upstream string) *Metrics {
	return &Metrics{
		upstream:  upstream,
		requests:  map[string]uint64{},
		responses: map[string]uint64{},
		quota:     map[string]uint64{},
		probes:    map[string]uint64{},
		r429:      map[string]uint64{},
		durBucket: make([]uint64, len(durationBuckets)),
	}
}

// IncRequests counts one handled request under its verdict label.
func (m *Metrics) IncRequests(verdict string) {
	m.mu.Lock()
	m.requests[verdict]++
	m.mu.Unlock()
}

// IncResponse counts one upstream response by status code.
func (m *Metrics) IncResponse(code int) {
	m.mu.Lock()
	m.responses[fmt.Sprintf("%d", code)]++
	m.mu.Unlock()
}

// IncQuota counts one /v2/quotas outcome ("ok" or "error").
func (m *Metrics) IncQuota(result string) {
	m.mu.Lock()
	m.quota[result]++
	m.mu.Unlock()
}

// IncProbe counts one reachability-probe outcome ("ok" or "error").
func (m *Metrics) IncProbe(result string) {
	m.mu.Lock()
	m.probes[result]++
	m.mu.Unlock()
}

// Inc429 counts one upstream 429 by its reason: quota_exhausted,
// rate_limited or parallel_limit. These are three different conditions with
// three different correct responses, so they must never be collapsed into one
// counter.
func (m *Metrics) Inc429(reason string) {
	m.mu.Lock()
	m.r429[reason]++
	m.mu.Unlock()
}

// ObserveDuration records one request's wall time into the histogram.
func (m *Metrics) ObserveDuration(seconds float64) {
	m.mu.Lock()
	m.durSum += seconds
	m.durCount++
	for i, b := range durationBuckets {
		if seconds <= b {
			m.durBucket[i]++
		}
	}
	m.mu.Unlock()
}

// Write renders the registry in the Prometheus text exposition format.
// keysTracked is read live so the gauge reflects the store's current size.
func (m *Metrics) Write(w io.Writer, keysTracked int) {
	m.mu.Lock()
	// Snapshot under the lock, render outside it: a scrape must never block a
	// request path, and the render walks maps.
	requests := cloneCounts(m.requests)
	responses := cloneCounts(m.responses)
	quota := cloneCounts(m.quota)
	probes := cloneCounts(m.probes)
	r429 := cloneCounts(m.r429)
	durSum := m.durSum
	durCount := m.durCount
	buckets := append([]uint64(nil), m.durBucket...)
	m.mu.Unlock()

	fmt.Fprintf(w, "# HELP synthetic_proxy_upstream_info Configured upstream identity.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_upstream_info gauge\n")
	fmt.Fprintf(w, "synthetic_proxy_upstream_info{upstream=%q} 1\n", m.upstream)

	fmt.Fprintf(w, "# HELP synthetic_proxy_keys_tracked API keys currently tracked in the state map (keyed by hash, never the raw value).\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_keys_tracked gauge\n")
	fmt.Fprintf(w, "synthetic_proxy_keys_tracked %d\n", keysTracked)

	fmt.Fprintf(w, "# HELP synthetic_proxy_requests_total Requests handled, by verdict.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_requests_total counter\n")
	writeLabeled(w, "synthetic_proxy_requests_total", "verdict", requests)

	fmt.Fprintf(w, "# HELP synthetic_proxy_upstream_responses_total Upstream responses forwarded back, by status code.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_upstream_responses_total counter\n")
	writeLabeled(w, "synthetic_proxy_upstream_responses_total", "code", responses)

	fmt.Fprintf(w, "# HELP synthetic_proxy_quota_checks_total Calls to the upstream quota endpoint, by result.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_quota_checks_total counter\n")
	writeLabeled(w, "synthetic_proxy_quota_checks_total", "result", quota)

	fmt.Fprintf(w, "# HELP synthetic_proxy_probes_total Calls to the upstream reachability probe, by result.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_probes_total counter\n")
	writeLabeled(w, "synthetic_proxy_probes_total", "result", probes)

	fmt.Fprintf(w, "# HELP synthetic_proxy_upstream_429_total Upstream 429 responses by reason. quota_exhausted = the subscription allowance is spent; rate_limited = the generic state that only clears after a long no-poke window; parallel_limit = per-model concurrency (transient, passed through rather than failed over).\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_upstream_429_total counter\n")
	writeLabeled(w, "synthetic_proxy_upstream_429_total", "reason", r429)

	fmt.Fprintf(w, "# HELP synthetic_proxy_request_duration_seconds Request wall time, including the forwarded (possibly streaming) response.\n")
	fmt.Fprintf(w, "# TYPE synthetic_proxy_request_duration_seconds histogram\n")
	for i, b := range durationBuckets {
		// durBucket[i] ALREADY counts observations <= b (ObserveDuration
		// increments every bucket the sample falls into), so emit it directly.
		// Accumulating here would double-count and break cumulative monotonicity.
		fmt.Fprintf(w, "synthetic_proxy_request_duration_seconds_bucket{le=%q} %d\n", formatFloat(b), buckets[i])
	}
	fmt.Fprintf(w, "synthetic_proxy_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", durCount)
	fmt.Fprintf(w, "synthetic_proxy_request_duration_seconds_sum %g\n", durSum)
	fmt.Fprintf(w, "synthetic_proxy_request_duration_seconds_count %d\n", durCount)
}

// writeLabeled emits a counter family with one label, keys sorted so a given
// state always renders identically (diffable, and stable for tests).
func writeLabeled(w io.Writer, name, label string, counts map[string]uint64) {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=%q} %d\n", name, label, k, counts[k])
	}
}

func cloneCounts(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// formatFloat renders a bucket bound the way the exposition format expects
// (no exponent, no trailing zeros beyond what strconv gives us).
func formatFloat(f float64) string {
	return fmt.Sprintf("%g", f)
}
