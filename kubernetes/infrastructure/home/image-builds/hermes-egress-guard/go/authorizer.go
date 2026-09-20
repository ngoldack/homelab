// Authorizer: the Envoy ext_authz decision point (port of
// src/egress_guard/authorizer.py).
//
// POST /check accepts the AuthorizationRequest JSON Envoy's HTTP ext_authz
// service sends:
//
//	{"attributes": {
//	   "source":  {"address": {"socketAddress": {"address": "10.42.1.9"}}},
//	   "request": {"http": {"method": "CONNECT", "host": "pypi.org:443",
//	                        "path": "/",
//	                        "headers": {"proxy-authorization": "Basic ..."}}}}}
//
// and a flat shape for direct callers (the e2e script, curl, tests):
//
//	{"method": "CONNECT", "target": "pypi.org:443",
//	 "proxy_authorization": "Basic ...", "source_ip": "10.42.1.9", "bytes": 4096}
//
// Decision protocol (fixed contract): 200 allow, 403 deny (counts a strike),
// 403 + x-egress-kill: 1 quarantine now. Every decision carries
// x-egress-decision/x-egress-reason/x-egress-strikes/x-egress-policy-version,
// plus the verified x-egress-session / x-egress-profile when authenticated.
//
// Every deny and kill decision is reported to the reaper as an HMAC-signed
// event; the authorizer makes no other network call (no DNS, no Kubernetes
// API). GET /healthz exists for probes.
package guard

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DefaultReaperURL is where deny/kill events go unless EGRESS_REAPER_URL
// overrides it (Python: DEFAULT_REAPER_URL). The reaper service listens on 8080.
const DefaultReaperURL = "http://sandbox-reaper.hermes-egress.svc.cluster.local:8080/events"

// CheckRequest is one parsed ext_authz check: the target is already split into
// host and port, headers are lower-cased, and the byte hint is normalized.
// Mirrors the Python frozen dataclass.
type CheckRequest struct {
	Method   string
	Host     string
	Port     int
	Headers  map[string]string
	SourceIP *string
	ByteHint int
}

// LowerHeaders copies a decoded headers object with lower-cased keys and string
// values. A JSON list value keeps its first element (Envoy never sends lists,
// but the Python did this and a hand-rolled caller might).
func LowerHeaders(raw any) map[string]string {
	headers := map[string]string{}
	block, ok := raw.(map[string]any)
	if !ok {
		return headers
	}
	for key, value := range block {
		if list, isList := value.([]any); isList {
			if len(list) > 0 {
				value = list[0]
			} else {
				value = ""
			}
		}
		headers[strings.ToLower(key)] = pythonString(value)
	}
	return headers
}

// pythonString renders a decoded JSON value the way Python's str() would, so a
// number that arrived as JSON 4096 (an int in Python, a float64 here) renders as
// "4096" and not "4096.0".
func pythonString(value any) string {
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
		if !math.IsInf(typed, 0) && !math.IsNaN(typed) && typed == math.Trunc(typed) {
			return strconv.FormatFloat(typed, 'f', 0, 64)
		}
		return pythonFloat(typed)
	case json.Number:
		return typed.String()
	default:
		return fmt.Sprint(typed)
	}
}

// optionalString renders a possibly-absent object member the way Python's
// `obj.get(key) or ""` does: an absent member, JSON null and every other falsy
// value (0, false, "", []) become "", anything else through pythonString.
func optionalString(value any) string {
	if !truthy(value) {
		return ""
	}
	return pythonString(value)
}

// socketAddress digs the peer address out of the Envoy "source" block:
// source.address.socketAddress.address.
func socketAddress(block any) *string {
	top, ok := block.(map[string]any)
	if !ok {
		return nil
	}
	address, ok := top["address"].(map[string]any)
	if !ok {
		return nil
	}
	socket, ok := address["socketAddress"].(map[string]any)
	if !ok {
		return nil
	}
	value, ok := socket["address"].(string)
	if !ok || value == "" {
		return nil
	}
	return &value
}

// SplitHostPort splits "host[:port]" / "[v6][:port]" without resolving
// anything. An absent or empty host comes back as "" (Python returns None); an
// unparseable port falls back to defaultPort.
func SplitHostPort(value string, defaultPort int) (string, int) {
	target := strings.TrimSpace(value)
	if target == "" {
		return "", defaultPort
	}
	if strings.HasPrefix(target, "[") {
		host, rest, _ := strings.Cut(target[1:], "]")
		rest = strings.TrimLeft(rest, ":")
		if rest != "" && isASCIIDigits(rest) {
			if port, ok := parsePort(rest); ok {
				return host, port
			}
		}
		return host, defaultPort
	}
	if strings.Count(target, ":") == 1 {
		host, rawPort, _ := strings.Cut(target, ":")
		if isASCIIDigits(rawPort) {
			if port, ok := parsePort(rawPort); ok {
				return host, port
			}
		}
		return host, defaultPort
	}
	return target, defaultPort
}

// parsePort converts an already-validated digit run to an int. An out-of-range
// run (Python's int() is unbounded) falls back to the default port rather than
// wrapping.
func parsePort(raw string) (int, bool) {
	port, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || port < 0 || port > math.MaxInt32 {
		return 0, false
	}
	return int(port), true
}

// firstNonEmpty returns the first value that Python's `or` would treat as
// truthy for a string, else "".
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// ParseCheckRequest normalizes either ext_authz payload shape into a
// CheckRequest. A payload with no usable target is an error (the caller answers
// 400), mirroring the Python ValueError.
func ParseCheckRequest(payload any) (CheckRequest, error) {
	body, ok := payload.(map[string]any)
	if !ok {
		return CheckRequest{}, fmt.Errorf("check payload must be a JSON object")
	}

	var (
		method   string
		headers  map[string]string
		target   string
		path     string
		sourceIP *string
		rawBytes any
	)

	switch attributes := body["attributes"].(type) {
	case map[string]any:
		http, err := httpBlock(attributes)
		if err != nil {
			return CheckRequest{}, err
		}
		method = optionalString(http["method"])
		headers = LowerHeaders(http["headers"])
		target = firstNonEmpty(optionalString(http["host"]), headers[":authority"])
		path = optionalString(http["path"])
		sourceIP = socketAddress(attributes["source"])
		rawBytes = headers["x-egress-bytes"]
	default:
		method = optionalString(body["method"])
		if method == "" {
			method = "CONNECT"
		}
		headers = LowerHeaders(body["headers"])
		if raw, present := body["proxy_authorization"]; present && truthy(raw) {
			if _, exists := headers["proxy-authorization"]; !exists {
				headers["proxy-authorization"] = pythonString(raw)
			}
		}
		target = firstNonEmpty(optionalString(body["target"]), optionalString(body["host"]))
		path = optionalString(body["path"])
		if value := optionalString(body["source_ip"]); value != "" {
			sourceIP = &value
		}
		rawBytes = body["bytes"]
	}

	defaultPort := 80
	if strings.ToUpper(method) == "CONNECT" {
		defaultPort = 443
	}
	host, port := SplitHostPort(target, defaultPort)
	if host == "" && path != "" {
		// Non-CONNECT proxy requests carry an absolute-form target in :path.
		candidate := path
		if !strings.Contains(path, "//") {
			candidate = "//" + path
		}
		if parsed, err := url.Parse(candidate); err == nil {
			host, port = SplitHostPort(parsed.Host, defaultPort)
		}
	}
	if host == "" {
		return CheckRequest{}, fmt.Errorf("check payload has no target host")
	}
	return CheckRequest{
		Method:   method,
		Host:     host,
		Port:     port,
		Headers:  headers,
		SourceIP: sourceIP,
		ByteHint: ByteHint(rawBytes),
	}, nil
}

// httpBlock pulls attributes.request.http out of the ext_authz payload: an
// absent request/http is an empty block (the "no target host" check then rejects
// it), a present-but-wrong type is a bad request.
//
// The Python spec answered a bare 500 here (AttributeError on a non-object
// request); a 400 is the same rejection with a status Envoy can act on.
func httpBlock(attributes map[string]any) (map[string]any, error) {
	rawRequest, present := attributes["request"]
	if !present || rawRequest == nil {
		return map[string]any{}, nil
	}
	request, ok := rawRequest.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("attributes.request must be an object")
	}
	rawHTTP, present := request["http"]
	if !present || rawHTTP == nil {
		return map[string]any{}, nil
	}
	http, ok := rawHTTP.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("attributes.request.http must be an object")
	}
	return http, nil
}

// truthy mirrors Python truthiness for the values a decoded JSON payload can
// hold.
func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case float64:
		return typed != 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// ByteHint normalizes the optional advisory byte count for the per-session
// budget: a non-numeric, absent or non-positive value is 0.
//
// The Python ran int(str(raw).strip()), so a float literal ("4096.0") yielded 0.
// encoding/json decodes every JSON number to float64, which loses the
// distinction between 4096 and 4096.0; an integral float is therefore honored
// here (the README's intent is that a supplied hint is consumed).
func ByteHint(raw any) int {
	switch typed := raw.(type) {
	case nil:
		return 0
	case string:
		value, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil || value <= 0 {
			return 0
		}
		return value
	case json.Number:
		value, err := strconv.Atoi(typed.String())
		if err != nil || value <= 0 {
			return 0
		}
		return value
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) {
			return 0
		}
		value := int(typed)
		if value <= 0 {
			return 0
		}
		return value
	case int:
		if typed <= 0 {
			return 0
		}
		return typed
	case int64:
		if typed <= 0 {
			return 0
		}
		return int(typed)
	default:
		return 0
	}
}

// BasicCredentials extracts (session_hash, password) from the proxy
// credentials. It returns nil plus the x-egress-reason the authorizer reports
// when authentication could not even be attempted.
func BasicCredentials(headers map[string]string) (*[2]string, string) {
	raw := headers["proxy-authorization"]
	if raw == "" {
		raw = headers["authorization"]
	}
	if raw == "" {
		return nil, TokenMissing
	}
	scheme, encoded, _ := strings.Cut(raw, " ")
	if !strings.EqualFold(scheme, "basic") || strings.TrimSpace(encoded) == "" {
		return nil, TokenInvalid
	}
	// Python base64.b64decode(validate=True) rejects stray characters and bad
	// padding; StdEncoding is the same strict form.
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || !utf8.Valid(decoded) {
		return nil, TokenInvalid
	}
	sessionHash, password, found := strings.Cut(string(decoded), ":")
	if !found || sessionHash == "" || password == "" {
		return nil, TokenInvalid
	}
	return &[2]string{sessionHash, password}, ""
}

// Event is the deny/kill report the reaper consumes. Nullable identities are
// pointers so they serialize as JSON null, exactly like the Python None.
type Event struct {
	EventID     string
	TS          int64
	Kind        string
	SessionHash *string
	Profile     *string
	TargetHost  string
	TargetPort  int
	Reason      string
	Strikes     int
	SourceIP    *string
}

// CompactJSON serializes the event the way the Python sender did:
// json.dumps(event, sort_keys=True, separators=(",", ":")). The reaper verifies
// the HMAC over these exact bytes, so the layout is part of the wire contract.
func (e *Event) CompactJSON() []byte {
	out := make([]byte, 0, 256)
	out = append(out, '{')
	out = appendEventField(out, "event_id", appendPythonString(nil, e.EventID), true)
	out = appendEventField(out, "kind", appendPythonString(nil, e.Kind), false)
	out = appendEventField(out, "profile", appendNullableString(e.Profile), false)
	out = appendEventField(out, "reason", appendPythonString(nil, e.Reason), false)
	out = appendEventField(out, "session_hash", appendNullableString(e.SessionHash), false)
	out = appendEventField(out, "source_ip", appendNullableString(e.SourceIP), false)
	out = appendEventField(out, "strikes", strconv.AppendInt(nil, int64(e.Strikes), 10), false)
	out = appendEventField(out, "target_host", appendPythonString(nil, e.TargetHost), false)
	out = appendEventField(out, "target_port", strconv.AppendInt(nil, int64(e.TargetPort), 10), false)
	out = appendEventField(out, "ts", strconv.AppendInt(nil, e.TS, 10), false)
	return append(out, '}')
}

func appendEventField(out []byte, name string, value []byte, first bool) []byte {
	if !first {
		out = append(out, ',')
	}
	out = appendPythonString(out, name)
	out = append(out, ':')
	return append(out, value...)
}

func appendNullableString(value *string) []byte {
	if value == nil {
		return []byte("null")
	}
	return appendPythonString(nil, *value)
}

// EventSink accepts a deny/kill report. EventEmitter is the production sink; a
// test records instead. Mirrors the duck-typed Python emitter.
type EventSink interface {
	Emit(event *Event) bool
}

// EventTransport posts one event and returns (status, body, error). A transport
// error is retried; an HTTP status is not, except 5xx.
type EventTransport func(method, url string, headers map[string]string, body []byte) (int, []byte, error)

// EventEmitter is the signed event POST to the reaper. It never raises and
// never blocks a decision longer than TimeoutS * Attempts.
type EventEmitter struct {
	URL       string
	Secret    []byte
	TimeoutS  float64
	Attempts  int
	Sleep     func(seconds float64)
	Transport EventTransport
}

// NewEventEmitter builds an emitter with the Python defaults: a 1s timeout, two
// attempts, a 0.2s backoff and the real HTTP transport.
func NewEventEmitter(url string, secret []byte) *EventEmitter {
	return &EventEmitter{URL: url, Secret: secret, TimeoutS: 1.0, Attempts: 2, Sleep: sleepSeconds}
}

func sleepSeconds(seconds float64) {
	if seconds <= 0 {
		return
	}
	time.Sleep(time.Duration(seconds * float64(time.Second)))
}

func (e *EventEmitter) timeout() float64 {
	if e.TimeoutS <= 0 {
		return 1.0
	}
	return e.TimeoutS
}

func (e *EventEmitter) attempts() int {
	if e.Attempts < 1 {
		return 1
	}
	return e.Attempts
}

func (e *EventEmitter) sleep(seconds float64) {
	if e.Sleep == nil {
		sleepSeconds(seconds)
		return
	}
	e.Sleep(seconds)
}

// Emit signs and POSTs the event, retrying transport failures and 5xx once.
func (e *EventEmitter) Emit(event *Event) bool {
	body := event.CompactJSON()
	headers := map[string]string{
		"Content-Type":      "application/json",
		"x-egress-signature": SignEvent(e.Secret, body),
	}
	attempts := e.attempts()
	for attempt := 1; attempt <= attempts; attempt++ {
		status, _, err := e.post(headers, body)
		if err != nil {
			log.Printf("event %s: reaper unreachable (attempt %d/%d): %v", event.EventID, attempt, attempts, err)
			if attempt < attempts {
				e.sleep(0.2)
			}
			continue
		}
		if status >= 200 && status < 300 {
			return true
		}
		if status >= 500 && attempt < attempts {
			log.Printf("event %s: reaper HTTP %d, retrying", event.EventID, status)
			e.sleep(0.2)
			continue
		}
		log.Printf("event %s: reaper rejected with HTTP %d", event.EventID, status)
		return false
	}
	return false
}

func (e *EventEmitter) post(headers map[string]string, body []byte) (int, []byte, error) {
	if e.Transport != nil {
		return e.Transport("POST", e.URL, headers, body)
	}
	request, err := http.NewRequest(http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := &http.Client{Timeout: time.Duration(e.timeout() * float64(time.Second))}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, payload, err
	}
	return response.StatusCode, payload, nil
}

// AuthorizerConfig carries the verifier knobs. Zero Skew/MTTL select the Python
// defaults (60s / 86400s), and a nil Clock uses the wall clock.
type AuthorizerConfig struct {
	Secret       []byte
	TokenSkewS   float64
	TokenMaxTTLS float64
	Clock        func() float64
}

// Authorizer ties the policy engine to token verification and event reporting.
type Authorizer struct {
	Engine       *EgressEngine
	Emitter      EventSink
	Secret       []byte
	TokenSkewS   float64
	TokenMaxTTLS float64
	Clock        func() float64
}

// NewAuthorizer builds an authorizer from an engine, a sink and the verifier
// config. The Python defaults for the token window apply to zero fields.
func NewAuthorizer(engine *EgressEngine, emitter EventSink, cfg AuthorizerConfig) *Authorizer {
	skew := cfg.TokenSkewS
	if skew == 0 {
		skew = DefaultTokenSkewS
	}
	maxTTL := cfg.TokenMaxTTLS
	if maxTTL == 0 {
		maxTTL = DefaultTokenMaxTTLS
	}
	clock := cfg.Clock
	if clock == nil {
		clock = NowSeconds
	}
	return &Authorizer{
		Engine:       engine,
		Emitter:      emitter,
		Secret:       cfg.Secret,
		TokenSkewS:   skew,
		TokenMaxTTLS: maxTTL,
		Clock:        clock,
	}
}

// Check evaluates one ext_authz payload at the injected moment.
func (a *Authorizer) CheckAt(payload any, now float64) *HttpResult {
	request, err := ParseCheckRequest(payload)
	if err != nil {
		return badRequestResult(err.Error())
	}

	credentials, failure := BasicCredentials(request.Headers)
	var token TokenCheck
	if credentials == nil {
		reason := failure
		if reason == "" {
			reason = TokenInvalid
		}
		token = TokenCheck{Reason: reason}
	} else {
		token = VerifyToken(a.Secret, credentials[0], credentials[1], now, a.TokenSkewS, a.TokenMaxTTLS)
	}

	var identity *Identity
	if token.Ok {
		identity = &Identity{SessionHash: token.SessionHash, Profile: token.Profile}
	}

	var decision Decision
	if identity == nil {
		decision = Decision{Kind: Deny, Reason: token.Reason, Strikes: 0, Status: http.StatusForbidden}
	} else {
		decision = a.Engine.EvaluateAt(identity, request.Host, request.Port, request.ByteHint, now)
	}

	headers := map[string]string{
		"x-egress-decision":       decision.Kind,
		"x-egress-reason":         decision.Reason,
		"x-egress-strikes":        strconv.Itoa(decision.Strikes),
		"x-egress-policy-version": a.Engine.Policy.Version,
	}
	if identity != nil {
		headers["x-egress-session"] = identity.SessionHash
		headers["x-egress-profile"] = identity.Profile
	}
	if decision.Kind == Kill {
		headers["x-egress-kill"] = "1"
	}
	if decision.Kind != Allow {
		a.EmitEvent(identity, request, decision, now)
	}
	return jsonResult(decision.Status, map[string]any{
		"decision": decision.Kind,
		"reason":   decision.Reason,
		"strikes":  decision.Strikes,
		"target":   request.Host + ":" + strconv.Itoa(request.Port),
	}, headers)
}

// Check evaluates one ext_authz payload at the authorizer clock's moment.
func (a *Authorizer) Check(payload any) *HttpResult {
	return a.CheckAt(payload, a.Clock())
}

// EmitEvent reports a deny/kill decision to the sink as a signed event. Identity
// is nil for a pre-authentication denial.
func (a *Authorizer) EmitEvent(identity *Identity, request CheckRequest, decision Decision, moment float64) {
	kind := "deny"
	if decision.Kind == Kill {
		kind = "kill"
	}
	event := &Event{
		EventID:    newEventID(),
		TS:         int64(moment),
		Kind:       kind,
		TargetHost: request.Host,
		TargetPort: request.Port,
		Reason:     decision.Reason,
		Strikes:    decision.Strikes,
		SourceIP:   request.SourceIP,
	}
	if identity != nil {
		session, profile := identity.SessionHash, identity.Profile
		event.SessionHash = &session
		event.Profile = &profile
	}
	if a.Emitter != nil {
		a.Emitter.Emit(event)
	}
}

// newEventID is the uuid4().hex the reaper's EVENT_ID_RE expects: 16 random
// bytes, 32 lowercase hex characters.
func newEventID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return hex.EncodeToString(buf[:])
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(sum[:16])
}

// AuthorizerHandler routes the two paths the service answers.
type AuthorizerHandler struct {
	authorizer *Authorizer
}

// NewAuthorizerHandler builds the /check + /healthz handler.
func NewAuthorizerHandler(authorizer *Authorizer) *AuthorizerHandler {
	return &AuthorizerHandler{authorizer: authorizer}
}

// ServeHTTP implements the Python JsonHandler routing: POST /check (400 on an
// unreadable body), GET /healthz, 404 for any other path.
func (h *AuthorizerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		if r.URL.Path != "/check" {
			writeResult(w, notFoundResult())
			return
		}
		payload, err := readJSON(r)
		if err != nil {
			writeResult(w, badRequestResult(err.Error()))
			return
		}
		writeResult(w, h.authorizer.Check(payload))
	case http.MethodGet:
		if r.URL.Path != "/healthz" {
			writeResult(w, notFoundResult())
			return
		}
		writeResult(w, jsonResult(http.StatusOK, map[string]any{
			"status":         "ok",
			"role":           "authorizer",
			"policy_version": h.authorizer.Engine.Policy.Version,
			"profiles":       sortedProfiles(h.authorizer.Engine.Policy),
		}, nil))
	default:
		http.Error(w, "Unsupported method", http.StatusNotImplemented)
	}
}

// sortedProfiles lists the policy's profile names in code-point order; the
// result is always a non-nil slice so an empty policy renders [] and not null.
func sortedProfiles(policy *Policy) []string {
	names := make([]string, 0, len(policy.Profiles))
	for name := range policy.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BuildAuthorizerServer wires the handler onto an HTTP server bound to
// bind:port, mirroring the Python build_server.
func BuildAuthorizerServer(authorizer *Authorizer, port int, bind string) *http.Server {
	if bind == "" {
		bind = "0.0.0.0"
	}
	return &http.Server{
		Addr:              bind + ":" + strconv.Itoa(port),
		Handler:           NewAuthorizerHandler(authorizer),
		ReadHeaderTimeout: 30 * time.Second,
	}
}

// AuthorizerMain is the authorizer process entry point: read the environment,
// load the mounted policy, build the service and serve until terminated.
// Returns the process exit code (the Python main returned 0).
func AuthorizerMain(args []string) int {
	secret := []byte(EnvRequired("EGRESS_HMAC_SECRET"))
	policyPath := EnvStr("EGRESS_POLICY_FILE", "/etc/egress/policy.json")
	policy, err := LoadPolicy(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot load egress policy from %s: %v\n", policyPath, err)
		return 1
	}
	engine, err := NewEgressEngine(policy, EgressEngineConfig{
		StrikeThreshold: EnvInt("EGRESS_STRIKE_THRESHOLD", 3),
		StrikeWindowS:   EnvFloat("EGRESS_STRIKE_WINDOW_S", 60.0),
		MaxSessions:     EnvInt("EGRESS_MAX_SESSIONS", 4096),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot build egress engine: %v\n", err)
		return 1
	}
	emitter := NewEventEmitter(EnvStr("EGRESS_REAPER_URL", DefaultReaperURL), secret)
	authorizer := NewAuthorizer(engine, emitter, AuthorizerConfig{
		Secret:       secret,
		TokenSkewS:   EnvFloat("EGRESS_TOKEN_SKEW_S", DefaultTokenSkewS),
		TokenMaxTTLS: EnvFloat("EGRESS_TOKEN_MAX_TTL_S", DefaultTokenMaxTTLS),
	})
	port := EnvInt("EGRESS_LISTEN_PORT", 8080)
	server := BuildAuthorizerServer(authorizer, port, "0.0.0.0")
	log.Printf(
		"authorizer listening on :%d (policy %s, profiles %v, reaper %s)",
		port, policy.Version, sortedProfiles(policy), emitter.URL,
	)
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("authorizer serve: %v", err)
	}
	return 0
}