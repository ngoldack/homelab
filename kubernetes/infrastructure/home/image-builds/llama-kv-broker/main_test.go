// Tests for llama-kv-broker.
//
// WHY A STUB UPSTREAM — the whole contract lives in the *order* of calls the
// broker makes against the serving pod (restore before the request, save only
// after the response reached EOF, erase before a cold serve). A stub that records
// its call log and reproduces the server's file side effects asserts exactly that
// without a GPU, a model or a network.
//
// WHY TESTS WAIT FOR THE SAVE — the save is deliberately after the client saw EOF,
// so a response can complete before the broker has persisted anything. Waiting on
// the stub's call log is what makes that ordering testable instead of racy.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const chatRequestBody = `{"model":"local","messages":[{"role":"user","content":"hello there"}]}`

// graceAfterResponse bounds every negative assertion ("this must NOT happen").
const graceAfterResponse = 250 * time.Millisecond

var slotNamePattern = regexp.MustCompile(`^slot-[0-9a-f]{20}\.bin$`)

// stubUpstream answers the three surfaces the broker uses (/props, the slot
// endpoints, chat/completions) and records every call in order.
type stubUpstream struct {
	slotDir string
	srv     *httptest.Server

	mu             sync.Mutex
	calls          []string
	inferring      int
	maxInferring   int
	propsFail      bool
	chatStatus     int
	cachePrompt    bool
	restoreFail    bool
	restoreErrBody bool
	inferGate      chan struct{}
	inferStarted   chan struct{}
	startedOnce    sync.Once
	gateOnce       sync.Once

	// idTask is the serving server's slot task counter as the /slots probe sees
	// it. idTaskAuto keeps it growing, i.e. a server that has not restarted; a
	// test that wants a restart turns that off and drops the value below the last
	// one it reported.
	idTask       int64
	idTaskAuto   bool
	slotListSeen int
}

func newStubUpstream(t *testing.T) *stubUpstream {
	t.Helper()
	s := &stubUpstream{slotDir: t.TempDir(), chatStatus: http.StatusOK, idTask: 40, idTaskAuto: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/props", s.handleProps)
	mux.HandleFunc("/slots", s.handleSlotList)
	mux.HandleFunc("/slots/", s.handleSlots)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// handleSlotList mirrors the live `GET /slots`: one slot whose id_task is the
// server's per-process task counter, which resets on a restart. It is deliberately
// NOT recorded in the call log — it is a probe, not part of the
// restore->infer->save contract the exact call-order assertions describe.
func (s *stubUpstream) handleSlotList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.idTaskAuto {
		s.idTask++
	}
	task := s.idTask
	s.slotListSeen++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `[{"id":0,"id_task":%d,"n_ctx":32768}]`, task)
}

func (s *stubUpstream) handleProps(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fail := s.propsFail
	s.mu.Unlock()
	if fail {
		http.Error(w, "props down", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"build_info":"b10809-test","model_path":"/models/qwen36-35b.gguf",`+
		`"default_generation_settings":{"n_ctx":32768},"chat_template":"test-template"}`)
}

// handleSlots mirrors the server's side effects: save writes the file, erase
// unlinks it, restore only succeeds while it exists.
func (s *stubUpstream) handleSlots(w http.ResponseWriter, r *http.Request) {
	action := r.URL.Query().Get("action")
	if action == "" {
		action = filepath.Base(r.URL.Path)
	}
	var payload struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	fail, errBody := s.restoreFail, s.restoreErrBody
	s.mu.Unlock()

	// The call is recorded only once the side effect has landed: a test that sees
	// it in the log can then rely on the named file already existing (or already
	// being gone), which is what keeps every file assertion deterministic instead
	// of racing the write.
	defer func() {
		s.mu.Lock()
		s.calls = append(s.calls, action+":"+payload.Filename)
		s.mu.Unlock()
	}()

	path := filepath.Join(s.slotDir, payload.Filename)
	switch action {
	case actionRestore:
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":400,"message":"Failed to restore"}}`)
			return
		}
		if errBody {
			io.WriteString(w, `{"error":"restore failed"}`)
			return
		}
		if _, err := os.Stat(path); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"no such slot"}`)
			return
		}
	case actionSave:
		if err := os.WriteFile(path, []byte("kv-state"), 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case actionErase:
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	io.WriteString(w, `{"ok":true}`)
}

func (s *stubUpstream) handleChat(w http.ResponseWriter, r *http.Request) {
	// cache_prompt is what makes the restored prefix actually reusable, so a body
	// without it is a silent regression the call order would not reveal.
	var body struct {
		CachePrompt bool `json:"cache_prompt"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.calls = append(s.calls, "infer")
	if body.CachePrompt {
		s.cachePrompt = true
	}
	s.inferring++
	if s.inferring > s.maxInferring {
		s.maxInferring = s.inferring
	}
	gate, started, status := s.inferGate, s.inferStarted, s.chatStatus
	s.mu.Unlock()

	if started != nil {
		s.startedOnce.Do(func() { close(started) })
	}
	defer func() {
		s.mu.Lock()
		s.inferring--
		s.mu.Unlock()
	}()
	if gate != nil {
		<-gate
	}

	w.WriteHeader(status)
	if status != http.StatusOK {
		io.WriteString(w, `{"error":"upstream down"}`)
	} else {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}
	// Recorded after the body was handed to the server, i.e. before the client can
	// see EOF — that ordering is what makes "saved only after EOF" assertable.
	s.mu.Lock()
	s.calls = append(s.calls, "infer:body-written")
	s.mu.Unlock()
}

func (s *stubUpstream) callLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *stubUpstream) slotListCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slotListSeen
}

// countAction counts the recorded calls of one action, so a test can name the
// regression it guards ("no restore happened") instead of only diffing the log.
func countAction(calls []string, action string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, action+":") {
			n++
		}
	}
	return n
}

// metricsBody fetches the broker's own exposition through the front server.
func metricsBody(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("GET /metrics: Content-Type %q, want text/plain", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET /metrics: read: %v", err)
	}
	return string(body)
}

// settledMetricsBody fetches the exposition once the broker has finished the
// bookkeeping it performs after the client already saw EOF (the save counters and
// the request counter are written then), so exact-value assertions are not racy.
func settledMetricsBody(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	time.Sleep(graceAfterResponse)
	return metricsBody(t, srv)
}

// mustHaveMetricLine asserts an exact sample line, value included: pre-seeded zero
// series make a family-name substring check tautological.
func mustHaveMetricLine(t *testing.T, body, line string) {
	t.Helper()
	if !strings.Contains(body, line+"\n") {
		t.Fatalf("metrics body has no %q line:\n%s", line, body)
	}
}

// expectedMetricFamilies is the whole exposition contract, in render order.
var expectedMetricFamilies = []string{
	"kv_broker_build_info",
	"kv_broker_inflight_transactions",
	"kv_broker_requests_total",
	"kv_broker_restore_seconds",
	"kv_broker_restore_total",
	"kv_broker_save_seconds",
	"kv_broker_save_total",
	"kv_broker_server_restarts_total",
	"kv_broker_slot_bytes",
	"kv_broker_slot_files",
	"kv_broker_sweep_deleted_total",
	"kv_broker_upstream_errors_total",
}

// metricFamilies lists the families in exposition order, which must be sorted.
func metricFamilies(body string) []string {
	var families []string
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			families = append(families, strings.Fields(rest)[0])
		}
	}
	return families
}

func (s *stubUpstream) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
	s.maxInferring = 0
}

func (s *stubUpstream) configure(fn func(*stubUpstream)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *stubUpstream) maxConcurrentInfer() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInferring
}

func (s *stubUpstream) sawCachePrompt() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cachePrompt
}

// releaseInfer unblocks a gated inference. Tests register it with t.Cleanup so a
// failing assertion cannot leave the httptest server waiting on a request that
// would never finish - a failure must be a failure, not a ten-minute hang.
func (s *stubUpstream) releaseInfer() {
	s.mu.Lock()
	gate := s.inferGate
	s.mu.Unlock()
	if gate != nil {
		s.gateOnce.Do(func() { close(gate) })
	}
}

func newTestBroker(t *testing.T, st *stubUpstream, mutate func(*config)) (*broker, *httptest.Server) {
	t.Helper()
	cfg := config{
		upstreamURL: st.srv.URL,
		slotPath:    st.slotDir,
		listenAddr:  ":0",
		imageTag:    "registry.ngoldack.de/llama-p100:0.4.0-p100",
		modelRef:    "qwen36-35b",
		ctxSize:     "32768",
		cacheTypeK:  "q8_0",
		cacheTypeV:  "q8_0",
		flashAttn:   "on",
		propsTTL:    30 * time.Second,
		ttl:         defaultTTL,
		capBytes:    defaultCapBytes,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	b, err := newBroker(cfg)
	if err != nil {
		t.Fatalf("newBroker: %v", err)
	}
	front := httptest.NewServer(b)
	t.Cleanup(front.Close)
	return b, front
}

func doPost(srv *httptest.Server, path, sessionID, body string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(reply), nil
}

func postOK(t *testing.T, srv *httptest.Server, path, sessionID, body string) string {
	t.Helper()
	status, reply, err := doPost(srv, path, sessionID, body)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	if status != http.StatusOK {
		t.Fatalf("POST %s: status %d, body %s", path, status, reply)
	}
	return reply
}

func saveCalls(calls []string) []string {
	var saves []string
	for _, c := range calls {
		if name, ok := strings.CutPrefix(c, actionSave+":"); ok {
			saves = append(saves, name)
		}
	}
	return saves
}

// assertCalls compares the call log once it is known to be complete.
func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("upstream calls:\n got %v\nwant %v", got, want)
	}
}

// waitForCalls blocks until the call log matches want exactly. A log that already
// holds at least as many calls as expected is a mismatch, not something to wait
// out, so a wrong order fails immediately.
func waitForCalls(t *testing.T, st *stubUpstream, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := st.callLog()
		if reflect.DeepEqual(got, want) {
			return
		}
		if len(got) >= len(want) || time.Now().After(deadline) {
			t.Fatalf("upstream calls:\n got %v\nwant %v", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// waitForSave returns the slot file of the next recorded save, blocking because the
// broker persists only after the client already saw the response.
func waitForSave(t *testing.T, st *stubUpstream) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if saves := saveCalls(st.callLog()); len(saves) > 0 {
			return saves[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("no save call recorded within 5s: %v", st.callLog())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func writeSlot(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestSessionKeyStableAndInvalidatedOnConfigChange(t *testing.T) {
	st := newStubUpstream(t)
	_, srv := newTestBroker(t, st, nil)

	postOK(t, srv, "/v1/chat/completions", "session-a", chatRequestBody)
	first := waitForSave(t, st)
	if !slotNamePattern.MatchString(first) {
		t.Fatalf("slot file %q does not match %v", first, slotNamePattern)
	}
	if _, err := os.Stat(filepath.Join(st.slotDir, first)); err != nil {
		t.Fatalf("saved slot file missing: %v", err)
	}
	assertCalls(t, st.callLog(), []string{"infer", "infer:body-written", actionSave + ":" + first})

	// Same conversation, same spec: the same file, and — the server never
	// restarted — it is left alone rather than restored over the live cache.
	st.reset()
	postOK(t, srv, "/v1/chat/completions", "session-a", chatRequestBody)
	waitForCalls(t, st, []string{"infer", "infer:body-written", actionSave + ":" + first})

	// A different conversation neither restores nor reuses that file.
	st.reset()
	postOK(t, srv, "/v1/chat/completions", "session-b", chatRequestBody)
	second := waitForSave(t, st)
	if second == first {
		t.Fatalf("session-b reused session-a's slot file %s", first)
	}
	assertCalls(t, st.callLog(), []string{"infer", "infer:body-written", actionSave + ":" + second})

	// A changed serving spec invalidates the old file instead of restoring it.
	st.reset()
	_, changedSrv := newTestBroker(t, st, func(c *config) { c.ctxSize = "65536" })
	postOK(t, changedSrv, "/v1/chat/completions", "session-a", chatRequestBody)
	changed := waitForSave(t, st)
	if changed == first {
		t.Fatalf("config change kept the same slot file %s", first)
	}
	assertCalls(t, st.callLog(), []string{"infer", "infer:body-written", actionSave + ":" + changed})
}

func TestColdStartNoRestoreAndSaveAfterEOF(t *testing.T) {
	st := newStubUpstream(t)
	_, srv := newTestBroker(t, st, nil)

	reply := postOK(t, srv, "/v1/chat/completions", "cold-session", chatRequestBody)
	if !strings.Contains(reply, `"hi"`) {
		t.Fatalf("upstream reply not forwarded: %s", reply)
	}
	name := waitForSave(t, st)
	// No restore happened, and the save is the last thing the broker did — after
	// the upstream body was complete.
	assertCalls(t, st.callLog(), []string{"infer", "infer:body-written", actionSave + ":" + name})
	if !st.sawCachePrompt() {
		t.Fatal("upstream request did not carry cache_prompt")
	}

	info, err := os.Stat(filepath.Join(st.slotDir, name))
	if err != nil {
		t.Fatalf("slot file not written: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("slot file %s is empty", name)
	}
}

// TestWarmSlotSkipsRestore is the regression guard for the bug this gate exists
// for: restoring on a warm slot is not only useless (the file restore is inert on
// this build) but actively destroys the in-process cache, 130 ms -> 44 s of prompt
// processing. Two identical requests against a server whose task counter only ever
// grows must therefore touch the slot exactly zero times beyond the save.
func TestWarmSlotSkipsRestore(t *testing.T) {
	st := newStubUpstream(t)
	_, srv := newTestBroker(t, st, nil)

	postOK(t, srv, "/v1/chat/completions", "warm-gate", chatRequestBody)
	name := waitForSave(t, st)
	if _, err := os.Stat(filepath.Join(st.slotDir, name)); err != nil {
		t.Fatalf("slot file not written by the first request: %v", err)
	}

	st.reset()
	postOK(t, srv, "/v1/chat/completions", "warm-gate", chatRequestBody)
	waitForCalls(t, st, []string{"infer", "infer:body-written", actionSave + ":" + name})
	if got := countAction(st.callLog(), actionRestore); got != 0 {
		t.Fatalf("warm slot was restored %d time(s), want 0: %v", got, st.callLog())
	}
	if st.slotListCount() < 2 {
		t.Fatalf("/slots was probed %d time(s), want one per transactional request", st.slotListCount())
	}
	mustHaveMetricLine(t, metricsBody(t, srv), `kv_broker_restore_total{result="skipped_warm"} 2`)
}

// TestRestoreOnceAfterServerRestart covers the only case where restoring is both
// necessary and useful: the serving process restarted, so its in-memory KV is gone
// while the slot file is still on disk.
func TestRestoreOnceAfterServerRestart(t *testing.T) {
	st := newStubUpstream(t)
	_, srv := newTestBroker(t, st, nil)

	postOK(t, srv, "/v1/chat/completions", "restart-gate", chatRequestBody)
	name := waitForSave(t, st)

	// The server came back: id_task restarts low while the slot file survives.
	st.configure(func(s *stubUpstream) { s.idTask, s.idTaskAuto = 1, false })
	st.reset()
	postOK(t, srv, "/v1/chat/completions", "restart-gate", chatRequestBody)
	waitForCalls(t, st, []string{
		actionRestore + ":" + name,
		"infer",
		"infer:body-written",
		actionSave + ":" + name,
	})
	body := metricsBody(t, srv)
	mustHaveMetricLine(t, body, "kv_broker_server_restarts_total 1")
	mustHaveMetricLine(t, body, `kv_broker_restore_total{result="ok"} 1`)

	// One shot: the restored slot is warm again, so the next request leaves it be.
	st.configure(func(s *stubUpstream) { s.idTaskAuto = true })
	st.reset()
	postOK(t, srv, "/v1/chat/completions", "restart-gate", chatRequestBody)
	waitForCalls(t, st, []string{"infer", "infer:body-written", actionSave + ":" + name})
	if got := countAction(st.callLog(), actionRestore); got != 0 {
		t.Fatalf("the request after the post-restart one restored again (%d): %v", got, st.callLog())
	}
	mustHaveMetricLine(t, metricsBody(t, srv), "kv_broker_server_restarts_total 1")
}

// TestMetricsEndpoint pins the whole exposition contract: the family set and its
// order, HELP/TYPE presence, the histogram bucket layout, and the counter values a
// completed and a failed request leave behind.
func TestMetricsEndpoint(t *testing.T) {
	st := newStubUpstream(t)
	_, srv := newTestBroker(t, st, nil)

	body := metricsBody(t, srv)
	families := metricFamilies(body)
	if !reflect.DeepEqual(families, expectedMetricFamilies) {
		t.Fatalf("metric families:\n got %v\nwant %v", families, expectedMetricFamilies)
	}
	t.Logf("emitted metric families (%d): %s", len(families), strings.Join(families, " "))
	for _, family := range families {
		if !strings.Contains(body, "# HELP "+family+" ") {
			t.Fatalf("family %s has no HELP line:\n%s", family, body)
		}
	}
	for _, le := range []string{"0.005", "0.01", "0.025", "0.05", "0.1", "0.25", "0.5", "1", "2.5", "5", "10", "+Inf"} {
		if !strings.Contains(body, `kv_broker_save_seconds_bucket{le="`+le+`"}`) {
			t.Fatalf("save histogram has no le=%s bucket:\n%s", le, body)
		}
	}
	mustHaveMetricLine(t, body, `kv_broker_build_info{version="3"} 1`)
	mustHaveMetricLine(t, body, "kv_broker_slot_files 0")

	// A 2xx completion saves its slot: the request and the save are both ok, and
	// the save latency was observed.
	postOK(t, srv, "/v1/chat/completions", "metrics-session", chatRequestBody)
	waitForSave(t, st)
	body = settledMetricsBody(t, srv)
	mustHaveMetricLine(t, body, `kv_broker_save_total{result="ok"} 1`)
	mustHaveMetricLine(t, body, `kv_broker_requests_total{handler="chat_completions",result="ok"} 1`)
	mustHaveMetricLine(t, body, `kv_broker_restore_total{result="skipped_warm"} 1`)
	mustHaveMetricLine(t, body, "kv_broker_save_seconds_count 1")
	mustHaveMetricLine(t, body, `kv_broker_save_seconds_bucket{le="+Inf"} 1`)

	// A failing upstream is never saved, and the reason is exported.
	st.configure(func(s *stubUpstream) { s.chatStatus = http.StatusInternalServerError })
	if status, _, err := doPost(srv, "/v1/chat/completions", "metrics-session", chatRequestBody); err != nil || status != http.StatusInternalServerError {
		t.Fatalf("failing request: status %d err %v", status, err)
	}
	body = settledMetricsBody(t, srv)
	mustHaveMetricLine(t, body, `kv_broker_save_total{result="skipped_status"} 1`)
	mustHaveMetricLine(t, body, `kv_broker_upstream_errors_total{kind="chat"} 1`)
	mustHaveMetricLine(t, body, `kv_broker_requests_total{handler="chat_completions",result="error"} 1`)

	// The plain streaming proxy is instrumented too: /v1/models is not a
	// completion, and the stub does not serve it, so it lands as a proxied
	// upstream error under the v1_other handler.
	if resp, err := srv.Client().Get(srv.URL + "/v1/models"); err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	} else {
		resp.Body.Close()
	}
	body = settledMetricsBody(t, srv)
	mustHaveMetricLine(t, body, `kv_broker_upstream_errors_total{kind="proxy"} 1`)
	mustHaveMetricLine(t, body, `kv_broker_requests_total{handler="v1_other",result="error"} 1`)

	// The metrics surface is counted like any other route, and never forwarded
	// upstream — this is the fifth scrape, so it renders the four before it.
	body = settledMetricsBody(t, srv)
	mustHaveMetricLine(t, body, `kv_broker_requests_total{handler="metrics",result="ok"} 4`)
	for _, c := range st.callLog() {
		if strings.Contains(c, "metrics") {
			t.Fatalf("the /metrics scrape was forwarded upstream: %v", st.callLog())
		}
	}
}

func TestRestoreFailureErasesAndServesCold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpStatus bool
	}{
		{name: "http_error", httpStatus: true},
		{name: "error_field_in_2xx_body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStubUpstream(t)
			_, srv := newTestBroker(t, st, nil)

			postOK(t, srv, "/v1/chat/completions", "restore-session", chatRequestBody)
			name := waitForSave(t, st)

			st.configure(func(s *stubUpstream) {
				s.restoreFail = tc.httpStatus
				s.restoreErrBody = !tc.httpStatus
				// A restore is only attempted after a restart, so the failure path
				// needs one: the server reports a lower id_task than before.
				s.idTask, s.idTaskAuto = 1, false
			})
			st.reset()
			postOK(t, srv, "/v1/chat/completions", "restore-session", chatRequestBody)
			waitForCalls(t, st, []string{
				actionRestore + ":" + name,
				actionErase + ":" + name,
				"infer",
				"infer:body-written",
				actionSave + ":" + name,
			})
			if _, err := os.Stat(filepath.Join(st.slotDir, name)); err != nil {
				t.Fatalf("slot file was not rewritten after the cold serve: %v", err)
			}
		})
	}
}

func TestNon2xxResponseIsNotSaved(t *testing.T) {
	st := newStubUpstream(t)
	st.configure(func(s *stubUpstream) { s.chatStatus = http.StatusInternalServerError })
	_, srv := newTestBroker(t, st, nil)

	status, reply, err := doPost(srv, "/v1/chat/completions", "failing-session", chatRequestBody)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", status, reply)
	}
	if !strings.Contains(reply, "upstream down") {
		t.Fatalf("upstream error body not forwarded: %s", reply)
	}
	time.Sleep(graceAfterResponse)
	assertCalls(t, st.callLog(), []string{"infer", "infer:body-written"})

	entries, err := os.ReadDir(st.slotDir)
	if err != nil {
		t.Fatalf("read slot dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("non-2xx response wrote slots: %v", entries)
	}
}

func TestConcurrentSameSessionSerialized(t *testing.T) {
	st := newStubUpstream(t)
	st.configure(func(s *stubUpstream) {
		s.inferGate = make(chan struct{})
		s.inferStarted = make(chan struct{})
	})
	t.Cleanup(st.releaseInfer)
	_, srv := newTestBroker(t, st, nil)

	results := make([]string, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, reply, err := doPost(srv, "/v1/chat/completions", "racing-session", chatRequestBody)
			results[i] = fmt.Sprintf("status=%d err=%v reply=%s", status, err, reply)
		}(i)
	}

	// The first request is inside the upstream call and holds the broker mutex; a
	// second request must not have reached upstream at all.
	select {
	case <-st.inferStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the upstream inference endpoint")
	}
	if got := st.callLog(); len(got) != 1 || got[0] != "infer" {
		t.Fatalf("a second request reached upstream while the first was in flight: %v", got)
	}

	st.releaseInfer()
	wg.Wait()
	for i, r := range results {
		if !strings.Contains(r, "status=200") || !strings.Contains(r, `"hi"`) {
			t.Fatalf("request %d did not complete cleanly: %s", i, r)
		}
	}
	if max := st.maxConcurrentInfer(); max != 1 {
		t.Fatalf("upstream saw %d concurrent inferences, want 1", max)
	}
	// The slot file is only known afterwards, so the full expectation is asserted
	// once both saves have landed.
	name := waitForSave(t, st)
	// The second transaction can only start after the first one saved, so it is
	// the warm one: it reuses the live cache (no restore) and re-saves its slot.
	waitForCalls(t, st, []string{
		"infer",
		"infer:body-written",
		actionSave + ":" + name,
		"infer",
		"infer:body-written",
		actionSave + ":" + name,
	})
}

func TestSweeper(t *testing.T) {
	t.Run("ttl_delete", func(t *testing.T) {
		st := newStubUpstream(t)
		b, srv := newTestBroker(t, st, func(c *config) { c.ttl = time.Hour })
		expired := writeSlot(t, st.slotDir, "slot-00000000000000000001.bin", "kv")
		fresh := writeSlot(t, st.slotDir, "slot-00000000000000000002.bin", "kv")
		other := filepath.Join(st.slotDir, "keep.txt")
		if err := os.WriteFile(other, []byte("not a slot"), 0o644); err != nil {
			t.Fatalf("write %s: %v", other, err)
		}
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(expired, old, old); err != nil {
			t.Fatalf("chtimes: %v", err)
		}

		b.sweep()

		if _, err := os.Stat(expired); !os.IsNotExist(err) {
			t.Fatalf("expired slot survived the sweep: %v", err)
		}
		for _, path := range []string{fresh, other} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("%s was removed by the sweep: %v", path, err)
			}
		}
		body := metricsBody(t, srv)
		mustHaveMetricLine(t, body, `kv_broker_sweep_deleted_total{reason="ttl"} 1`)
		mustHaveMetricLine(t, body, "kv_broker_slot_files 1")
		mustHaveMetricLine(t, body, "kv_broker_slot_bytes 2")
	})

	t.Run("cap_evicts_oldest_first", func(t *testing.T) {
		st := newStubUpstream(t)
		// 10 bytes of cap against three 8-byte slots: only the newest fits.
		b, srv := newTestBroker(t, st, func(c *config) { c.capBytes = 10 })
		oldest := writeSlot(t, st.slotDir, "slot-00000000000000000001.bin", "01234567")
		middle := writeSlot(t, st.slotDir, "slot-00000000000000000002.bin", "01234567")
		newest := writeSlot(t, st.slotDir, "slot-00000000000000000003.bin", "01234567")
		now := time.Now()
		for i, path := range []string{oldest, middle, newest} {
			ts := now.Add(-time.Duration(3-i) * time.Hour)
			if err := os.Chtimes(path, ts, ts); err != nil {
				t.Fatalf("chtimes %s: %v", path, err)
			}
		}

		b.sweep()

		for _, path := range []string{oldest, middle} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("%s should have been evicted first: %v", path, err)
			}
		}
		if _, err := os.Stat(newest); err != nil {
			t.Fatalf("newest slot was evicted: %v", err)
		}
		body := metricsBody(t, srv)
		mustHaveMetricLine(t, body, `kv_broker_sweep_deleted_total{reason="cap"} 2`)
		mustHaveMetricLine(t, body, "kv_broker_slot_files 1")
		mustHaveMetricLine(t, body, "kv_broker_slot_bytes 8")
	})

	t.Run("in_flight_session_is_not_deleted", func(t *testing.T) {
		st := newStubUpstream(t)
		// A cap of 0 makes any sweep delete every slot file, so a sweep that ran
		// while the transaction held the mutex cannot go unnoticed.
		b, srv := newTestBroker(t, st, func(c *config) { c.capBytes = 0 })

		postOK(t, srv, "/v1/chat/completions", "inflight-session", chatRequestBody)
		name := waitForSave(t, st)
		path := filepath.Join(st.slotDir, name)

		st.configure(func(s *stubUpstream) {
			s.inferGate = make(chan struct{})
			s.inferStarted = make(chan struct{})
		})
		t.Cleanup(st.releaseInfer)
		done := make(chan struct{})
		go func() {
			defer close(done)
			doPost(srv, "/v1/chat/completions", "inflight-session", chatRequestBody)
		}()
		select {
		case <-st.inferStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight request never reached upstream")
		}

		swept := make(chan struct{})
		go func() {
			b.sweep()
			close(swept)
		}()
		select {
		case <-swept:
			t.Fatal("sweep ran while a session was in flight")
		case <-time.After(graceAfterResponse):
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("in-flight slot file was removed: %v", err)
		}

		st.releaseInfer()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight request never completed")
		}
		select {
		case <-swept:
		case <-time.After(5 * time.Second):
			t.Fatal("sweep never completed after the transaction")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("sweep with cap 0 did not evict %s after the transaction: %v", name, err)
		}
	})
}
