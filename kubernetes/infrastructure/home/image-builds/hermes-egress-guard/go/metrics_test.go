package guard

import (
	"strings"
	"sync"
	"testing"
)

func newMetricsForTest() *Metrics {
	m := NewMetrics()
	m.Declare("hermes_quarantine_total", "Sessions quarantined by the reaper.")
	m.Declare("hermes_egress_denied_total", "Egress deny events consumed.")
	m.Declare("hermes_events_rejected_total", "Events rejected (signature/replay/chain).")
	m.Declare("hermes_quarantine_sweep_total", "Ledger TTL sweeps performed by the reaper.")
	m.Declare("hermes_quarantine_swept_total", "Expired ledger entries dropped by the reaper.")
	return m
}

func TestRenderSeedsDeclaredCountersAtZero(t *testing.T) {
	m := newMetricsForTest()
	got := m.Render()
	want := "" +
		"# HELP hermes_egress_denied_total Egress deny events consumed.\n" +
		"# TYPE hermes_egress_denied_total counter\n" +
		"hermes_egress_denied_total 0\n" +
		"# HELP hermes_events_rejected_total Events rejected (signature/replay/chain).\n" +
		"# TYPE hermes_events_rejected_total counter\n" +
		"hermes_events_rejected_total 0\n" +
		"# HELP hermes_quarantine_sweep_total Ledger TTL sweeps performed by the reaper.\n" +
		"# TYPE hermes_quarantine_sweep_total counter\n" +
		"hermes_quarantine_sweep_total 0\n" +
		"# HELP hermes_quarantine_swept_total Expired ledger entries dropped by the reaper.\n" +
		"# TYPE hermes_quarantine_swept_total counter\n" +
		"hermes_quarantine_swept_total 0\n" +
		"# HELP hermes_quarantine_total Sessions quarantined by the reaper.\n" +
		"# TYPE hermes_quarantine_total counter\n" +
		"hermes_quarantine_total 0\n"
	if got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderSortedBySeriesWithLabels(t *testing.T) {
	m := newMetricsForTest()
	m.Inc("hermes_egress_denied_total", nil)
	m.Inc("hermes_egress_denied_total", map[string]string{"reason": "builtin"})
	m.Add("hermes_egress_denied_total", map[string]string{"reason": "builtin"}, 4)
	m.Inc("hermes_guard_unknown_total", map[string]string{"b": "2", "a": "1"})

	got := m.Render()
	want := "" +
		"# HELP hermes_egress_denied_total Egress deny events consumed.\n" +
		"# TYPE hermes_egress_denied_total counter\n" +
		"hermes_egress_denied_total 1\n" +
		"hermes_egress_denied_total{reason=\"builtin\"} 5\n" +
		"# HELP hermes_events_rejected_total Events rejected (signature/replay/chain).\n" +
		"# TYPE hermes_events_rejected_total counter\n" +
		"hermes_events_rejected_total 0\n" +
		"# TYPE hermes_guard_unknown_total counter\n" +
		"hermes_guard_unknown_total{a=\"1\",b=\"2\"} 1\n" +
		"# HELP hermes_quarantine_sweep_total Ledger TTL sweeps performed by the reaper.\n" +
		"# TYPE hermes_quarantine_sweep_total counter\n" +
		"hermes_quarantine_sweep_total 0\n" +
		"# HELP hermes_quarantine_swept_total Expired ledger entries dropped by the reaper.\n" +
		"# TYPE hermes_quarantine_swept_total counter\n" +
		"hermes_quarantine_swept_total 0\n" +
		"# HELP hermes_quarantine_total Sessions quarantined by the reaper.\n" +
		"# TYPE hermes_quarantine_total counter\n" +
		"hermes_quarantine_total 0\n"
	if got != want {
		t.Fatalf("render mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderEscapesLabelValues(t *testing.T) {
	m := NewMetrics()
	m.Inc("hermes_x_total", map[string]string{"k": "a\\b\"c\nd", "z": "plain"})
	got := m.Render()
	want := "# TYPE hermes_x_total counter\nhermes_x_total{k=\"a\\\\b\\\"c\\nd\",z=\"plain\"} 1\n"
	if got != want {
		t.Fatalf("escape mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderFractionalValues(t *testing.T) {
	m := NewMetrics()
	m.Add("hermes_x_total", nil, 1.5)
	m.Add("hermes_y_total", nil, 1073741824)
	got := m.Render()
	if !strings.Contains(got, "hermes_x_total 1.5\n") {
		t.Fatalf("fractional value not rendered as float: %s", got)
	}
	if !strings.Contains(got, "hermes_y_total 1073741824\n") {
		t.Fatalf("integer value not rendered as int: %s", got)
	}
	// Python str(int(1e19)) prints all 19 digits, never exponent notation.
	m.Add("hermes_z_total", nil, 1e19)
	if got := m.Render(); !strings.Contains(got, "hermes_z_total 10000000000000000000\n") {
		t.Fatalf("large integral value not rendered exactly: %s", got)
	}
	// repr(1e-5) is 1e-05 in Python; Go 'g' agrees on the mantissa/exponent form.
	m.Add("hermes_w_total", nil, 1e-5)
	if got := m.Render(); !strings.Contains(got, "hermes_w_total 1e-05\n") {
		t.Fatalf("small fractional value format: %s", got)
	}
}

func TestValueAndFirstDeclareWins(t *testing.T) {
	m := NewMetrics()
	m.Declare("hermes_x_total", "first")
	m.Declare("hermes_x_total", "second")
	m.Inc("hermes_x_total", nil)
	if v := m.Value("hermes_x_total", nil); v != 1 {
		t.Fatalf("value = %v, want 1", v)
	}
	if v := m.Value("hermes_missing_total", nil); v != 0 {
		t.Fatalf("missing value = %v, want 0", v)
	}
	got := m.Render()
	if !strings.Contains(got, "# HELP hermes_x_total first\n") {
		t.Fatalf("declare did not keep the first HELP text: %s", got)
	}
}

func TestConcurrentIncrements(t *testing.T) {
	m := NewMetrics()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Inc("hermes_egress_denied_total", nil)
			}
		}()
	}
	wg.Wait()
	if v := m.Value("hermes_egress_denied_total", nil); v != 1600 {
		t.Fatalf("value = %v, want 1600", v)
	}
}
