// Reaper: consumes signed events, quarantines sessions, deletes claims
// (port of src/egress_guard/reaper.py, plan Unit 3.3 + the 2.E TTL sweep).
//
// `POST /events` accepts ONE event (the authorizer's schema, auth.go) with
// `x-egress-signature: v1=<hex HMAC_SHA256(secret, raw body)>` — verified over
// the RAW received bytes (no re-serialization), then parsed. Replay guard:
// event-id format, ±300 s window, seen-id LRU recorded only AFTER the event was
// processed (EventReplayGuard contract).
//
// Per kill event: resolve the session's SandboxClaims by the
// `workload.hermes.io/session-hash` LABEL via the Kubernetes API, write the
// hermes-quarantine ConfigMap key (namespace/key from env; the ledger is the
// admission gate), then delete every resolved claim with a UID precondition.
// The event's `source_ip` is recorded in the ledger for forensics only — no
// pod-IP -> pod -> claim lookup is performed (the authorizer carries no
// Kubernetes client, and the source address is not a second identity factor).
//
// Metrics: hermes_quarantine_total, hermes_egress_denied_total,
// hermes_events_rejected_total, hermes_quarantine_sweep_total,
// hermes_quarantine_swept_total (metrics.go).
//
// Env (ClientFromEnv + config plumbing):
//
//	EGRESS_HMAC_SECRET (required) | EGRESS_LISTEN_PORT (8080)
//	EGRESS_QUARANTINE_NAMESPACE (hermes-sandbox) | EGRESS_QUARANTINE_CONFIGMAP (hermes-quarantine)
package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// sessionHashLabel is the SandboxClaim label naming the session a claim belongs
// to; the reaper resolves a session's claims by it (Python: SESSION_HASH_LABEL).
const sessionHashLabel = "workload.hermes.io/session-hash"

// Metric names, declared once below and referenced by the handlers so a typo
// cannot silently create an undeclared series.
const (
	metricQuarantineTotal     = "hermes_quarantine_total"
	metricEgressDeniedTotal   = "hermes_egress_denied_total"
	metricEventsRejectedTotal = "hermes_events_rejected_total"
	metricQuarantineSweep     = "hermes_quarantine_sweep_total"
	metricQuarantineSwept     = "hermes_quarantine_swept_total"
)

// reaperMetrics is the module registry the reaper declares its counters on,
// mirroring Python's module-level METRICS (reaper.py). A Reaper built without an
// explicit Metrics field uses this one, so the /metrics endpoint always renders
// the declared set.
var reaperMetrics = newReaperMetrics()

// newReaperMetrics returns a registry with every counter the reaper exposes
// declared, so an untouched counter still renders as an explicit 0.
func newReaperMetrics() *Metrics {
	metrics := NewMetrics()
	metrics.Declare(metricQuarantineTotal, "Sessions quarantined by the reaper.")
	metrics.Declare(metricEgressDeniedTotal, "Egress deny events consumed.")
	metrics.Declare(metricEventsRejectedTotal, "Events rejected (signature/replay/chain).")
	metrics.Declare(metricQuarantineSweep, "Ledger TTL sweeps performed by the reaper.")
	metrics.Declare(metricQuarantineSwept, "Expired ledger entries dropped by the reaper.")
	return metrics
}

// ReaperConfig carries the reaper settings. Mirrors the Python frozen dataclass;
// ReaperMain fills it from the environment.
type ReaperConfig struct {
	// Secret is the HMAC key events are signed with (EGRESS_HMAC_SECRET).
	Secret []byte
	// Namespace holds the SandboxClaims and the ledger ConfigMap.
	Namespace string
	// ConfigMap is the quarantine ledger name.
	ConfigMap string
	// QuarantineTTLS is the fallback ledger TTL in seconds when an event
	// carries none.
	QuarantineTTLS int
	// ListenPort is the HTTP listen port.
	ListenPort int
	// SweepIntervalS is the ledger TTL sweep period in seconds.
	SweepIntervalS int
}

// ReaperK8s is the subset of the Kubernetes verbs the reaper calls. *K8sClient
// satisfies it; tests inject a fake.
type ReaperK8s interface {
	ListClaims(namespace, labelSelector string) ([]map[string]any, error)
	DeleteClaim(namespace, name, uid string) (bool, error)
	GetConfigMap(namespace, name string) (map[string]any, error)
	CreateConfigMap(namespace, name string, data map[string]string) (map[string]any, error)
	ReplaceConfigMap(namespace, name string, data map[string]string, resourceVersion string) (map[string]any, error)
}

// Reaper consumes signed events and maintains the quarantine ledger.
type Reaper struct {
	Config ReaperConfig
	K8s    ReaperK8s
	Replay *EventReplayGuard
	// Metrics overrides the module registry (test isolation). Nil means the
	// module registry, exactly like the Python module global.
	Metrics *Metrics
}

// NewReaper builds a reaper. Metrics stays nil so the module registry is used;
// a caller that wants its own registry sets the field.
func NewReaper(config ReaperConfig, k8s ReaperK8s, replay *EventReplayGuard) *Reaper {
	return &Reaper{Config: config, K8s: k8s, Replay: replay}
}

// metrics resolves the registry this reaper counts on.
func (r *Reaper) metrics() *Metrics {
	if r.Metrics == nil {
		return reaperMetrics
	}
	return r.Metrics
}

// HandleEvent verifies, parses and routes ONE signed event. Statuses match the
// Python exactly: 401 bad signature, 400 unparseable/non-object body, 409 replay
// rejection, 503 Kubernetes failure (NOT recorded in the replay guard, so the
// authorizer's retry still works), 200 processed.
//
// now is the injected clock; nil means the wall clock.
func (r *Reaper) HandleEvent(body []byte, signature string, now *float64) *HttpResult {
	moment := NowSeconds()
	if now != nil {
		moment = *now
	}
	if !VerifyEvent(r.Config.Secret, body, signature) {
		r.metrics().Inc(metricEventsRejectedTotal, nil)
		return jsonResult(http.StatusUnauthorized, map[string]any{"error": "bad-signature"}, nil)
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		// Python surfaces json.JSONDecodeError's str(); the decoder wording is
		// Go's. Status and the "error" code are the contract.
		r.metrics().Inc(metricEventsRejectedTotal, nil)
		return jsonResult(http.StatusBadRequest, map[string]any{
			"error": "bad-event", "detail": err.Error(),
		}, nil)
	}
	event, ok := decoded.(map[string]any)
	if !ok {
		r.metrics().Inc(metricEventsRejectedTotal, nil)
		return jsonResult(http.StatusBadRequest, map[string]any{
			"error": "bad-event", "detail": "expected an object",
		}, nil)
	}
	if rejection := r.Replay.Check(reaperString(event["event_id"]), event["ts"], moment); rejection != nil {
		r.metrics().Inc(metricEventsRejectedTotal, nil)
		return jsonResult(http.StatusConflict, map[string]any{"error": *rejection}, nil)
	}
	if kind, _ := event["kind"].(string); kind == "kill" {
		if err := r.Quarantine(event, int(moment)); err != nil {
			// Kubernetes failure: NOT recorded in the replay guard, so the
			// authorizer's retry still works (EventReplayGuard contract).
			log.Printf("event %v: kubernetes failure: %v", event["event_id"], err)
			return jsonResult(http.StatusServiceUnavailable, map[string]any{
				"error": "kubernetes-unavailable", "detail": err.Error(),
			}, nil)
		}
	} else {
		r.metrics().Inc(metricEgressDeniedTotal, nil)
	}
	r.Replay.Record(reaperString(event["event_id"]))
	return jsonResult(http.StatusOK, map[string]any{
		"status": "processed", "event_id": event["event_id"],
	}, nil)
}

// Quarantine writes the session's ledger entry and deletes its live claims.
//
// The ledger entry is written BEFORE any delete: it is the admission gate
// (Kyverno denies a claim whose hash is listed), while the deletes are the
// containment leg. A single claim's delete failure does not strand the others —
// the ledger is already written, so a later event or sweep can retry.
func (r *Reaper) Quarantine(event map[string]any, now int) error {
	sessionHash := reaperOptionalString(event["session_hash"])
	if !SessionHashIsValid(sessionHash) {
		return &K8sApiError{
			Status: http.StatusBadRequest,
			Body:   "event session_hash invalid",
			Method: "POST",
			Path:   "/events",
		}
	}
	claims, err := r.K8s.ListClaims(r.Config.Namespace, sessionHashLabel+"="+sessionHash)
	if err != nil {
		return err
	}
	if len(claims) == 0 {
		// No live claim carries the hash: the session may have recycled since
		// the event was minted. Quarantine the hash anyway — the ledger is the
		// admission gate, the delete is the containment leg.
		log.Printf("session %s: no live claims found; quarantining the hash anyway", sessionHash)
	}
	entry, err := quarantineEntry(event, now, r.Config.QuarantineTTLS)
	if err != nil {
		return err
	}
	if err := r.writeLedger(sessionHash, entry); err != nil {
		return err
	}
	deleted := 0
	for _, claim := range claims {
		metadata, _ := claim["metadata"].(map[string]any)
		name := reaperOptionalString(metadata["name"])
		uid := reaperOptionalString(metadata["uid"])
		if name == "" || uid == "" {
			continue
		}
		removed, err := r.K8s.DeleteClaim(r.Config.Namespace, name, uid)
		if err != nil {
			// One claim's failure must not strand the others: the ledger is
			// already written (admission gate holds), so a later event/sweep
			// can retry the delete.
			log.Printf("claim %s delete failed: %v", name, err)
			continue
		}
		if removed {
			deleted++
		}
	}
	if deleted > 0 {
		log.Printf("session %s quarantined: %d claim(s) deleted (%v)", sessionHash, deleted, event["reason"])
	}
	r.metrics().Inc(metricQuarantineTotal, nil)
	return nil
}

// writeLedger adds one key to the quarantine ledger. A 409 (concurrent writer)
// re-reads once and merges: the ledger is add-only per key, so a merge is
// always safe.
func (r *Reaper) writeLedger(sessionHash, entry string) error {
	existing, err := r.K8s.GetConfigMap(r.Config.Namespace, r.Config.ConfigMap)
	if err != nil {
		return err
	}
	if existing == nil {
		return &K8sApiError{
			Status: http.StatusServiceUnavailable,
			Body:   fmt.Sprintf("%s ConfigMap missing (the reaper creates it at startup)", r.Config.ConfigMap),
			Method: "GET",
			Path:   "/events",
		}
	}
	data := ledgerData(existing)
	if _, present := data[sessionHash]; !present {
		data[sessionHash] = entry
	}
	_, conflict := r.K8s.ReplaceConfigMap(r.Config.Namespace, r.Config.ConfigMap, data, ledgerResourceVersion(existing))
	if conflict == nil {
		return nil
	}
	var apiErr *K8sApiError
	if !errors.As(conflict, &apiErr) || apiErr.Status != http.StatusConflict {
		return conflict
	}
	existing, err = r.K8s.GetConfigMap(r.Config.Namespace, r.Config.ConfigMap)
	if err != nil {
		return err
	}
	if existing == nil {
		// The Python's bare `raise` inside the 409 handler: the ledger
		// vanished under us, so the original conflict is the failure.
		return conflict
	}
	data = ledgerData(existing)
	if _, present := data[sessionHash]; !present {
		data[sessionHash] = entry
	}
	_, err = r.K8s.ReplaceConfigMap(
		r.Config.Namespace, r.Config.ConfigMap, data, ledgerResourceVersion(existing),
	)
	return err
}

// SweepExpired drops ledger entries whose ttl_s has passed and returns the count
// removed (plan 2.E). now is the injected clock; nil means the wall clock.
//
// A malformed entry cannot expire by its own clock and is left alone: a corrupt
// ledger must not be silently deleted by the sweeper. A 409 on the write-back
// means a concurrent writer beat us — re-reading and retrying could drop a
// session that was just re-added with a fresh TTL, so the sweep skips and the
// next interval finishes the job.
func (r *Reaper) SweepExpired(now *int64) (int, error) {
	moment := time.Now().Unix()
	if now != nil {
		moment = *now
	}
	existing, err := r.K8s.GetConfigMap(r.Config.Namespace, r.Config.ConfigMap)
	if err != nil {
		return 0, err
	}
	if existing == nil {
		return 0, nil
	}
	data := ledgerData(existing)
	var dropped []string
	for key, value := range data {
		var entry map[string]any
		if err := json.Unmarshal([]byte(value), &entry); err != nil {
			continue
		}
		expiry := int64(reaperIntValue(entry["ttl_s"]))
		if expiry != 0 && expiry <= moment {
			dropped = append(dropped, key)
		}
	}
	if len(dropped) == 0 {
		return 0, nil
	}
	sort.Strings(dropped)
	for _, key := range dropped {
		delete(data, key)
	}
	_, err = r.K8s.ReplaceConfigMap(
		r.Config.Namespace, r.Config.ConfigMap, data, ledgerResourceVersion(existing),
	)
	if err != nil {
		var apiErr *K8sApiError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			log.Printf("ledger sweep 409 (concurrent writer); next sweep retries")
			return 0, nil
		}
		return 0, err
	}
	r.metrics().Add(metricQuarantineSwept, nil, float64(len(dropped)))
	log.Printf("ledger sweep: dropped %d expired key(s): %s", len(dropped), strings.Join(dropped, ", "))
	return len(dropped), nil
}

// SweepForever is the daemon loop target (plan 2.E): sweep, count the sweep,
// sleep. The ledger write-back can fail transiently, so the next interval
// retries; the loop never exits.
func (r *Reaper) SweepForever(intervalS int) {
	interval := intervalS
	if interval < 1 {
		interval = 1
	}
	for {
		if _, err := r.SweepExpired(nil); err != nil {
			log.Printf("ledger sweep failed: %v", err)
		} else {
			r.metrics().Inc(metricQuarantineSweep, nil)
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// EnsureLedger makes sure the empty quarantine ConfigMap exists at startup:
// Kyverno's context lookup consumes it, so a missing ConfigMap is the
// reaper-down state. An existing ledger (with entries) is left untouched.
func (r *Reaper) EnsureLedger() error {
	existing, err := r.K8s.GetConfigMap(r.Config.Namespace, r.Config.ConfigMap)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}
	_, err = r.K8s.CreateConfigMap(r.Config.Namespace, r.Config.ConfigMap, map[string]string{})
	return err
}

// quarantineEntry renders the ConfigMap value for one quarantine: reason,
// strikes and the resolved expiry on one JSON line (Python: quarantine_entry).
//
// The event MAY carry its own ttl_s; when it does not, the reaper's configured
// default applies, so a ledger entry always states a real expiry instead of an
// already-past one. A non-numeric ttl_s/strikes that Python's int() would refuse
// (a 500 there) coerces to 0 here — no producer emits one, and a wrong-but-live
// entry beats an unhandled error path.
func quarantineEntry(event map[string]any, now int, defaultTTLS int) (string, error) {
	ttlSeconds := reaperIntValue(reaperOr(reaperOr(event["ttl_s"], defaultTTLS), 0))
	reason := any("unknown")
	if reaperTruthy(event["reason"]) {
		reason = event["reason"]
	}
	payload := map[string]any{
		"reason":         reason,
		"strikes":        reaperIntValue(reaperOr(event["strikes"], 0)),
		"quarantined_at": now,
		"ttl_s":          now + ttlSeconds,
		"source_ip":      event["source_ip"],
		"target":         reaperString(event["target_host"]) + ":" + reaperString(event["target_port"]),
	}
	body, err := marshalPythonJSON(payload)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// reaperString mirrors Python's str() over the JSON value kinds a decoded event
// can hold: a string passes through, None renders "None" (which the event-id
// grammar then rejects), booleans render "True"/"False" and numbers render like
// the metrics renderer (integral values as exact decimals).
func reaperString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case float64:
		return renderValue(typed)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

// reaperOptionalString mirrors Python's str(value or ""): an absent or empty
// field is the empty string.
func reaperOptionalString(value any) string {
	if !reaperTruthy(value) {
		return ""
	}
	return reaperString(value)
}

// reaperTruthy mirrors Python truthiness for the JSON value kinds in play.
func reaperTruthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	case json.Number:
		return typed.String() != "" && typed.String() != "0"
	default:
		return true
	}
}

// reaperOr mirrors Python's `a or b`.
func reaperOr(value any, fallback any) any {
	if reaperTruthy(value) {
		return value
	}
	return fallback
}

// reaperIntValue mirrors Python's int() for the numeric shapes a decoded event
// or ledger entry can hold, truncating toward zero like Python does.
func reaperIntValue(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case bool:
		if typed {
			return 1
		}
		return 0
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		if number, err := typed.Int64(); err == nil {
			return int(number)
		}
		if number, err := typed.Float64(); err == nil {
			return int(number)
		}
		return 0
	case string:
		text := strings.TrimSpace(typed)
		if number, err := strconv.Atoi(text); err == nil {
			return number
		}
		if number, err := strconv.ParseFloat(text, 64); err == nil {
			return int(number)
		}
		return 0
	default:
		return 0
	}
}

// ledgerData copies a ConfigMap's data to the string map the write verbs take.
// A missing or non-object data field is the empty ledger, matching Python's
// `dict(existing.get("data") or {})`.
func ledgerData(configMap map[string]any) map[string]string {
	data := map[string]string{}
	raw, _ := configMap["data"].(map[string]any)
	for key, value := range raw {
		text, ok := value.(string)
		if !ok {
			text = reaperString(value)
		}
		data[key] = text
	}
	return data
}

// ledgerResourceVersion reads the resourceVersion the write-back asserts; a
// missing one is the empty string (an unconditional replace), as in Python.
func ledgerResourceVersion(configMap map[string]any) string {
	metadata, _ := configMap["metadata"].(map[string]any)
	value, present := metadata["resourceVersion"]
	if !present || value == nil {
		return ""
	}
	return reaperString(value)
}

// reaperServer routes the three reaper endpoints. Path matching is on
// r.URL.Path, which already excludes the query, mirroring the Python's
// `self.path.split("?")[0]`.
type reaperServer struct {
	reaper *Reaper
}

func (s *reaperServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/events":
		body, err := readBody(r)
		if err != nil {
			writeResult(w, badRequestResult(err.Error()))
			return
		}
		writeResult(w, s.reaper.HandleEvent(body, r.Header.Get("x-egress-signature"), nil))
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		writeResult(w, jsonResult(http.StatusOK, map[string]any{"status": "ok", "role": "reaper"}, nil))
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		writeResult(w, &HttpResult{
			Status:      http.StatusOK,
			Body:        []byte(s.reaper.metrics().Render()),
			ContentType: "text/plain; version=0.0.4",
		})
	default:
		writeResult(w, notFoundResult())
	}
}

// ReaperHandler returns the reaper's routing handler.
func ReaperHandler(reaper *Reaper) Handler {
	return &reaperServer{reaper: reaper}
}

// BuildReaperServer builds the blocking HTTP server (container SIGTERM
// terminates it — the kv-broker precedent, no signal hook).
func BuildReaperServer(reaper *Reaper, port int, bind string) *http.Server {
	return &http.Server{
		Addr:              net.JoinHostPort(bind, strconv.Itoa(port)),
		Handler:           ReaperHandler(reaper),
		ReadHeaderTimeout: 30 * time.Second,
	}
}

// ReaperMain is the reaper role entry point (Python: reaper.main). It returns
// the process exit code; EnvRequired/EnvInt exit 1 themselves on bad config.
func ReaperMain(args []string) int {
	_ = args // the Python contract takes argv and ignores it
	config := ReaperConfig{
		Secret:         []byte(EnvRequired("EGRESS_HMAC_SECRET")),
		Namespace:      EnvStr("EGRESS_QUARANTINE_NAMESPACE", "hermes-sandbox"),
		ConfigMap:      EnvStr("EGRESS_QUARANTINE_CONFIGMAP", "hermes-quarantine"),
		QuarantineTTLS: EnvInt("EGRESS_QUARANTINE_TTL_S", 86400),
		ListenPort:     EnvInt("EGRESS_LISTEN_PORT", 8080),
		SweepIntervalS: EnvInt("EGRESS_QUARANTINE_SWEEP_S", 3600),
	}
	reaper := NewReaper(config, ClientFromEnv(), NewEventReplayGuard(0, 0))
	if err := reaper.EnsureLedger(); err != nil {
		// Startup proceeds: the ledger's absence surfaces on the first event
		// (503) and Kyverno's fail-closed policy reports the same condition.
		log.Printf("hermes-quarantine ledger create failed: %v", err)
	} else {
		log.Printf("hermes-quarantine ledger present (ns %s)", config.Namespace)
	}
	go reaper.SweepForever(config.SweepIntervalS)
	log.Printf("ledger TTL sweep started (every %ds)", config.SweepIntervalS)
	server := BuildReaperServer(reaper, config.ListenPort, "0.0.0.0")
	log.Printf(
		"reaper listening on :%d (ledger %s/%s)",
		config.ListenPort, config.Namespace, config.ConfigMap,
	)
	if err := server.ListenAndServe(); err != nil {
		log.Printf("reaper serve: %v", err)
		return 1
	}
	return 0
}