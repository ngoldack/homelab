package guard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeK8sTransport records every round trip and answers with a programmed
// response, so the tests assert the wire request (method, URL, headers, body)
// and the client's status handling independently.
type fakeK8sTransport struct {
	calls []fakeK8sCall
	// responses are consumed in order; the last one repeats for extra calls.
	responses []fakeK8sResponse
	// err, when set, makes the transport fail at the connection level.
	err error
}

type fakeK8sCall struct {
	method  string
	url     string
	headers map[string]string
	body    []byte
}

type fakeK8sResponse struct {
	status int
	body   []byte
}

func (f *fakeK8sTransport) do(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	f.calls = append(f.calls, fakeK8sCall{method: method, url: url, headers: headers, body: body})
	if f.err != nil {
		return 0, nil, f.err
	}
	if len(f.responses) == 0 {
		return 0, nil, errors.New("fakeK8sTransport: no programmed response")
	}
	response := f.responses[0]
	if len(f.responses) > 1 {
		f.responses = f.responses[1:]
	}
	return response.status, response.body, nil
}

func (f *fakeK8sTransport) last() fakeK8sCall {
	if len(f.calls) == 0 {
		panic("fakeK8sTransport: no calls recorded")
	}
	return f.calls[len(f.calls)-1]
}

// newTestK8sClient builds a client with a fixed bearer token and the fake
// transport, so no test touches the real ServiceAccount files.
func newTestK8sClient(t *testing.T, responses ...fakeK8sResponse) (*K8sClient, *fakeK8sTransport) {
	t.Helper()
	transport := &fakeK8sTransport{responses: responses}
	token := "test-token"
	client := NewK8sClient(K8sOptions{
		BaseURL:   "https://api.test",
		Token:     &token,
		TimeoutS:  1,
		Transport: transport.do,
	})
	return client, transport
}

func TestAPIBuildsGroupVersionPrefix(t *testing.T) {
	cases := map[string]string{
		"extensions.agents.x-k8s.io/v1beta1": "/apis/extensions.agents.x-k8s.io/v1beta1",
		"agents.x-k8s.io/v1beta1":            "/apis/agents.x-k8s.io/v1beta1",
		"v1":                                 "/apis/v1/",
	}
	for groupVersion, want := range cases {
		if got := k8sAPIPathBase(groupVersion); got != want {
			t.Fatalf("k8sAPIPathBase(%q) = %q, want %q", groupVersion, got, want)
		}
	}
}

func TestQuotePathMatchesPythonQuote(t *testing.T) {
	cases := map[string]string{
		"hermes-sandbox":                  "hermes-sandbox",
		"team a/b":                        "team%20a%2Fb",
		"job-1_suffix.2026":               "job-1_suffix.2026",
		"ümlaut+":                         "%C3%BCmlaut%2B",
		"workload.hermes.io/session-hash": "workload.hermes.io%2Fsession-hash",
	}
	for input, want := range cases {
		if got := k8sQuotePath(input); got != want {
			t.Fatalf("k8sQuotePath(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestListClaimsSendsSelectorAndFiltersNonObjects(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{
		status: 200,
		body:   []byte(`{"items":[{"metadata":{"name":"claim-0"}},"nope",7]}`),
	})

	claims, err := client.ListClaims("hermes-sandbox", "workload.hermes.io/session-hash=abc-123")
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}

	call := transport.last()
	if call.method != "GET" {
		t.Fatalf("method = %q, want GET", call.method)
	}
	wantURL := "https://api.test/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/hermes-sandbox/sandboxclaims" +
		"?labelSelector=workload.hermes.io%2Fsession-hash%3Dabc-123"
	if call.url != wantURL {
		t.Fatalf("url = %q, want %q", call.url, wantURL)
	}
	if call.headers["Authorization"] != "Bearer test-token" {
		t.Fatalf("Authorization = %q", call.headers["Authorization"])
	}
	if call.headers["Accept"] != "application/json" {
		t.Fatalf("Accept = %q", call.headers["Accept"])
	}
	if _, present := call.headers["Content-Type"]; present {
		t.Fatalf("Content-Type sent on a bodyless GET")
	}
	if call.body != nil {
		t.Fatalf("body = %q, want nil", call.body)
	}
	if len(claims) != 1 || claims[0]["metadata"] == nil {
		t.Fatalf("claims = %#v, want only the object entry", claims)
	}
}

func TestListClaimsWithoutSelectorOmitsQuery(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{"items":[]}`)})

	claims, err := client.ListClaims("hermes-sandbox", "")
	if err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %#v, want empty", claims)
	}
	wantURL := "https://api.test/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/hermes-sandbox/sandboxclaims"
	if got := transport.last().url; got != wantURL {
		t.Fatalf("url = %q, want %q", got, wantURL)
	}
}

func TestListClaimsEncodesNamespaceAndDefaultGroup(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{}`)})

	if _, err := client.ListClaims("team a", "x=y"); err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	wantURL := "https://api.test/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/team%20a/sandboxclaims?labelSelector=x%3Dy"
	if got := transport.last().url; got != wantURL {
		t.Fatalf("url = %q, want %q", got, wantURL)
	}
}

func TestGetSandboxUsesSandboxGroup(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{"metadata":{"name":"sb"}}`)})

	payload, err := client.GetSandbox("hermes-sandbox", "sb")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	if payload["metadata"] == nil {
		t.Fatalf("payload = %#v", payload)
	}
	wantURL := "https://api.test/apis/agents.x-k8s.io/v1beta1/namespaces/hermes-sandbox/sandboxes/sb"
	if got := transport.last().url; got != wantURL {
		t.Fatalf("url = %q, want %q", got, wantURL)
	}
}

func TestDeleteClaimSendsUIDPrecondition(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{"status":"Success"}`)})

	deleted, err := client.DeleteClaim("hermes-sandbox", "claim-0", "uid-7")
	if err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	if !deleted {
		t.Fatalf("deleted = false, want true")
	}

	call := transport.last()
	if call.method != "DELETE" {
		t.Fatalf("method = %q, want DELETE", call.method)
	}
	wantURL := "https://api.test/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/hermes-sandbox/sandboxclaims/claim-0"
	if call.url != wantURL {
		t.Fatalf("url = %q, want %q", call.url, wantURL)
	}
	if call.headers["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", call.headers["Content-Type"])
	}
	wantBody := `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"uid-7"}}`
	if string(call.body) != wantBody {
		t.Fatalf("body = %s, want %s", call.body, wantBody)
	}
}

func TestDeleteClaimMissingReturnsFalseWithoutError(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 404, body: []byte(`{"kind":"Status"}`)})

	deleted, err := client.DeleteClaim("hermes-sandbox", "gone", "uid-1")
	if err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	if deleted {
		t.Fatalf("deleted = true for a 404")
	}
}

func TestDeleteClaimErrorStatusRaises(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 409, body: []byte(`{"reason":"Conflict"}`)})

	deleted, err := client.DeleteClaim("hermes-sandbox", "claim-0", "uid-1")
	if deleted {
		t.Fatalf("deleted = true on a 409")
	}
	var apiErr *K8sApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *K8sApiError", err)
	}
	if apiErr.Status != 409 || apiErr.Method != "DELETE" || apiErr.Path != "claim-0" {
		t.Fatalf("apiErr = %#v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), `DELETE claim-0: HTTP 409: {"reason":"Conflict"}`) {
		t.Fatalf("message = %q", apiErr.Error())
	}
}

func TestDeleteClaimEncodesClaimName(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{}`)})

	if _, err := client.DeleteClaim("hermes-sandbox", "claim/x y", "uid-1"); err != nil {
		t.Fatalf("DeleteClaim: %v", err)
	}
	wantURL := "https://api.test/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/hermes-sandbox/sandboxclaims/claim%2Fx%20y"
	if got := transport.last().url; got != wantURL {
		t.Fatalf("url = %q, want %q", got, wantURL)
	}
}

func TestGetConfigMapMissingReturnsNil(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 404, body: []byte(`{"reason":"NotFound"}`)})

	cm, err := client.GetConfigMap("hermes-sandbox", "hermes-quarantine")
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if cm != nil {
		t.Fatalf("cm = %#v, want nil", cm)
	}
	wantURL := "https://api.test/api/v1/namespaces/hermes-sandbox/configmaps/hermes-quarantine"
	if got := transport.last().url; got != wantURL {
		t.Fatalf("url = %q, want %q", got, wantURL)
	}
}

func TestGetConfigMapParsesData(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{
		status: 200,
		body:   []byte(`{"metadata":{"resourceVersion":"42"},"data":{"hash":{"ttl_s":1}}}`),
	})

	cm, err := client.GetConfigMap("hermes-sandbox", "hermes-quarantine")
	if err != nil {
		t.Fatalf("GetConfigMap: %v", err)
	}
	if cm["metadata"].(map[string]any)["resourceVersion"] != "42" {
		t.Fatalf("cm = %#v", cm)
	}
}

func TestGetConfigMapInvalidJSONRaises(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{nope`)})

	cm, err := client.GetConfigMap("hermes-sandbox", "hermes-quarantine")
	if cm != nil {
		t.Fatalf("cm = %#v, want nil", cm)
	}
	var apiErr *K8sApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *K8sApiError", err)
	}
	if apiErr.Status != 200 || !strings.HasPrefix(apiErr.Body, "invalid ConfigMap JSON: ") {
		t.Fatalf("apiErr = %#v", apiErr)
	}
}

func TestCreateConfigMapBody(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 201, body: []byte(`{"metadata":{"name":"hermes-quarantine"}}`)})

	created, err := client.CreateConfigMap("hermes-sandbox", "hermes-quarantine", map[string]string{})
	if err != nil {
		t.Fatalf("CreateConfigMap: %v", err)
	}
	if created["metadata"] == nil {
		t.Fatalf("created = %#v", created)
	}

	call := transport.last()
	if call.method != "POST" {
		t.Fatalf("method = %q, want POST", call.method)
	}
	wantURL := "https://api.test/api/v1/namespaces/hermes-sandbox/configmaps"
	if call.url != wantURL {
		t.Fatalf("url = %q, want %q", call.url, wantURL)
	}
	wantBody := `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"hermes-quarantine","namespace":"hermes-sandbox"},"data":{}}`
	if string(call.body) != wantBody {
		t.Fatalf("body = %s, want %s", call.body, wantBody)
	}
}

func TestReplaceConfigMapSendsResourceVersion(t *testing.T) {
	client, transport := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`{}`)})

	if _, err := client.ReplaceConfigMap("hermes-sandbox", "hermes-quarantine", map[string]string{"k": "v"}, "42"); err != nil {
		t.Fatalf("ReplaceConfigMap: %v", err)
	}

	call := transport.last()
	if call.method != "PUT" {
		t.Fatalf("method = %q, want PUT", call.method)
	}
	wantURL := "https://api.test/api/v1/namespaces/hermes-sandbox/configmaps/hermes-quarantine"
	if call.url != wantURL {
		t.Fatalf("url = %q, want %q", call.url, wantURL)
	}
	wantBody := `{"apiVersion":"v1","kind":"ConfigMap",` +
		`"metadata":{"name":"hermes-quarantine","namespace":"hermes-sandbox","resourceVersion":"42"},` +
		`"data":{"k":"v"}}`
	if string(call.body) != wantBody {
		t.Fatalf("body = %s, want %s", call.body, wantBody)
	}
}

func TestJSONObjectEmptyBodyIsEmptyObject(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 200, body: nil})

	payload, err := client.jsonObject("GET", "/api/v1/nodes", nil)
	if err != nil {
		t.Fatalf("jsonObject: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("payload = %#v, want empty", payload)
	}
}

func TestJSONObjectNonObjectPayloadRaises(t *testing.T) {
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 200, body: []byte(`[1,2]`)})

	_, err := client.jsonObject("GET", "/api/v1/nodes", nil)
	var apiErr *K8sApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *K8sApiError", err)
	}
	if apiErr.Body != "expected a JSON object from API server" {
		t.Fatalf("body = %q", apiErr.Body)
	}
}

func TestJSONObjectErrorStatusCarriesTruncatedBody(t *testing.T) {
	// The error record keeps the whole body (the reaper logs it), while the
	// message cuts it to 400 runes like Python's s[:400].
	long := strings.Repeat("x", 500)
	client, _ := newTestK8sClient(t, fakeK8sResponse{status: 500, body: []byte(long)})

	_, err := client.jsonObject("GET", "/api/v1/nodes", nil)
	var apiErr *K8sApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *K8sApiError", err)
	}
	if apiErr.Status != 500 || apiErr.Body != long {
		t.Fatalf("apiErr = %#v", apiErr)
	}
	want := "GET /api/v1/nodes: HTTP 500: " + strings.Repeat("x", 400)
	if apiErr.Error() != want {
		t.Fatalf("message = %q, want %q", apiErr.Error(), want)
	}
}

func TestK8sTruncateRunesCountsRunesNotBytes(t *testing.T) {
	// 401 two-byte runes: a byte-based cut would split the 200th rune.
	long := strings.Repeat("é", 401)
	got := k8sTruncateRunes(long, 400)
	if got != strings.Repeat("é", 400) {
		t.Fatalf("truncate = %q (%d runes)", got, len([]rune(got)))
	}
	if short := k8sTruncateRunes("héllo", 5); short != "héllo" {
		t.Fatalf("short input rewritten to %q", short)
	}
}

func TestRawSurfacesTransportFailure(t *testing.T) {
	client, _ := newTestK8sClient(t)
	client.Transport = func(string, string, map[string]string, []byte) (int, []byte, error) {
		return 0, nil, errors.New("connection refused")
	}

	_, err := client.ListClaims("hermes-sandbox", "")
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want transport failure", err)
	}
}

func TestBearerReadsTokenFilePerRequest(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	transport := &fakeK8sTransport{responses: []fakeK8sResponse{
		{status: 200, body: []byte(`{}`)},
		{status: 200, body: []byte(`{}`)},
	}}
	client := NewK8sClient(K8sOptions{
		BaseURL:   "https://api.test",
		TokenPath: tokenPath,
		TimeoutS:  1,
		Transport: transport.do,
	})

	if _, err := client.ListClaims("hermes-sandbox", ""); err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
	// A rotated projected token must be picked up by the next request.
	if err := os.WriteFile(tokenPath, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("rewrite token: %v", err)
	}
	if _, err := client.ListClaims("hermes-sandbox", ""); err != nil {
		t.Fatalf("ListClaims: %v", err)
	}

	if got := transport.calls[0].headers["Authorization"]; got != "Bearer first" {
		t.Fatalf("first Authorization = %q", got)
	}
	if got := transport.calls[1].headers["Authorization"]; got != "Bearer second" {
		t.Fatalf("second Authorization = %q", got)
	}
}

func TestBearerMissingTokenFileRaisesStatusZero(t *testing.T) {
	client := NewK8sClient(K8sOptions{
		BaseURL:   "https://api.test",
		TokenPath: filepath.Join(t.TempDir(), "absent"),
		TimeoutS:  1,
		Transport: func(string, string, map[string]string, []byte) (int, []byte, error) {
			t.Fatalf("transport called without a token")
			return 0, nil, nil
		},
	})

	_, err := client.ListClaims("hermes-sandbox", "")
	var apiErr *K8sApiError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *K8sApiError", err)
	}
	if apiErr.Status != 0 || !strings.HasPrefix(apiErr.Body, "cannot read service account token: ") {
		t.Fatalf("apiErr = %#v", apiErr)
	}
}

func TestNewK8sClientDefaults(t *testing.T) {
	client := NewK8sClient(K8sOptions{})

	if client.BaseURL != defaultK8sBaseURL {
		t.Fatalf("BaseURL = %q", client.BaseURL)
	}
	if client.TokenPath != defaultTokenPath || client.CAPath != defaultCAPath {
		t.Fatalf("credential paths = %q / %q", client.TokenPath, client.CAPath)
	}
	if client.ClaimBase != "/apis/extensions.agents.x-k8s.io/v1beta1" {
		t.Fatalf("ClaimBase = %q", client.ClaimBase)
	}
	if client.SandboxBase != "/apis/agents.x-k8s.io/v1beta1" {
		t.Fatalf("SandboxBase = %q", client.SandboxBase)
	}
	if client.TimeoutS != defaultK8sTimeoutS {
		t.Fatalf("TimeoutS = %v", client.TimeoutS)
	}
	if client.Transport == nil {
		t.Fatalf("Transport = nil, want the default net/http transport")
	}
	if client.Token != nil {
		t.Fatalf("Token = %v, want nil", client.Token)
	}
}

func TestNewK8sClientOverridesAndTrimsBaseURL(t *testing.T) {
	token := ""
	client := NewK8sClient(K8sOptions{
		BaseURL:    "https://api.test/",
		Token:      &token,
		ClaimAPI:   "custom.example.com/v1",
		SandboxAPI: "other.example.com/v2",
		TimeoutS:   2.5,
	})

	if client.BaseURL != "https://api.test" {
		t.Fatalf("BaseURL = %q", client.BaseURL)
	}
	if client.ClaimBase != "/apis/custom.example.com/v1" || client.SandboxBase != "/apis/other.example.com/v2" {
		t.Fatalf("bases = %q / %q", client.ClaimBase, client.SandboxBase)
	}
	if client.TimeoutS != 2.5 {
		t.Fatalf("TimeoutS = %v", client.TimeoutS)
	}
	// An explicitly empty token is still a token: the file must not be read.
	client.Transport = func(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		if headers["Authorization"] != "Bearer " {
			t.Fatalf("Authorization = %q, want the empty explicit token", headers["Authorization"])
		}
		return 200, []byte(`{}`), nil
	}
	if _, err := client.ListClaims("hermes-sandbox", ""); err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
}

func TestClientFromEnvOverrides(t *testing.T) {
	t.Setenv("EGRESS_K8S_API", "https://env.test/")
	t.Setenv("EGRESS_CLAIM_API", "env.example.com/v9")
	t.Setenv("EGRESS_SANDBOX_API", "envsb.example.com/v9")

	token := "t"
	client := ClientFromEnv()
	client.Token = &token
	client.Transport = func(method, url string, headers map[string]string, body []byte) (int, []byte, error) {
		if url != "https://env.test/apis/env.example.com/v9/namespaces/hermes-sandbox/sandboxclaims" {
			t.Fatalf("url = %q", url)
		}
		return 200, []byte(`{}`), nil
	}
	if _, err := client.ListClaims("hermes-sandbox", ""); err != nil {
		t.Fatalf("ListClaims: %v", err)
	}
}

func TestDeleteOptionsBodyShapeIsStableJSON(t *testing.T) {
	body, err := json.Marshal(k8sDeleteOptions{
		APIVersion:    "v1",
		Kind:          "DeleteOptions",
		Preconditions: k8sPreconditions{UID: "u"},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(body) != `{"apiVersion":"v1","kind":"DeleteOptions","preconditions":{"uid":"u"}}` {
		t.Fatalf("body = %s", body)
	}
}
