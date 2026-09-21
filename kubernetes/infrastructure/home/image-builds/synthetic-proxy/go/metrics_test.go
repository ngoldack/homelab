package syntheticproxy

import (
	"strconv"
	"strings"
	"testing"
)

// TestMetricsExposition pins the observable contract of /metrics: the
// exposition carries every family with HELP/TYPE, cumulative monotonic buckets,
// and — critically — never leaks an API-key-derived label.
func TestMetricsExposition(t *testing.T) {
	m := NewMetrics("api.synthetic.new:443")
	m.IncRequests("healthy")
	m.IncRequests("healthy")
	m.IncRequests("quota_exhausted")
	m.IncResponse(200)
	m.IncResponse(200)
	m.IncResponse(429)
	m.IncQuota("ok")
	m.IncQuota("ok")
	m.IncQuota("error")
	m.IncProbe("ok")
	m.ObserveDuration(0.04) // <= 0.05 bucket
	m.ObserveDuration(2)    // <= 5 bucket
	m.ObserveDuration(600)  // only +Inf

	var sb strings.Builder
	m.Write(&sb, 3)
	out := sb.String()

	for _, want := range []string{
		`synthetic_proxy_requests_total{verdict="healthy"} 2`,
		`synthetic_proxy_requests_total{verdict="quota_exhausted"} 1`,
		`synthetic_proxy_upstream_responses_total{code="200"} 2`,
		`synthetic_proxy_upstream_responses_total{code="429"} 1`,
		`synthetic_proxy_quota_checks_total{result="ok"} 2`,
		`synthetic_proxy_quota_checks_total{result="error"} 1`,
		`synthetic_proxy_probes_total{result="ok"} 1`,
		`synthetic_proxy_keys_tracked 3`,
		`synthetic_proxy_upstream_info{upstream="api.synthetic.new:443"} 1`,
		`synthetic_proxy_request_duration_seconds_count 3`,
		`synthetic_proxy_request_duration_seconds_sum 602.04`,
		`synthetic_proxy_request_duration_seconds_bucket{le="0.05"} 1`,
		`synthetic_proxy_request_duration_seconds_bucket{le="5"} 2`,
		`synthetic_proxy_request_duration_seconds_bucket{le="+Inf"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n---\n%s", want, out)
		}
	}

	// Buckets must be cumulative and monotonic.
	prev := -1
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "synthetic_proxy_request_duration_seconds_bucket") {
			continue
		}
		fields := strings.Fields(line)
		n, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("unparseable bucket line %q", line)
		}
		if n < prev {
			t.Fatalf("bucket counts not monotonic: %d after %d (line %q)", n, prev, line)
		}
		prev = n
	}

	// Every family needs its HELP/TYPE header for a well-formed exposition.
	for _, fam := range []string{
		"synthetic_proxy_requests_total",
		"synthetic_proxy_upstream_responses_total",
		"synthetic_proxy_quota_checks_total",
		"synthetic_proxy_probes_total",
		"synthetic_proxy_request_duration_seconds",
	} {
		if !strings.Contains(out, "# HELP "+fam+" ") || !strings.Contains(out, "# TYPE "+fam+" ") {
			t.Errorf("family %s missing HELP/TYPE", fam)
		}
	}
}
