// Package guard is the Go port of the Python egress_guard package.
//
// This file ports src/egress_guard/metrics.py: thread-safe Prometheus
// text-format counters with a deterministic (sorted) renderer.
//
// Only counters exist here on purpose: the alerting rules consume
// hermes_egress_denied_total and friends, and counter semantics need no
// exposition-format exotica. The renderer is deterministic (sorted series) so
// tests can assert on exact lines.
package guard

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// metricLabel is one (name, value) pair of a rendered label set.
type metricLabel struct {
	name  string
	value string
}

// metricSeries identifies one counter series: a metric name plus its labels,
// kept sorted by (name, value) exactly like the Python tuple.
type metricSeries struct {
	name   string
	labels []metricLabel
}

// canonical returns the map key that identifies this series.
func (s metricSeries) canonical() string {
	var b strings.Builder
	b.WriteString(s.name)
	for _, l := range s.labels {
		b.WriteByte(0x00)
		b.WriteString(l.name)
		b.WriteByte(0x01)
		b.WriteString(l.value)
	}
	return b.String()
}

// sortedLabels mirrors Python's tuple(sorted((str(k), str(v)) ...)).
func sortedLabels(labels map[string]string) []metricLabel {
	if len(labels) == 0 {
		return nil
	}
	out := make([]metricLabel, 0, len(labels))
	for k, v := range labels {
		out = append(out, metricLabel{name: k, value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].value < out[j].value
	})
	return out
}

// seriesLess orders series by (name, labels) as Python tuple comparison does.
func seriesLess(a, b metricSeries) bool {
	if a.name != b.name {
		return a.name < b.name
	}
	for i := 0; i < len(a.labels) && i < len(b.labels); i++ {
		if a.labels[i].name != b.labels[i].name {
			return a.labels[i].name < b.labels[i].name
		}
		if a.labels[i].value != b.labels[i].value {
			return a.labels[i].value < b.labels[i].value
		}
	}
	return len(a.labels) < len(b.labels)
}

// escapeLabelValue escapes a Prometheus label value: backslash, double quote
// and newline (in that order, matching the Python).
func escapeLabelValue(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	value = strings.ReplaceAll(value, "\n", "\\n")
	return value
}

// renderValue renders a counter value: integral values render as exact decimal
// integers (Python str(int(value))), everything else through the shortest
// repr-like form (Python repr(float(value))).
func renderValue(value float64) string {
	if !math.IsInf(value, 0) && !math.IsNaN(value) && value == math.Trunc(value) {
		return strconv.FormatFloat(value, 'f', 0, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// Metrics is a thread-safe registry of Prometheus counters.
type Metrics struct {
	mu     sync.Mutex
	help   map[string]string
	values map[string]float64
	series map[string]metricSeries
}

// NewMetrics returns an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{
		help:   map[string]string{},
		values: map[string]float64{},
		series: map[string]metricSeries{},
	}
}

// Declare registers a counter's HELP text; declared counters render as 0 even
// when never incremented. First declaration wins (setdefault).
func (m *Metrics) Declare(name, helpText string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.help[name]; !ok {
		m.help[name] = helpText
	}
}

// Inc adds 1 to a counter series (Python inc(name, labels)).
func (m *Metrics) Inc(name string, labels map[string]string) {
	m.Add(name, labels, 1)
}

// Add adds amount to a counter series (Python inc(name, labels, amount)).
func (m *Metrics) Add(name string, labels map[string]string, amount float64) {
	s := metricSeries{name: name, labels: sortedLabels(labels)}
	key := s.canonical()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] += amount
	m.series[key] = s
}

// Value returns the current value of a series (0 when unknown).
func (m *Metrics) Value(name string, labels map[string]string) float64 {
	key := metricSeries{name: name, labels: sortedLabels(labels)}.canonical()
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[key]
}

// Render returns the Prometheus text exposition, sorted by (name, labels).
func (m *Metrics) Render() string {
	m.mu.Lock()
	values := make(map[string]float64, len(m.values))
	for k, v := range m.values {
		values[k] = v
	}
	seriesByKey := make(map[string]metricSeries, len(m.series))
	for k, s := range m.series {
		seriesByKey[k] = s
	}
	help := make(map[string]string, len(m.help))
	for k, v := range m.help {
		help[k] = v
	}
	m.mu.Unlock()

	// Declared-but-never-incremented counters render as unlabelled 0.
	for name := range help {
		s := metricSeries{name: name}
		key := s.canonical()
		if _, ok := values[key]; !ok {
			values[key] = 0
			seriesByKey[key] = s
		}
	}

	keys := make([]string, 0, len(seriesByKey))
	for k := range seriesByKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return seriesLess(seriesByKey[keys[i]], seriesByKey[keys[j]])
	})

	lines := make([]string, 0, len(keys)*3)
	current := ""
	for _, k := range keys {
		s := seriesByKey[k]
		if s.name != current {
			if h, ok := help[s.name]; ok {
				lines = append(lines, "# HELP "+s.name+" "+h)
			}
			lines = append(lines, "# TYPE "+s.name+" counter")
			current = s.name
		}
		rendered := renderValue(values[k])
		if len(s.labels) > 0 {
			parts := make([]string, 0, len(s.labels))
			for _, l := range s.labels {
				parts = append(parts, l.name+"=\""+escapeLabelValue(l.value)+"\"")
			}
			lines = append(lines, s.name+"{"+strings.Join(parts, ",")+"} "+rendered)
		} else {
			lines = append(lines, s.name+" "+rendered)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
