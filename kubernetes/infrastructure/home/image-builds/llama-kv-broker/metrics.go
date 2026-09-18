// Prometheus-text metrics for llama-kv-broker.
//
// WHY HAND-ROLLED — the module is stdlib-only by contract (the image builds with
// CGO_ENABLED=0 and there is no go.sum), so client_golang is not an option. The
// series set is fixed and small, which keeps a text renderer honest.
//
// WHY EVERY LABEL COMBINATION IS PRE-SEEDED — a series that disappears when it has
// no observations cannot be told apart from a broker that never implemented it, and
// rendering straight from live maps would make the exposition order depend on map
// iteration. The zero-valued seeds plus sorted sample lines make every scrape
// reproducible and the tests' line assertions meaningful.
package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metricsContentType is the Prometheus text exposition format; the 0.0.4 version
// string is what the cluster's scrapers expect.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// buildInfoVersion identifies the deployed broker build. It tracks the image tag
// in the build Job (image-builds/llama-kv-broker.yaml); bump it together with the
// tag so a scrape can tell builds apart.
const buildInfoVersion = "3"

// The label values each family can produce. Keeping them as explicit lists is what
// makes the pre-seeding and the deterministic render order possible.
var (
	requestHandlers = []string{"chat_completions", "completions", "healthz", "metrics", "not_found", "rejected", "v1_other"}
	requestResults  = []string{"error", "ok"}
	restoreResults  = []string{"error", "ok", "skipped_no_file", "skipped_warm"}
	saveResults     = []string{"error", "ok", "skipped_no_eof", "skipped_status"}
	upstreamKinds   = []string{"chat", "props", "proxy", "slots"}
	sweepReasons    = []string{"cap", "ttl"}
	// latencyBuckets is the fixed layout for the slot-operation histograms: the
	// interesting range runs from a warm in-process restore (single-digit
	// milliseconds) to a cold server writing a large slot (tens of seconds).
	latencyBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}
)

// vec is a label-keyed counter family. Its values map is guarded by metrics.mu;
// the vec itself holds no lock.
type vec struct {
	name   string
	labels []string
	values map[string]uint64
}

// newVec pre-seeds one sample per combination, rendered later as
// `name{labels[0]="combo[0]",...}`.
func newVec(name string, labels []string, combos ...[]string) *vec {
	v := &vec{name: name, labels: labels, values: make(map[string]uint64, len(combos))}
	for _, combo := range combos {
		v.values[strings.Join(combo, "\x00")] = 0
	}
	return v
}

func (v *vec) inc(values ...string) {
	v.values[strings.Join(values, "\x00")]++
}

// samples renders every pre-seeded sample, sorted so the order never depends on
// map iteration. Sorting is safe for label sets because each key is rendered
// exactly once.
func (v *vec) samples() []string {
	out := make([]string, 0, len(v.values))
	for key, n := range v.values {
		parts := strings.Split(key, "\x00")
		var b strings.Builder
		b.WriteString(v.name)
		b.WriteByte('{')
		for i, label := range v.labels {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `%s="%s"`, label, parts[i])
		}
		b.WriteString("} ")
		b.WriteString(strconv.FormatUint(n, 10))
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}

// histogram is a fixed-bucket latency histogram. counts[i] is the non-cumulative
// observation count for buckets[i]; the last slot is the implicit +Inf bucket.
type histogram struct {
	buckets []float64
	counts  []uint64
	sum     float64
	count   uint64
}

func newHistogram(buckets []float64) histogram {
	return histogram{buckets: buckets, counts: make([]uint64, len(buckets)+1)}
}

func (h *histogram) observe(seconds float64) {
	h.counts[sort.SearchFloat64s(h.buckets, seconds)]++
	h.count++
	h.sum += seconds
}

// samples renders the cumulative buckets followed by _sum and _count, in the
// order the exposition format expects.
func (h *histogram) samples(name string) []string {
	out := make([]string, 0, len(h.buckets)+3)
	var cumulative uint64
	for i, upper := range h.buckets {
		cumulative += h.counts[i]
		out = append(out, fmt.Sprintf(`%s_bucket{le="%s"} %d`, name, formatFloat(upper), cumulative))
	}
	cumulative += h.counts[len(h.buckets)]
	out = append(out,
		fmt.Sprintf(`%s_bucket{le="+Inf"} %d`, name, cumulative),
		fmt.Sprintf("%s_sum %s", name, formatFloat(h.sum)),
		fmt.Sprintf("%s_count %d", name, h.count),
	)
	return out
}

// formatFloat renders a bucket bound the way the exposition format wants it
// (0.005, 0.01, ... 10) instead of scientific notation.
func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// metrics is the broker's entire exposition state behind one mutex; every counter
// family is pre-seeded by newMetrics.
type metrics struct {
	mu sync.Mutex

	requests       *vec
	restores       *vec
	saves          *vec
	upstreamErrors *vec
	sweepDeleted   *vec

	serverRestarts uint64
	slotFiles      int64
	slotBytes      int64
	inflight       int64

	restoreSeconds histogram
	saveSeconds    histogram
}

func newMetrics() *metrics {
	m := &metrics{
		restoreSeconds: newHistogram(latencyBuckets),
		saveSeconds:    newHistogram(latencyBuckets),
	}
	requestCombos := make([][]string, 0, len(requestHandlers)*len(requestResults))
	for _, handler := range requestHandlers {
		for _, result := range requestResults {
			requestCombos = append(requestCombos, []string{handler, result})
		}
	}
	m.requests = newVec("kv_broker_requests_total", []string{"handler", "result"}, requestCombos...)
	m.restores = newVec("kv_broker_restore_total", []string{"result"}, labelCombos(restoreResults)...)
	m.saves = newVec("kv_broker_save_total", []string{"result"}, labelCombos(saveResults)...)
	m.upstreamErrors = newVec("kv_broker_upstream_errors_total", []string{"kind"}, labelCombos(upstreamKinds)...)
	m.sweepDeleted = newVec("kv_broker_sweep_deleted_total", []string{"reason"}, labelCombos(sweepReasons)...)
	return m
}

// labelCombos turns single-label values into combos for newVec.
func labelCombos(values []string) [][]string {
	combos := make([][]string, 0, len(values))
	for _, v := range values {
		combos = append(combos, []string{v})
	}
	return combos
}

func (m *metrics) incRequest(handler, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests.inc(handler, result)
}

func (m *metrics) incRestore(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restores.inc(result)
}

func (m *metrics) incSave(result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saves.inc(result)
}

func (m *metrics) incUpstreamError(kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstreamErrors.inc(kind)
}

func (m *metrics) incSweepDeleted(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sweepDeleted.inc(reason)
}

func (m *metrics) incServerRestart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.serverRestarts++
}

func (m *metrics) setSlotStats(files, bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slotFiles, m.slotBytes = files, bytes
}

func (m *metrics) incInflight() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight++
}

func (m *metrics) decInflight() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inflight--
}

func (m *metrics) observeRestore(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreSeconds.observe(d.Seconds())
}

func (m *metrics) observeSave(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveSeconds.observe(d.Seconds())
}

// serveHTTP answers GET /metrics. The family order is alphabetical and every
// sample order is fixed, so two scrapes of the same state are identical.
func (m *metrics) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", metricsContentType)
	io.WriteString(w, m.render())
}

func (m *metrics) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var b strings.Builder

	writeFamily(&b, "kv_broker_build_info", "gauge",
		"Build metadata of the running broker; always 1.",
		[]string{fmt.Sprintf("kv_broker_build_info{version=%q} 1", buildInfoVersion)})

	writeFamily(&b, "kv_broker_inflight_transactions", "gauge",
		"Completion requests currently inside the transactional slot path.",
		[]string{"kv_broker_inflight_transactions " + strconv.FormatInt(m.inflight, 10)})

	writeFamily(&b, "kv_broker_requests_total", "counter",
		"Broker requests by handler and result (ok means the response was 2xx).",
		m.requests.samples())

	writeFamily(&b, "kv_broker_restore_seconds", "histogram",
		"Duration of the slot restore calls that were attempted.",
		m.restoreSeconds.samples("kv_broker_restore_seconds"))

	writeFamily(&b, "kv_broker_restore_total", "counter",
		"Slot restore decisions by result (skipped_warm means the live slot was reused).",
		m.restores.samples())

	writeFamily(&b, "kv_broker_save_seconds", "histogram",
		"Duration of the slot save calls that were attempted.",
		m.saveSeconds.samples("kv_broker_save_seconds"))

	writeFamily(&b, "kv_broker_save_total", "counter",
		"Slot save decisions by result.",
		m.saves.samples())

	writeFamily(&b, "kv_broker_server_restarts_total", "counter",
		"Detected serving-server restarts (a slot id_task that went backwards).",
		[]string{"kv_broker_server_restarts_total " + strconv.FormatUint(m.serverRestarts, 10)})

	writeFamily(&b, "kv_broker_slot_bytes", "gauge",
		"Bytes of slot files on disk, refreshed by every sweep scan.",
		[]string{"kv_broker_slot_bytes " + strconv.FormatInt(m.slotBytes, 10)})

	writeFamily(&b, "kv_broker_slot_files", "gauge",
		"Number of slot files on disk, refreshed by every sweep scan.",
		[]string{"kv_broker_slot_files " + strconv.FormatInt(m.slotFiles, 10)})

	writeFamily(&b, "kv_broker_sweep_deleted_total", "counter",
		"Slot files deleted by the sweeper, by reason.",
		m.sweepDeleted.samples())

	writeFamily(&b, "kv_broker_upstream_errors_total", "counter",
		"Upstream call failures by kind.",
		m.upstreamErrors.samples())

	return b.String()
}

func writeFamily(b *strings.Builder, name, kind, help string, samples []string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, help)
	fmt.Fprintf(b, "# TYPE %s %s\n", name, kind)
	for _, sample := range samples {
		b.WriteString(sample)
		b.WriteByte('\n')
	}
}
