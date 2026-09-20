// HTTP-level contract tests for the authorizer service (port of
// tests/test_authorizer_http.py).
//
// test_policy.go covers the rules engine in isolation; this file covers the
// SERVICE: the real net/http server from BuildAuthorizerServer, real HTTP
// round-trips, the Envoy ext_authz payload shape, the token byte layout
// (MintToken -> Proxy-Authorization -> VerifyToken) and the decision headers
// Envoy acts on.
//
// The interesting boundary is authentication: identity is whatever the HMAC
// token says, because the sandbox controls every other header. The tests below
// therefore drive each failure mode (missing, malformed, expired) and assert
// the response carries no session identity.
package guard

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const (
	authzNow           = 1_700_000_000
	authzPolicyVersion = "http-test-1"
)

var (
	authzSecret  = []byte("unit-test-secret")
	authzSession = strings.Repeat("a", 64)
)

// authzRecorder is the stand-in for EventEmitter: it records events instead of
// POSTing them.
type authzRecorder struct {
	events []*Event
}

func (r *authzRecorder) Emit(event *Event) bool {
	r.events = append(r.events, event)
	return true
}

// authzPolicyBody is the two-profile fixture the Python test used.
func authzPolicyBody() map[string]any {
	return map[string]any{
		"version": authzPolicyVersion,
		"profiles": map[string]any{
			"python": map[string]any{
				"allow": []any{
					map[string]any{"host": ".pythonhosted.org", "ports": []any{443}},
					map[string]any{"host": "pypi.org", "ports": []any{443}},
				},
			},
			"offline": map[string]any{"allow": []any{}},
		},
	}
}

// newAuthzFixture builds the authorizer the HTTP tests drive, with a fixed
// clock and a recording sink.
func newAuthzFixture(t *testing.T) (*Authorizer, *authzRecorder, *EgressEngine) {
	t.Helper()
	policy := loadTestPolicy(t, authzPolicyBody())
	engine, err := NewEgressEngine(policy, EgressEngineConfig{StrikeThreshold: 3, StrikeWindowS: 60.0})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}
	recorder := &authzRecorder{}
	authorizer := NewAuthorizer(engine, recorder, AuthorizerConfig{
		Secret: authzSecret,
		Clock:  func() float64 { return authzNow },
	})
	return authorizer, recorder, engine
}

func authzServer(t *testing.T, authorizer *Authorizer) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(NewAuthorizerHandler(authorizer))
	t.Cleanup(server.Close)
	return server
}

// authzPost sends a body to path and returns (status, headers, decoded body).
func authzPost(t *testing.T, server *httptest.Server, path, raw string) (int, http.Header, map[string]any) {
	t.Helper()
	return authzSend(t, server, http.MethodPost, path, raw)
}

func authzGet(t *testing.T, server *httptest.Server, path string) (int, http.Header, map[string]any) {
	t.Helper()
	return authzSend(t, server, http.MethodGet, path, "")
}

func authzSend(t *testing.T, server *httptest.Server, method, path, raw string) (int, http.Header, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if raw == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(raw)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.StatusCode, response.Header, body
}

// authzToken mints a proxy password for the standard session.
func authzToken(session, profile string, expiry int) string {
	token, err := MintToken(authzSecret, session, profile, expiry)
	if err != nil {
		panic(err)
	}
	return token
}

func authzBasic(session, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(session+":"+token))
}

// authzEnvoyPayload builds the AuthorizationRequest Envoy's ext_authz sends.
// Each option mutates the request.http block, mirroring the Python helper's
// named arguments.
func authzEnvoyPayload(target string, port int, options ...func(http map[string]any)) map[string]any {
	httpBlock := map[string]any{
		"method":  "CONNECT",
		"host":    net.JoinHostPort(target, strconv.Itoa(port)),
		"headers": map[string]any{},
	}
	for _, option := range options {
		option(httpBlock)
	}
	return map[string]any{
		"attributes": map[string]any{
			"request": map[string]any{"http": httpBlock},
			"source": map[string]any{
				"address": map[string]any{"socketAddress": map[string]any{"address": "172.20.5.9"}},
			},
		},
	}
}

func authzWithToken(session, token string) func(map[string]any) {
	return func(httpBlock map[string]any) {
		httpBlock["headers"].(map[string]any)["proxy-authorization"] = authzBasic(session, token)
	}
}

func authzWithByteHint(hint int) func(map[string]any) {
	return func(httpBlock map[string]any) {
		httpBlock["headers"].(map[string]any)["x-egress-bytes"] = fmt.Sprintf("%d", hint)
	}
}

func authzPostPayload(t *testing.T, server *httptest.Server, payload map[string]any) (int, http.Header, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return authzPost(t, server, "/check", string(encoded))
}

// --- /check: allow ----------------------------------------------------------

func TestAuthorizerAllowlistedConnectIsAllowed(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	status, headers, body := authzPostPayload(t, server, payload)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got := headers.Get("x-egress-decision"); got != "allow" {
		t.Fatalf("decision = %q, want allow", got)
	}
	if got := headers.Get("x-egress-reason"); got != "allowed" {
		t.Fatalf("reason = %q, want allowed", got)
	}
	if got := headers.Get("x-egress-session"); got != authzSession {
		t.Fatalf("session = %q, want %q", got, authzSession)
	}
	if got := headers.Get("x-egress-profile"); got != "python" {
		t.Fatalf("profile = %q, want python", got)
	}
	if got := headers.Get("x-egress-policy-version"); got != authzPolicyVersion {
		t.Fatalf("policy version = %q, want %q", got, authzPolicyVersion)
	}
	if got := headers.Get("x-egress-kill"); got != "" {
		t.Fatalf("kill header = %q, want absent", got)
	}
	if got, ok := body["target"].(string); !ok || got != "pypi.org:443" {
		t.Fatalf("target = %v, want pypi.org:443", body["target"])
	}
	if len(recorder.events) != 0 {
		t.Fatalf("events = %d, want 0", len(recorder.events))
	}
}

func TestAuthorizerSuffixEntryMatchesSubdomainOverHTTP(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("files.pythonhosted.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusOK || headers.Get("x-egress-decision") != "allow" {
		t.Fatalf("(status, decision) = (%d, %q), want (200, allow)", status, headers.Get("x-egress-decision"))
	}
}

// --- /check: deny and kill --------------------------------------------------

func TestAuthorizerUnapprovedHostIsDeniedWithAStrike(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("example.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	status, headers, body := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if got := headers.Get("x-egress-decision"); got != "deny" {
		t.Fatalf("decision = %q, want deny", got)
	}
	if got := headers.Get("x-egress-reason"); got != "not-allowlisted" {
		t.Fatalf("reason = %q, want not-allowlisted", got)
	}
	if got := headers.Get("x-egress-strikes"); got != "1" {
		t.Fatalf("strikes = %q, want 1", got)
	}
	if got := headers.Get("x-egress-kill"); got != "" {
		t.Fatalf("kill header = %q, want absent", got)
	}
	if strikes, ok := body["strikes"].(float64); !ok || strikes != 1 {
		t.Fatalf("body strikes = %v, want 1", body["strikes"])
	}
	if len(recorder.events) != 1 {
		t.Fatalf("events = %d, want 1", len(recorder.events))
	}
	if recorder.events[0].Kind != "deny" {
		t.Fatalf("event kind = %q, want deny", recorder.events[0].Kind)
	}
	if recorder.events[0].TargetHost != "example.org" {
		t.Fatalf("event target = %q, want example.org", recorder.events[0].TargetHost)
	}
}

func TestAuthorizerManagementPlaneTargetKills(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("169.254.169.254", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if got := headers.Get("x-egress-decision"); got != "kill" {
		t.Fatalf("decision = %q, want kill", got)
	}
	if got := headers.Get("x-egress-kill"); got != "1" {
		t.Fatalf("kill header = %q, want 1", got)
	}
	if got := headers.Get("x-egress-reason"); got != "kill-destination" {
		t.Fatalf("reason = %q, want kill-destination", got)
	}
	if len(recorder.events) != 1 || recorder.events[0].Kind != "kill" {
		t.Fatalf("events = %+v, want one kill event", recorder.events)
	}
}

func TestAuthorizerRepeatedDenialsEscalateToKillOverHTTP(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	type outcome struct {
		decision string
		kill     string
	}
	results := make([]outcome, 0, 3)
	for i := 0; i < 3; i++ {
		payload := authzEnvoyPayload("example.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
		_, headers, _ := authzPostPayload(t, server, payload)
		results = append(results, outcome{headers.Get("x-egress-decision"), headers.Get("x-egress-kill")})
	}

	want := []outcome{{"deny", ""}, {"deny", ""}, {"kill", "1"}}
	for i, got := range results {
		if got != want[i] {
			t.Fatalf("result[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

// --- /check: authentication -------------------------------------------------

func TestAuthorizerMissingTokenIsDeniedWithoutIdentity(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	status, headers, _ := authzPostPayload(t, server, authzEnvoyPayload("pypi.org", 443))

	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if got := headers.Get("x-egress-reason"); got != TokenMissing {
		t.Fatalf("reason = %q, want %q", got, TokenMissing)
	}
	if got := headers.Get("x-egress-session"); got != "" {
		t.Fatalf("session header = %q, want absent", got)
	}
	if got := headers.Get("x-egress-strikes"); got != "0" {
		t.Fatalf("strikes = %q, want 0", got)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("events = %d, want 1", len(recorder.events))
	}
	if recorder.events[0].SessionHash != nil {
		t.Fatalf("event session = %v, want null", *recorder.events[0].SessionHash)
	}
}

func TestAuthorizerMalformedPasswordIsRejected(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, "not-a-token"))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden || headers.Get("x-egress-reason") != TokenInvalid {
		t.Fatalf("(status, reason) = (%d, %q), want (403, %q)", status, headers.Get("x-egress-reason"), TokenInvalid)
	}
}

func TestAuthorizerTokenBoundToAnotherSessionIsRejected(t *testing.T) {
	// The MAC covers the session hash, so a stolen password cannot be reused.
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(strings.Repeat("b", 64), authzToken(authzSession, "python", authzNow+300)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden || headers.Get("x-egress-reason") != TokenInvalid {
		t.Fatalf("(status, reason) = (%d, %q), want (403, %q)", status, headers.Get("x-egress-reason"), TokenInvalid)
	}
}

func TestAuthorizerExpiredTokenIsRejected(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow-600)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden || headers.Get("x-egress-reason") != TokenExpired {
		t.Fatalf("(status, reason) = (%d, %q), want (403, %q)", status, headers.Get("x-egress-reason"), TokenExpired)
	}
	if got := headers.Get("x-egress-session"); got != "" {
		t.Fatalf("session header = %q, want absent", got)
	}
}

func TestAuthorizerUnknownProfileIsDenied(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, authzToken(authzSession, "ruby", authzNow+300)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden || headers.Get("x-egress-reason") != "profile-unknown" {
		t.Fatalf("(status, reason) = (%d, %q), want (403, profile-unknown)", status, headers.Get("x-egress-reason"))
	}
}

func TestAuthorizerOfflineProfileDeniesEveryTarget(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	session := strings.Repeat("c", 64)
	payload := authzEnvoyPayload("pypi.org", 443, authzWithToken(session, authzToken(session, "offline", authzNow+300)))
	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusForbidden || headers.Get("x-egress-decision") != "deny" {
		t.Fatalf("(status, decision) = (%d, %q), want (403, deny)", status, headers.Get("x-egress-decision"))
	}
}

// --- /check: byte budget ----------------------------------------------------

func TestAuthorizerByteBudgetIsConsumedOverHTTP(t *testing.T) {
	authorizer, _, engine := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	profile := engine.Policy.Profiles["python"]
	if profile.BudgetBytes != nil {
		t.Fatalf("the shipped python profile must have no budget")
	}
	// Enforce one through the engine the service holds, then drive it over HTTP.
	budget := 1000
	engine.Policy.Profiles["python"] = &ProfilePolicy{Name: profile.Name, Allow: profile.Allow, BudgetBytes: &budget}

	token := authzToken(authzSession, "python", authzNow+300)
	firstStatus, firstHeaders, _ := authzPostPayload(t, server, authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, token), authzWithByteHint(600)))
	secondStatus, secondHeaders, _ := authzPostPayload(t, server, authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, token), authzWithByteHint(600)))

	if firstStatus != http.StatusOK || firstHeaders.Get("x-egress-decision") != "allow" {
		t.Fatalf("first = (%d, %q), want (200, allow)", firstStatus, firstHeaders.Get("x-egress-decision"))
	}
	if secondStatus != http.StatusForbidden {
		t.Fatalf("second status = %d, want 403", secondStatus)
	}
	if secondHeaders.Get("x-egress-decision") != "deny" || secondHeaders.Get("x-egress-reason") != "budget-exhausted" {
		t.Fatalf("second = (%q, %q), want (deny, budget-exhausted)", secondHeaders.Get("x-egress-decision"), secondHeaders.Get("x-egress-reason"))
	}
}

// --- /check: request shapes -------------------------------------------------

func TestAuthorizerSourceAddressIsRecordedForTheReaper(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("example.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	payload["attributes"].(map[string]any)["source"] = map[string]any{
		"address": map[string]any{"socketAddress": map[string]any{"address": "172.20.7.7"}},
	}
	authzPostPayload(t, server, payload)

	if recorder.events[0].SourceIP == nil || *recorder.events[0].SourceIP != "172.20.7.7" {
		t.Fatalf("event source = %v, want 172.20.7.7", recorder.events[0].SourceIP)
	}
}

func TestAuthorizerAbsoluteFormPathResolvesTheTarget(t *testing.T) {
	// Non-CONNECT proxying puts the authority in :path, not :host.
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := authzEnvoyPayload("ignored", 0, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300)))
	httpBlock := payload["attributes"].(map[string]any)["request"].(map[string]any)["http"].(map[string]any)
	httpBlock["method"] = "GET"
	httpBlock["host"] = ""
	httpBlock["path"] = "http://pypi.org:443/simple/"

	status, headers, _ := authzPostPayload(t, server, payload)

	if status != http.StatusOK || headers.Get("x-egress-decision") != "allow" {
		t.Fatalf("(status, decision) = (%d, %q), want (200, allow)", status, headers.Get("x-egress-decision"))
	}
}

func TestAuthorizerRequestWithoutATargetIsABadRequest(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	payload := map[string]any{"attributes": map[string]any{"request": map[string]any{"http": map[string]any{"method": "CONNECT", "host": ""}}}}
	status, _, body := authzPostPayload(t, server, payload)

	if status != http.StatusBadRequest || body["error"] != "bad-request" {
		t.Fatalf("(status, error) = (%d, %v), want (400, bad-request)", status, body["error"])
	}
}

func TestAuthorizerUnparseableBodyIsABadRequest(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	status, _, body := authzPost(t, server, "/check", "{not json")

	if status != http.StatusBadRequest || body["error"] != "bad-request" {
		t.Fatalf("(status, error) = (%d, %v), want (400, bad-request)", status, body["error"])
	}
}

func TestAuthorizerUnknownPostPathIsNotFound(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	status, _, body := authzPost(t, server, "/nope", "{}")

	if status != http.StatusNotFound || body["error"] != "not-found" {
		t.Fatalf("(status, error) = (%d, %v), want (404, not-found)", status, body["error"])
	}
}

func TestAuthorizerUnknownGetPathIsNotFound(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	status, _, body := authzGet(t, server, "/metrics")

	if status != http.StatusNotFound || body["error"] != "not-found" {
		t.Fatalf("(status, error) = (%d, %v), want (404, not-found)", status, body["error"])
	}
}

// --- /healthz ---------------------------------------------------------------

func TestAuthorizerHealthzReportsRoleAndPolicyVersion(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	status, _, body := authzGet(t, server, "/healthz")

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if body["status"] != "ok" {
		t.Fatalf("status = %v, want ok", body["status"])
	}
	if body["role"] != "authorizer" {
		t.Fatalf("role = %v, want authorizer", body["role"])
	}
	if body["policy_version"] != authzPolicyVersion {
		t.Fatalf("policy version = %v, want %s", body["policy_version"], authzPolicyVersion)
	}
	profiles, ok := body["profiles"].([]any)
	if !ok || len(profiles) != 2 || profiles[0] != "offline" || profiles[1] != "python" {
		t.Fatalf("profiles = %v, want [offline python]", body["profiles"])
	}
}

func TestAuthorizerHealthzIsExemptFromAuthentication(t *testing.T) {
	// Probes must not need a token: /healthz is the only unauthenticated path.
	authorizer, _, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)

	if status, _, _ := authzGet(t, server, "/healthz"); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// --- build_server -----------------------------------------------------------

func TestBuildAuthorizerServerBindsPortAndHandler(t *testing.T) {
	authorizer, _, _ := newAuthzFixture(t)

	server := BuildAuthorizerServer(authorizer, 9091, "127.0.0.1")

	if server.Addr != "127.0.0.1:9091" {
		t.Fatalf("addr = %q, want 127.0.0.1:9091", server.Addr)
	}
	if server.Handler == nil {
		t.Fatalf("handler is nil")
	}
	if server.ReadHeaderTimeout <= 0 {
		t.Fatalf("read header timeout = %v, want positive", server.ReadHeaderTimeout)
	}
	if defaulted := BuildAuthorizerServer(authorizer, 8080, ""); defaulted.Addr != "0.0.0.0:8080" {
		t.Fatalf("default bind = %q, want 0.0.0.0:8080", defaulted.Addr)
	}
}

// --- request parsing --------------------------------------------------------

func TestParseCheckRequestReadsTheEnvoyShape(t *testing.T) {
	payload := authzEnvoyPayload("pypi.org", 8443, authzWithToken("session-1", "token-1"), authzWithByteHint(4096))

	request, err := ParseCheckRequest(payload)
	if err != nil {
		t.Fatalf("ParseCheckRequest: %v", err)
	}

	if request.Method != "CONNECT" || request.Host != "pypi.org" || request.Port != 8443 {
		t.Fatalf("request = %+v, want CONNECT pypi.org:8443", request)
	}
	if request.Headers["proxy-authorization"] != authzBasic("session-1", "token-1") {
		t.Fatalf("proxy authorization = %q", request.Headers["proxy-authorization"])
	}
	if request.SourceIP == nil || *request.SourceIP != "172.20.5.9" {
		t.Fatalf("source ip = %v, want 172.20.5.9", request.SourceIP)
	}
	if request.ByteHint != 4096 {
		t.Fatalf("byte hint = %d, want 4096", request.ByteHint)
	}
}

func TestParseCheckRequestReadsTheFlatShape(t *testing.T) {
	payload := map[string]any{
		"method":              "CONNECT",
		"target":              "example.org:443",
		"proxy_authorization": "Basic abc",
		"source_ip":           "10.42.1.9",
		"bytes":               "2048",
		"headers":             map[string]any{"X-Custom": "VALUE"},
	}

	request, err := ParseCheckRequest(payload)
	if err != nil {
		t.Fatalf("ParseCheckRequest: %v", err)
	}

	if request.Host != "example.org" || request.Port != 443 {
		t.Fatalf("target = %s:%d, want example.org:443", request.Host, request.Port)
	}
	if request.Headers["proxy-authorization"] != "Basic abc" {
		t.Fatalf("proxy authorization = %q", request.Headers["proxy-authorization"])
	}
	if request.Headers["x-custom"] != "VALUE" {
		t.Fatalf("header keys must be lower-cased, got %v", request.Headers)
	}
	if request.ByteHint != 2048 {
		t.Fatalf("byte hint = %d, want 2048", request.ByteHint)
	}
}

func TestParseCheckRequestFlatShapeDefaultsMethodAndKeepsExplicitHeader(t *testing.T) {
	payload := map[string]any{
		"target":      "example.org:443",
		"headers":     map[string]any{"proxy-authorization": "Basic explicit"},
		"source_ip":   "",
		"bytes":       0,
		"method":      "",
	}

	request, err := ParseCheckRequest(payload)
	if err != nil {
		t.Fatalf("ParseCheckRequest: %v", err)
	}

	if request.Method != "CONNECT" {
		t.Fatalf("method = %q, want CONNECT", request.Method)
	}
	// An explicit header wins over the flat proxy_authorization convenience key.
	if request.Headers["proxy-authorization"] != "Basic explicit" {
		t.Fatalf("proxy authorization = %q, want the explicit header", request.Headers["proxy-authorization"])
	}
	if request.SourceIP != nil {
		t.Fatalf("source ip = %v, want nil", *request.SourceIP)
	}
	if request.ByteHint != 0 {
		t.Fatalf("byte hint = %d, want 0", request.ByteHint)
	}
}

func TestParseCheckRequestRejectsNonObjectsAndMissingTargets(t *testing.T) {
	if _, err := ParseCheckRequest([]any{}); err == nil {
		t.Fatalf("a JSON array was accepted as a check payload")
	}
	if _, err := ParseCheckRequest(map[string]any{}); err == nil {
		t.Fatalf("a payload with no target was accepted")
	}
	if _, err := ParseCheckRequest(map[string]any{"attributes": map[string]any{"request": map[string]any{"http": "nope"}}}); err == nil {
		t.Fatalf("a non-object attributes.request.http was accepted")
	}
	if _, err := ParseCheckRequest(map[string]any{"attributes": map[string]any{"request": "nope"}}); err == nil {
		t.Fatalf("a non-object attributes.request was accepted")
	}
}

func TestLowerHeadersKeepsTheFirstListElement(t *testing.T) {
	headers := LowerHeaders(map[string]any{
		"X-List":    []any{"first", "second"},
		"X-Empty":   []any{},
		"X-Number":  4096,
		"X-Boolean": true,
	})

	if headers["x-list"] != "first" {
		t.Fatalf("list header = %q, want first", headers["x-list"])
	}
	if headers["x-empty"] != "" {
		t.Fatalf("empty list header = %q, want empty", headers["x-empty"])
	}
	if headers["x-number"] != "4096" {
		t.Fatalf("number header = %q, want 4096", headers["x-number"])
	}
	if headers["x-boolean"] != "True" {
		t.Fatalf("boolean header = %q, want True", headers["x-boolean"])
	}
	if len(LowerHeaders("not an object")) != 0 {
		t.Fatalf("a non-object headers block must yield no headers")
	}
}

func TestSplitHostPortHandlesBracketsAndDefaults(t *testing.T) {
	cases := []struct {
		value       string
		defaultPort int
		wantHost    string
		wantPort    int
	}{
		{"pypi.org:8443", 443, "pypi.org", 8443},
		{"pypi.org", 443, "pypi.org", 443},
		{"  pypi.org:443  ", 80, "pypi.org", 443},
		{"[::1]:8080", 443, "::1", 8080},
		{"[::1]", 443, "::1", 443},
		{"[::1]:", 443, "::1", 443},
		{"pypi.org:notaport", 80, "pypi.org", 80},
		{"", 443, "", 443},
		{"2001:db8::1", 443, "2001:db8::1", 443},
	}
	for _, tc := range cases {
		host, port := SplitHostPort(tc.value, tc.defaultPort)
		if host != tc.wantHost || port != tc.wantPort {
			t.Fatalf("SplitHostPort(%q, %d) = (%q, %d), want (%q, %d)", tc.value, tc.defaultPort, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestByteHintNormalizesValues(t *testing.T) {
	cases := []struct {
		raw  any
		want int
	}{
		{nil, 0},
		{"4096", 4096},
		{" 4096 ", 4096},
		{"not-a-number", 0},
		{"-5", 0},
		{"4096.0", 0},
		{float64(4096), 4096},
		{float64(-1), 0},
		{float64(1.5), 0},
		{0, 0},
		{json.Number("2048"), 2048},
		{[]any{1}, 0},
	}
	for _, tc := range cases {
		if got := ByteHint(tc.raw); got != tc.want {
			t.Fatalf("ByteHint(%#v) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// --- Proxy-Authorization ----------------------------------------------------

func TestBasicCredentialsParsesAndRejects(t *testing.T) {
	valid := map[string]string{"proxy-authorization": authzBasic("session-1", "token-1")}
	credentials, failure := BasicCredentials(valid)
	if credentials == nil || credentials[0] != "session-1" || credentials[1] != "token-1" {
		t.Fatalf("credentials = %v, want (session-1, token-1)", credentials)
	}
	if failure != "" {
		t.Fatalf("failure = %q, want empty", failure)
	}

	// The authorization header is the fallback when no proxy header is present.
	fallback := map[string]string{"authorization": authzBasic("session-2", "token-2")}
	if credentials, _ := BasicCredentials(fallback); credentials == nil || credentials[0] != "session-2" {
		t.Fatalf("authorization fallback = %v, want session-2", credentials)
	}

	cases := []struct {
		headers map[string]string
		reason  string
	}{
		{map[string]string{}, TokenMissing},
		{map[string]string{"proxy-authorization": "Bearer abc"}, TokenInvalid},
		{map[string]string{"proxy-authorization": "Basic "}, TokenInvalid},
		{map[string]string{"proxy-authorization": "Basic %%%"}, TokenInvalid},
		{map[string]string{"proxy-authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("noseparator"))}, TokenInvalid},
		{map[string]string{"proxy-authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(":password"))}, TokenInvalid},
		{map[string]string{"proxy-authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("session:"))}, TokenInvalid},
	}
	for _, tc := range cases {
		credentials, failure := BasicCredentials(tc.headers)
		if credentials != nil || failure != tc.reason {
			t.Fatalf("BasicCredentials(%v) = (%v, %q), want (nil, %q)", tc.headers, credentials, failure, tc.reason)
		}
	}
}

// --- events -----------------------------------------------------------------

func TestEventCompactJSONMatchesPythonSortKeysSeparators(t *testing.T) {
	session, profile, source := "session", "python", "10.0.0.1"
	event := &Event{
		EventID:     "deadbeefdeadbeef",
		TS:          1_700_000_000,
		Kind:        "deny",
		SessionHash: &session,
		Profile:     &profile,
		TargetHost:  "example.org",
		TargetPort:  443,
		Reason:      "not-allowlisted",
		Strikes:     1,
		SourceIP:    &source,
	}

	want := `{"event_id":"deadbeefdeadbeef","kind":"deny","profile":"python",` +
		`"reason":"not-allowlisted","session_hash":"session","source_ip":"10.0.0.1",` +
		`"strikes":1,"target_host":"example.org","target_port":443,"ts":1700000000}`
	if got := string(event.CompactJSON()); got != want {
		t.Fatalf("event JSON = %s, want %s", got, want)
	}

	unauthenticated := &Event{EventID: "deadbeefdeadbeef", TS: 1, Kind: "deny", TargetHost: "h", TargetPort: 1, Reason: "token-missing"}
	if got := string(unauthenticated.CompactJSON()); !strings.Contains(got, `"session_hash":null`) || !strings.Contains(got, `"profile":null`) || !strings.Contains(got, `"source_ip":null`) {
		t.Fatalf("unauthenticated event must carry null identity fields, got %s", got)
	}
}

func TestAuthorizerEmitsNoEventOnAllowAndOneOnDeny(t *testing.T) {
	authorizer, recorder, _ := newAuthzFixture(t)
	server := authzServer(t, authorizer)
	token := authzToken(authzSession, "python", authzNow+300)

	authzPostPayload(t, server, authzEnvoyPayload("pypi.org", 443, authzWithToken(authzSession, token)))
	if len(recorder.events) != 0 {
		t.Fatalf("allow emitted %d events, want 0", len(recorder.events))
	}

	authzPostPayload(t, server, authzEnvoyPayload("example.org", 443, authzWithToken(authzSession, token)))
	authzPostPayload(t, server, authzEnvoyPayload("169.254.169.254", 443, authzWithToken(authzSession, token)))

	if len(recorder.events) != 2 {
		t.Fatalf("events = %d, want 2", len(recorder.events))
	}
	if recorder.events[0].Kind != "deny" || recorder.events[1].Kind != "kill" {
		t.Fatalf("kinds = (%q, %q), want (deny, kill)", recorder.events[0].Kind, recorder.events[1].Kind)
	}
	if recorder.events[0].SessionHash == nil || *recorder.events[0].SessionHash != authzSession {
		t.Fatalf("event session = %v, want %s", recorder.events[0].SessionHash, authzSession)
	}
	if recorder.events[0].TargetPort != 443 || recorder.events[0].Strikes != 1 {
		t.Fatalf("event = %+v, want port 443 strike 1", recorder.events[0])
	}
	if recorder.events[1].Reason != "kill-destination" {
		t.Fatalf("kill reason = %q, want kill-destination", recorder.events[1].Reason)
	}
}

func TestEventIDMatchesTheReaperGrammar(t *testing.T) {
	id := newEventID()

	if !EventIDIsValid(id) {
		t.Fatalf("event id %q does not match [A-Za-z0-9_-]{8,64}", id)
	}
	if len(id) != 32 {
		t.Fatalf("event id length = %d, want 32 (uuid4 hex)", len(id))
	}
}

// --- EventEmitter -----------------------------------------------------------

// authzTransport records the last POST and answers with a script.
type authzTransport struct {
	calls   []authzTransportCall
	answers []authzTransportAnswer
}

type authzTransportCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

type authzTransportAnswer struct {
	status int
	body   []byte
	err    error
}

func (t *authzTransport) do(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	t.calls = append(t.calls, authzTransportCall{method: method, url: url, headers: headers, body: body})
	index := len(t.calls) - 1
	if index < len(t.answers) {
		answer := t.answers[index]
		return answer.status, answer.body, answer.err
	}
	return 200, nil, nil
}

func newAuthzEmitter(transport *authzTransport) *EventEmitter {
	emitter := NewEventEmitter("http://reaper.test/events", authzSecret)
	emitter.Transport = transport.do
	emitter.Sleep = func(float64) {}
	return emitter
}

func TestEventEmitterSignsTheRawBodyAndPostsIt(t *testing.T) {
	transport := &authzTransport{answers: []authzTransportAnswer{{status: 200}}}
	emitter := newAuthzEmitter(transport)
	source := "10.0.0.1"
	event := &Event{EventID: "deadbeefdeadbeef", TS: authzNow, Kind: "deny", TargetHost: "example.org", TargetPort: 443, Reason: "not-allowlisted", Strikes: 1, SourceIP: &source}

	if !emitter.Emit(event) {
		t.Fatalf("Emit reported failure on a 200")
	}
	if len(transport.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(transport.calls))
	}
	call := transport.calls[0]
	if call.method != "POST" || call.url != "http://reaper.test/events" {
		t.Fatalf("call = %s %s, want POST http://reaper.test/events", call.method, call.url)
	}
	if call.headers["Content-Type"] != "application/json" {
		t.Fatalf("content type = %q", call.headers["Content-Type"])
	}
	if !VerifyEvent(authzSecret, call.body, call.headers["x-egress-signature"]) {
		t.Fatalf("the reaper could not verify the emitted signature")
	}
	if !strings.Contains(string(call.body), `"reason":"not-allowlisted"`) {
		t.Fatalf("body = %s", call.body)
	}
}

func TestEventEmitterRetriesTransportFailuresAnd5xx(t *testing.T) {
	transport := &authzTransport{answers: []authzTransportAnswer{
		{err: fmt.Errorf("connection refused")},
		{status: 500},
		{status: 200},
	}}
	emitter := newAuthzEmitter(transport)
	emitter.Attempts = 3

	if !emitter.Emit(&Event{EventID: "deadbeefdeadbeef"}) {
		t.Fatalf("Emit reported failure after a recoverable sequence")
	}
	if len(transport.calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(transport.calls))
	}
}

func TestEventEmitterGivesUpAfterTheAttemptBudget(t *testing.T) {
	transport := &authzTransport{answers: []authzTransportAnswer{
		{err: fmt.Errorf("connection refused")},
		{err: fmt.Errorf("connection refused")},
	}}
	emitter := newAuthzEmitter(transport)

	if emitter.Emit(&Event{EventID: "deadbeefdeadbeef"}) {
		t.Fatalf("Emit reported success with every attempt failing")
	}
	if len(transport.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (attempts)", len(transport.calls))
	}
}

func TestEventEmitterDoesNotRetryAClientRejection(t *testing.T) {
	transport := &authzTransport{answers: []authzTransportAnswer{{status: 403}, {status: 200}}}
	emitter := newAuthzEmitter(transport)

	if emitter.Emit(&Event{EventID: "deadbeefdeadbeef"}) {
		t.Fatalf("Emit reported success on a 403")
	}
	if len(transport.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (a 4xx is final)", len(transport.calls))
	}
}

// authzFailingSink is an emitter that always fails; a decision must be
// unaffected by it.
type authzFailingSink struct{ calls int }

func (s *authzFailingSink) Emit(*Event) bool {
	s.calls++
	return false
}

func TestAuthorizerDecisionSurvivesAFailingEmitter(t *testing.T) {
	policy := loadTestPolicy(t, authzPolicyBody())
	engine, err := NewEgressEngine(policy, EgressEngineConfig{StrikeThreshold: 3, StrikeWindowS: 60.0})
	if err != nil {
		t.Fatalf("NewEgressEngine: %v", err)
	}
	sink := &authzFailingSink{}
	authorizer := NewAuthorizer(engine, sink, AuthorizerConfig{Secret: authzSecret, Clock: func() float64 { return authzNow }})
	server := authzServer(t, authorizer)

	status, headers, _ := authzPostPayload(t, server, authzEnvoyPayload("example.org", 443, authzWithToken(authzSession, authzToken(authzSession, "python", authzNow+300))))

	if status != http.StatusForbidden || headers.Get("x-egress-decision") != "deny" {
		t.Fatalf("(status, decision) = (%d, %q), want (403, deny)", status, headers.Get("x-egress-decision"))
	}
	if sink.calls != 1 {
		t.Fatalf("sink calls = %d, want 1", sink.calls)
	}
}