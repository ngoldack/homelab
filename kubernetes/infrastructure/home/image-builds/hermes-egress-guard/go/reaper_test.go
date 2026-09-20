package guard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The reaper tests mirror tests/test_reaper.py: one fake Kubernetes client, an
// injected clock, and assertions on statuses, the ledger and the delete calls.

const (
	reaperTestNow       = int64(1_700_000_000)
	reaperTestSession   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	reaperTestEventID   = "eeeeeeeeeeeeeeee"
	reaperTestTTLS      = 86400
	reaperTestNamespace = "hermes-sandbox"
	reaperTestConfigMap = "hermes-quarantine"
)

var reaperTestSecret = []byte("unit-test-secret")

// fakeReaperK8s implements ReaperK8s with the same affordances the Python
// StubK8s exposes: a mutable ConfigMap, recorded deletes, injectable errors and
// an ordered op log (so write-before-delete is observable).
type fakeReaperK8s struct {
	mu sync.Mutex

	claims      []map[string]any
	deleted     []string
	ops         []string
	configMap   map[string]any
	createCalls []map[string]string

	replaceErr    error
	deleteErr     map[string]error
	listErr       error
	selectors     []string
	replaceCalls  int
	sweepSignals  chan struct{}
	lastNamespace string
}

func newFakeReaperK8s() *fakeReaperK8s {
	return &fakeReaperK8s{
		deleteErr:   map[string]error{},
		configMap:   map[string]any{"data": map[string]any{}, "metadata": map[string]any{"resourceVersion": "1"}},
		sweepSignals: make(chan struct{}, 8),
	}
}

func (f *fakeReaperK8s) ListClaims(namespace, labelSelector string) ([]map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selectors = append(f.selectors, labelSelector)
	f.lastNamespace = namespace
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]map[string]any(nil), f.claims...), nil
}

func (f *fakeReaperK8s) DeleteClaim(namespace, name, uid string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "delete:"+name)
	if err, ok := f.deleteErr[name]; ok {
		return false, err
	}
	f.deleted = append(f.deleted, name+"/"+uid)
	return true, nil
}

func (f *fakeReaperK8s) GetConfigMap(namespace, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case f.sweepSignals <- struct{}{}:
	default:
	}
	return f.configMap, nil
}

func (f *fakeReaperK8s) CreateConfigMap(namespace, name string, data map[string]string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make(map[string]string, len(data))
	for key, value := range data {
		copied[key] = value
	}
	f.createCalls = append(f.createCalls, copied)
	f.configMap = map[string]any{
		"data":     copyAsAny(data),
		"metadata": map[string]any{"resourceVersion": "1"},
	}
	return f.configMap, nil
}

func (f *fakeReaperK8s) ReplaceConfigMap(namespace, name string, data map[string]string, resourceVersion string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaceCalls++
	if f.replaceErr != nil {
		err := f.replaceErr
		f.replaceErr = nil
		return nil, err
	}
	f.ops = append(f.ops, "ledger")
	f.configMap = map[string]any{
		"data":     copyAsAny(data),
		"metadata": map[string]any{"resourceVersion": "2"},
	}
	return f.configMap, nil
}

// ledger returns the fake's current ledger as the string map tests assert on.
func (f *fakeReaperK8s) ledger() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.configMap == nil {
		return nil
	}
	raw, _ := f.configMap["data"].(map[string]any)
	ledger := make(map[string]string, len(raw))
	for key, value := range raw {
		text, _ := value.(string)
		ledger[key] = text
	}
	return ledger
}

// opLog returns the ordered ledger-write/delete operations, so a test can
// assert the ledger write happens BEFORE any delete.
func (f *fakeReaperK8s) opLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

// deletedClaims returns the deleted claims as "name/uid": the UID precondition
// is part of the assertion, since a delete without it is a different string.
func (f *fakeReaperK8s) deletedClaims() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}


func copyAsAny(data map[string]string) map[string]any {
	copied := make(map[string]any, len(data))
	for key, value := range data {
		copied[key] = value
	}
	return copied
}

// newTestReaper builds a reaper with an isolated metrics registry so counter
// assertions never see another test's increments.
func newTestReaper(t *testing.T, k8s ReaperK8s) *Reaper {
	t.Helper()
	reaper := NewReaper(ReaperConfig{
		Secret:         reaperTestSecret,
		Namespace:      reaperTestNamespace,
		ConfigMap:      reaperTestConfigMap,
		QuarantineTTLS: reaperTestTTLS,
	}, k8s, NewEventReplayGuard(0, 0))
	reaper.Metrics = newReaperMetrics()
	return reaper
}

func reaperTestEvent(overrides map[string]any) map[string]any {
	event := map[string]any{
		"event_id":    reaperTestEventID,
		"ts":          float64(reaperTestNow),
		"kind":        "kill",
		"session_hash": reaperTestSession,
		"profile":     "python",
		"target_host": "169.254.169.254",
		"target_port": float64(443),
		"reason":      "kill-destination",
		"strikes":     float64(1),
		"source_ip":   "172.20.5.9",
	}
	for key, value := range overrides {
		if value == nil {
			delete(event, key)
			continue
		}
		event[key] = value
	}
	return event
}

func reaperTestBody(t *testing.T, event map[string]any) []byte {
	t.Helper()
	body, err := marshalPythonJSON(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	return body
}

// postEvent signs the event body and calls HandleEvent at the injected clock.
func postEvent(t *testing.T, reaper *Reaper, event map[string]any) *HttpResult {
	t.Helper()
	body := reaperTestBody(t, event)
	now := float64(reaperTestNow)
	return reaper.HandleEvent(body, SignEvent(reaperTestSecret, body), &now)
}

func reaperTestClaims(count int) []map[string]any {
	claims := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		claims = append(claims, map[string]any{
			"metadata": map[string]any{
				"name": "claim-" + string(rune('0'+index)),
				"uid":  "uid-" + string(rune('0'+index)),
			},
		})
	}
	return claims
}

func decodeResult(t *testing.T, res *HttpResult) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(res.Body, &payload); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	return payload
}

// --- signature gate ---------------------------------------------------------

func TestReaperRejectsUnsignedEvent(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)

	res := reaper.HandleEvent(reaperTestBody(t, reaperTestEvent(nil)), "", nil)

	if res.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Status)
	}
	if got := decodeResult(t, res)["error"]; got != "bad-signature" {
		t.Fatalf("error = %v, want bad-signature", got)
	}
	if len(fake.ledger()) != 0 || len(fake.deletedClaims()) != 0 {
		t.Fatalf("rejected event touched state: ledger=%v deleted=%v", fake.ledger(), fake.deletedClaims())
	}
}

func TestReaperRejectsWrongSignature(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	body := reaperTestBody(t, reaperTestEvent(nil))
	now := float64(reaperTestNow)

	res := reaper.HandleEvent(body, "v1="+strings.Repeat("0", 64), &now)

	if res.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Status)
	}
}

func TestReaperRejectsTamperedBody(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	body := reaperTestBody(t, reaperTestEvent(nil))
	signature := SignEvent(reaperTestSecret, body)
	tampered := []byte(strings.Replace(string(body), "kill-destination", "not-allowlisted", 1))
	now := float64(reaperTestNow)

	res := reaper.HandleEvent(tampered, signature, &now)

	if res.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", res.Status)
	}
}

func TestReaperRejectsUnparseableBody(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	raw := []byte("{not json")
	now := float64(reaperTestNow)

	res := reaper.HandleEvent(raw, SignEvent(reaperTestSecret, raw), &now)

	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Status)
	}
	if got := decodeResult(t, res)["error"]; got != "bad-event" {
		t.Fatalf("error = %v, want bad-event", got)
	}
}

func TestReaperRejectsNonObjectEvent(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	raw := []byte(`[1, 2, 3]`)
	now := float64(reaperTestNow)

	res := reaper.HandleEvent(raw, SignEvent(reaperTestSecret, raw), &now)

	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Status)
	}
	if got := decodeResult(t, res)["detail"]; got != "expected an object" {
		t.Fatalf("detail = %v, want expected an object", got)
	}
}

// --- deny events ------------------------------------------------------------

func TestReaperCountsDenyWithoutQuarantining(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(map[string]any{"kind": "deny", "reason": "not-allowlisted"}))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if len(fake.ledger()) != 0 || len(fake.deletedClaims()) != 0 {
		t.Fatalf("deny event touched state: ledger=%v deleted=%v", fake.ledger(), fake.deletedClaims())
	}
	if got := reaper.Metrics.Value(metricEgressDeniedTotal, nil); got != 1 {
		t.Fatalf("%s = %v, want 1", metricEgressDeniedTotal, got)
	}
	if got := reaper.Metrics.Value(metricQuarantineTotal, nil); got != 0 {
		t.Fatalf("%s = %v, want 0", metricQuarantineTotal, got)
	}
}

// --- kill events ------------------------------------------------------------

func TestReaperKillQuarantinesSessionAndDeletesClaims(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(2)
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	ledger := fake.ledger()
	if len(ledger) != 1 {
		t.Fatalf("ledger keys = %v, want exactly the session", ledger)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(ledger[reaperTestSession]), &entry); err != nil {
		t.Fatalf("ledger entry is not JSON: %v", err)
	}
	wants := map[string]any{
		"reason":       "kill-destination",
		"strikes":      float64(1),
		"quarantined_at": float64(reaperTestNow),
		"ttl_s":        float64(reaperTestNow + reaperTestTTLS),
		"source_ip":    "172.20.5.9",
		"target":       "169.254.169.254:443",
	}
	for key, want := range wants {
		if got := entry[key]; got != want {
			t.Fatalf("ledger %s = %v, want %v", key, got, want)
		}
	}
	// Deleted by name WITH the UID precondition, in list order.
	if got := fake.deletedClaims(); strings.Join(got, ",") != "claim-0/uid-0,claim-1/uid-1" {
		t.Fatalf("deleted = %v", got)
	}
	if got := fake.selectors[len(fake.selectors)-1]; got != sessionHashLabel+"="+reaperTestSession {
		t.Fatalf("label selector = %q", got)
	}
	if got := reaper.Metrics.Value(metricQuarantineTotal, nil); got != 1 {
		t.Fatalf("%s = %v, want 1", metricQuarantineTotal, got)
	}
}

func TestReaperKillQuarantinesWithoutLiveClaim(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if _, present := fake.ledger()[reaperTestSession]; !present {
		t.Fatalf("ledger = %v, want the session key", fake.ledger())
	}
}

func TestReaperWritesLedgerBeforeDeletingClaims(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(2)
	reaper := newTestReaper(t, fake)

	postEvent(t, reaper, reaperTestEvent(nil))

	ops := fake.opLog()
	if len(ops) != 3 || ops[0] != "ledger" {
		t.Fatalf("ops = %v, want ledger write first", ops)
	}
	if ops[1] != "delete:claim-0" || ops[2] != "delete:claim-1" {
		t.Fatalf("ops = %v, want both deletes after the ledger", ops)
	}
}

func TestReaperSkipsClaimsWithoutIdentity(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = []map[string]any{
		{"metadata": map[string]any{"name": "claim-0"}},                  // no UID
		{"metadata": map[string]any{"uid": "uid-1"}},                     // no name
		{"metadata": map[string]any{"name": "claim-2", "uid": "uid-2"}},  // usable
	}
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if got := fake.deletedClaims(); strings.Join(got, ",") != "claim-2/uid-2" {
		t.Fatalf("deleted = %v, want only the identified claim", got)
	}
}

func TestReaperOneFailingDeleteDoesNotStrandTheOthers(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(2)
	fake.deleteErr["claim-0"] = &K8sApiError{Status: 500, Body: "boom", Method: "DELETE", Path: "claim-0"}
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if got := fake.deletedClaims(); strings.Join(got, ",") != "claim-1/uid-1" {
		t.Fatalf("deleted = %v, want only claim-1", got)
	}
	if _, present := fake.ledger()[reaperTestSession]; !present {
		t.Fatalf("ledger = %v, want the session (admission gate holds)", fake.ledger())
	}
}

func TestReaperRejectsInvalidSessionHash(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(map[string]any{"session_hash": "not a hash"}))

	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Status)
	}
	if len(fake.deletedClaims()) != 0 || len(fake.ledger()) != 0 {
		t.Fatalf("invalid hash touched state: %v / %v", fake.deletedClaims(), fake.ledger())
	}
}

func TestQuarantineEntryIsStableJSON(t *testing.T) {
	entry, err := quarantineEntry(reaperTestEvent(nil), int(reaperTestNow), 0)
	if err != nil {
		t.Fatalf("quarantineEntry: %v", err)
	}

	// Sorted keys and a round trip: exactly the Python
	// `entry == json.dumps(json.loads(entry), sort_keys=True)` invariant. The
	// decode keeps each value's raw token (json.RawMessage), so integers do not
	// round-trip through float64 the way a generic decode would.
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(entry), &decoded); err != nil {
		t.Fatalf("entry is not JSON: %v", err)
	}
	reencoded, err := marshalPythonJSON(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if string(reencoded) != entry {
		t.Fatalf("entry = %s, re-encoded = %s", entry, reencoded)
	}
}

func TestQuarantineEntryUsesTheConfiguredDefaultTTL(t *testing.T) {
	entry, err := quarantineEntry(map[string]any{"target_host": "example.org", "target_port": float64(80)}, 100, 3600)
	if err != nil {
		t.Fatalf("quarantineEntry: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(entry), &decoded); err != nil {
		t.Fatalf("entry is not JSON: %v", err)
	}
	if decoded["ttl_s"] != float64(3700) {
		t.Fatalf("ttl_s = %v, want 3700", decoded["ttl_s"])
	}
	if decoded["reason"] != "unknown" {
		t.Fatalf("reason = %v, want unknown", decoded["reason"])
	}
	if decoded["source_ip"] != nil {
		t.Fatalf("source_ip = %v, want null", decoded["source_ip"])
	}
	if decoded["target"] != "example.org:80" {
		t.Fatalf("target = %v", decoded["target"])
	}
}

// --- replay guard -----------------------------------------------------------

func TestReaperRefusesReplayedEventID(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	reaper := newTestReaper(t, fake)

	first := postEvent(t, reaper, reaperTestEvent(nil))
	second := postEvent(t, reaper, reaperTestEvent(nil))

	if first.Status != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Status)
	}
	if second.Status != http.StatusConflict {
		t.Fatalf("second status = %d, want 409", second.Status)
	}
	if got := decodeResult(t, second)["error"]; got != RejectionReplay {
		t.Fatalf("error = %v, want %s", got, RejectionReplay)
	}
	// The second attempt must not delete anything again.
	if got := fake.deletedClaims(); len(got) != 1 {
		t.Fatalf("deleted = %v, want exactly one claim", got)
	}
}

func TestReaperRefusesEventOutsideReplayWindow(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(map[string]any{"ts": float64(reaperTestNow - 3600)}))

	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.Status)
	}
	if got := decodeResult(t, res)["error"]; got != RejectionStaleTimestamp {
		t.Fatalf("error = %v, want %s", got, RejectionStaleTimestamp)
	}
}

func TestReaperRefusesMissingEventID(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(map[string]any{"event_id": nil}))

	if res.Status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.Status)
	}
	if got := decodeResult(t, res)["error"]; got != RejectionMalformedEventID {
		t.Fatalf("error = %v, want %s", got, RejectionMalformedEventID)
	}
}

// --- kubernetes failure handling -------------------------------------------

func TestReaperKubernetesFailureReturns503AndIsNotRecorded(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	fake.replaceErr = &K8sApiError{Status: 500, Body: "apiserver down", Method: "PUT", Path: reaperTestConfigMap}
	reaper := newTestReaper(t, fake)

	failed := postEvent(t, reaper, reaperTestEvent(nil))
	retried := postEvent(t, reaper, reaperTestEvent(nil))

	if failed.Status != http.StatusServiceUnavailable {
		t.Fatalf("failed status = %d, want 503", failed.Status)
	}
	if got := decodeResult(t, failed)["error"]; got != "kubernetes-unavailable" {
		t.Fatalf("error = %v, want kubernetes-unavailable", got)
	}
	// The retry of the SAME event id is processed (not 409): a Kubernetes
	// failure must not burn the event id.
	if retried.Status != http.StatusOK {
		t.Fatalf("retried status = %d, want 200", retried.Status)
	}
	if _, present := fake.ledger()[reaperTestSession]; !present {
		t.Fatalf("ledger = %v, want the session", fake.ledger())
	}
}

func TestReaperMissingLedgerFailsClosed(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	fake.configMap = nil
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Status)
	}
	if len(fake.deletedClaims()) != 0 {
		t.Fatalf("deleted = %v, want none (ledger is the admission gate)", fake.deletedClaims())
	}
}

func TestReaperMergesConflictingWrite(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	fake.replaceErr = &K8sApiError{Status: http.StatusConflict, Body: "conflict", Method: "PUT", Path: reaperTestConfigMap}
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if _, present := fake.ledger()[reaperTestSession]; !present {
		t.Fatalf("ledger = %v, want the session after the merge", fake.ledger())
	}
	if fake.replaceCalls != 2 {
		t.Fatalf("replace calls = %d, want 2 (initial + merged)", fake.replaceCalls)
	}
}

func TestReaperListClaimsFailureFailsClosed(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.listErr = &K8sApiError{Status: 500, Body: "list down", Method: "GET", Path: "sandboxclaims"}
	reaper := newTestReaper(t, fake)

	res := postEvent(t, reaper, reaperTestEvent(nil))

	if res.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.Status)
	}
	if len(fake.ledger()) != 0 || len(fake.deletedClaims()) != 0 {
		t.Fatalf("failed list touched state: %v / %v", fake.ledger(), fake.deletedClaims())
	}
}

func TestReaperEnsureLedgerCreatesTheEmptyConfigMapWhenAbsent(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.configMap = nil
	reaper := newTestReaper(t, fake)

	if err := reaper.EnsureLedger(); err != nil {
		t.Fatalf("EnsureLedger: %v", err)
	}

	if len(fake.createCalls) != 1 || len(fake.createCalls[0]) != 0 {
		t.Fatalf("create calls = %v, want one empty data map", fake.createCalls)
	}
}

func TestReaperEnsureLedgerKeepsAnExistingConfigMap(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.configMap = map[string]any{"data": map[string]any{"other": "entry"}, "metadata": map[string]any{"resourceVersion": "9"}}
	reaper := newTestReaper(t, fake)

	if err := reaper.EnsureLedger(); err != nil {
		t.Fatalf("EnsureLedger: %v", err)
	}

	if len(fake.createCalls) != 0 {
		t.Fatalf("create calls = %v, want none", fake.createCalls)
	}
	if got := fake.ledger(); len(got) != 1 || got["other"] != "entry" {
		t.Fatalf("ledger = %v, want the existing entry", got)
	}
}

// --- HTTP surface -----------------------------------------------------------

func TestReaperEventsEndpointAcceptsSignedKill(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.claims = reaperTestClaims(1)
	reaper := newTestReaper(t, fake)
	live := httptest.NewServer(ReaperHandler(reaper))
	defer live.Close()

	// The HTTP path uses the wall clock (no injected clock), so the event must
	// sit inside the replay guard's ±300s window.
	body := reaperTestBody(t, reaperTestEvent(map[string]any{"ts": float64(time.Now().Unix())}))
	request, err := http.NewRequest(http.MethodPost, live.URL+"/events", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("x-egress-signature", SignEvent(reaperTestSecret, body))

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, payload)
	}
	if got := string(payload); got != `{"event_id": "eeeeeeeeeeeeeeee", "status": "processed"}` {
		t.Fatalf("body = %s", got)
	}
	if got := fake.deletedClaims(); strings.Join(got, ",") != "claim-0/uid-0" {
		t.Fatalf("deleted = %v", got)
	}
}

func TestReaperEventsEndpointRejectsUnsignedBody(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	live := httptest.NewServer(ReaperHandler(reaper))
	defer live.Close()

	response, err := http.Post(
		live.URL+"/events", "application/json",
		strings.NewReader(string(reaperTestBody(t, reaperTestEvent(nil)))),
	)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.StatusCode)
	}
}

func TestReaperHealthzAndMetricsAreOpen(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	reaper.Metrics.Inc(metricQuarantineTotal, nil)
	live := httptest.NewServer(ReaperHandler(reaper))
	defer live.Close()

	health, err := http.Get(live.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	healthBody, _ := io.ReadAll(health.Body)
	health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", health.StatusCode)
	}
	if got := string(healthBody); got != `{"role": "reaper", "status": "ok"}` {
		t.Fatalf("healthz body = %s", got)
	}

	metrics, err := http.Get(live.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	metricsBody, _ := io.ReadAll(metrics.Body)
	metrics.Body.Close()
	if metrics.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", metrics.StatusCode)
	}
	if got := metrics.Header.Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("metrics content type = %q", got)
	}
	if !strings.Contains(string(metricsBody), metricQuarantineTotal) {
		t.Fatalf("metrics body = %s", metricsBody)
	}
	if !strings.Contains(string(metricsBody), "\n"+metricQuarantineTotal+" 1\n") {
		t.Fatalf("metrics body = %s", metricsBody)
	}
}

func TestReaperUnknownPathIsNotFound(t *testing.T) {
	fake := newFakeReaperK8s()
	reaper := newTestReaper(t, fake)
	live := httptest.NewServer(ReaperHandler(reaper))
	defer live.Close()

	for _, request := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/nope"},
		{http.MethodPost, "/nope"},
		{http.MethodGet, "/events"}, // GET on the event path is not routed
	} {
		req, err := http.NewRequest(request.method, live.URL+request.path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", request.method, request.path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s status = %d, want 404", request.method, request.path, response.StatusCode)
		}
	}
}

// TestBuildReaperServerUsesTheConfiguredAddr pins the entry point's listen
// address, including the IPv6-bracketing JoinHostPort applies.
func TestBuildReaperServerUsesTheConfiguredAddr(t *testing.T) {
	reaper := newTestReaper(t, newFakeReaperK8s())

	server := BuildReaperServer(reaper, 8080, "0.0.0.0")
	if server.Addr != "0.0.0.0:8080" {
		t.Fatalf("addr = %q", server.Addr)
	}
	if server.Handler == nil {
		t.Fatal("handler is nil")
	}
	if got := BuildReaperServer(reaper, 9090, "::1").Addr; got != "[::1]:9090" {
		t.Fatalf("addr = %q, want [::1]:9090", got)
	}
}

// --- ledger TTL sweep (plan 2.E) -------------------------------------------

func TestSweepDropsOnlyExpiredLedgerKeys(t *testing.T) {
	fake := newFakeReaperK8s()
	live := newTestReaper(t, fake)
	fake.configMap = map[string]any{
		"data": map[string]any{
			strings.Repeat("a", 64): mustEntry(t, map[string]any{"ttl_s": float64(3600)}, reaperTestNow-10, reaperTestTTLS),
			strings.Repeat("b", 64): mustEntry(t, map[string]any{"ttl_s": float64(86400)}, reaperTestNow, reaperTestTTLS),
			strings.Repeat("c", 64): mustEntry(t, map[string]any{"ttl_s": float64(0)}, reaperTestNow-7200, 0),
		},
		"metadata": map[string]any{"resourceVersion": "1"},
	}
	now := reaperTestNow

	removed, err := live.SweepExpired(&now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	ledger := fake.ledger()
	if _, present := ledger[strings.Repeat("c", 64)]; present {
		t.Fatalf("expired key survived: %v", ledger)
	}
	if _, present := ledger[strings.Repeat("a", 64)]; !present {
		t.Fatalf("live key dropped: %v", ledger)
	}
	if _, present := ledger[strings.Repeat("b", 64)]; !present {
		t.Fatalf("live key dropped: %v", ledger)
	}
	if got := live.Metrics.Value(metricQuarantineSwept, nil); got != 1 {
		t.Fatalf("%s = %v, want 1", metricQuarantineSwept, got)
	}
}

func TestSweepLeavesLedgerUntouchedWhenNothingExpired(t *testing.T) {
	fake := newFakeReaperK8s()
	live := newTestReaper(t, fake)
	fake.configMap = map[string]any{
		"data": map[string]any{
			strings.Repeat("a", 64): mustEntry(t, map[string]any{"ttl_s": float64(3600)}, reaperTestNow, reaperTestTTLS),
		},
		"metadata": map[string]any{"resourceVersion": "1"},
	}
	now := reaperTestNow

	removed, err := live.SweepExpired(&now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	if _, present := fake.ledger()[strings.Repeat("a", 64)]; !present {
		t.Fatalf("live key dropped: %v", fake.ledger())
	}
	if fake.replaceCalls != 0 {
		t.Fatalf("replace calls = %d, want 0 (nothing to write)", fake.replaceCalls)
	}
}

func TestSweepSkipsMalformedEntries(t *testing.T) {
	fake := newFakeReaperK8s()
	live := newTestReaper(t, fake)
	fake.configMap = map[string]any{
		"data": map[string]any{
			strings.Repeat("a", 64): mustEntry(t, map[string]any{"ttl_s": float64(3600)}, reaperTestNow, reaperTestTTLS),
			"bad":                   "not-json{",
			"nope":                  "5",
		},
		"metadata": map[string]any{"resourceVersion": "1"},
	}
	now := reaperTestNow

	removed, err := live.SweepExpired(&now)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if removed != 0 {
		t.Fatalf("removed = %d, want 0", removed)
	}
	ledger := fake.ledger()
	for _, key := range []string{"bad", "nope"} {
		if _, present := ledger[key]; !present {
			t.Fatalf("malformed key %q was dropped: %v", key, ledger)
		}
	}
}

func TestSweepSkipsOnConflict(t *testing.T) {
	fake := newFakeReaperK8s()
	live := newTestReaper(t, fake)
	fake.configMap = map[string]any{
		"data": map[string]any{
			strings.Repeat("c", 64): mustEntry(t, map[string]any{"ttl_s": float64(0)}, reaperTestNow-7200, 0),
		},
		"metadata": map[string]any{"resourceVersion": "1"},
	}
	fake.replaceErr = &K8sApiError{Status: http.StatusConflict, Body: "conflict", Method: "PUT", Path: reaperTestConfigMap}
	now := reaperTestNow

	removed, err := live.SweepExpired(&now)

	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Fatalf("removed = %d, want 0 (next sweep retries)", removed)
	}
	if _, present := fake.ledger()[strings.Repeat("c", 64)]; !present {
		t.Fatalf("entry dropped despite the conflict: %v", fake.ledger())
	}
	if got := live.Metrics.Value(metricQuarantineSwept, nil); got != 0 {
		t.Fatalf("%s = %v, want 0", metricQuarantineSwept, got)
	}
}

func TestSweepWithoutLedgerIsANoOp(t *testing.T) {
	fake := newFakeReaperK8s()
	fake.configMap = nil
	live := newTestReaper(t, fake)
	now := reaperTestNow

	removed, err := live.SweepExpired(&now)

	if err != nil || removed != 0 {
		t.Fatalf("sweep = %d, %v; want 0, nil", removed, err)
	}
}

// TestSweepForeverRepeatsOnItsOwnInterval proves the daemon wiring main() uses:
// consecutive sweeps happen without a manual call.
func TestSweepForeverRepeatsOnItsOwnInterval(t *testing.T) {
	fake := newFakeReaperK8s()
	live := newTestReaper(t, fake)
	// One-second interval so the repetition is observable without a long test.
	go live.SweepForever(1)

	// Wait for two sweeps (each reads the ledger) with an interval of one
	// second; a stalled loop fails loudly instead of hanging the suite.
	deadline := time.After(5 * time.Second)
	for sweep := 0; sweep < 2; sweep++ {
		select {
		case <-fake.sweepSignals:
		case <-deadline:
			t.Fatalf("sweep %d did not happen within 5s", sweep+1)
		}
	}
	if got := live.Metrics.Value(metricQuarantineSweep, nil); got < 1 {
		t.Fatalf("%s = %v, want at least 1", metricQuarantineSweep, got)
	}
}

// mustEntry renders a ledger value, failing the test on an encode error.
func mustEntry(t *testing.T, event map[string]any, now int64, defaultTTLS int) string {
	t.Helper()
	entry, err := quarantineEntry(event, int(now), defaultTTLS)
	if err != nil {
		t.Fatalf("quarantineEntry: %v", err)
	}
	return entry
}