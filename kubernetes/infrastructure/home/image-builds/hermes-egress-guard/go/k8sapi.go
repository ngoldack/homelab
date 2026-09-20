// Minimal stdlib Kubernetes API client for the reaper (port of
// “src/egress_guard/k8sapi.py“).
//
// No client library is vendored, so this speaks the REST API directly and only
// the verbs the reaper needs. RBAC the reaper's ServiceAccount requires (Role
// in “hermes-sandbox“, bound to the “hermes-egress“ ServiceAccount):
//
//   - “extensions.agents.x-k8s.io/v1beta1“ “sandboxclaims“  get, list, delete
//   - “agents.x-k8s.io/v1beta1“            “sandboxes“      get
//   - core                                   “configmaps“     get, create, update
//     (restrict with “resourceNames: [hermes-quarantine]“)
//
// The Role only grants the sandboxclaims verbs and the restricted configmap
// verbs; “reaper.go“ resolves a session's claims by the
// “workload.hermes.io/session-hash“ label and never calls “GetSandbox“, so
// the sandboxes (and pods) verbs are deliberately NOT in the Role — a grant
// with no caller is standing privilege, not headroom.
//
// CRD group/version facts (from the vendored upstream manifests in
// “kubernetes/infrastructure/home/agent-sandbox/upstream/“): SandboxClaim
// lives in “extensions.agents.x-k8s.io/v1beta1“ and Sandbox in
// “agents.x-k8s.io/v1beta1“; both are namespaced.
//
// Wire fidelity notes, deliberately kept from the Python:
//   - Path segments are percent-encoded with the Python “quote(safe="")“
//     alphabet (every byte outside “A-Za-z0-9-_.~“), which is byte-identical
//     to Python for both group names and namespace/name segments.
//   - “DeleteClaim“/“GetConfigMap“ error records carry the bare
//     resource name as their path, exactly as the Python does.
//   - Request bodies are marshalled from ordered structs so the exact key
//     order of Python's “json.dumps(dict)“ is preserved on the wire.
package guard

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultK8sBaseURL      = "https://kubernetes.default.svc"
	defaultTokenPath       = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultCAPath          = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultClaimAPI        = "extensions.agents.x-k8s.io/v1beta1"
	defaultSandboxAPI      = "agents.x-k8s.io/v1beta1"
	defaultK8sTimeoutS     = 10.0
	k8sErrorBodyLimitRunes = 400
)

// K8sTransport performs one HTTP round trip. It mirrors the Python
// “Transport“ alias: the status is returned for both success and error
// responses (an HTTP 4xx/5xx is a value, not an error), while a transport-level
// failure — a connection error, a broken TLS handshake — is reported as an
// error. A nil body means "no request body".
type K8sTransport func(method, url string, headers map[string]string, body []byte) (int, []byte, error)

// K8sApiError is a failed Kubernetes API call (or an unreadable client
// credential). It mirrors the Python “K8sApiError“; Status 0 marks a local
// failure such as an unreadable ServiceAccount token.
type K8sApiError struct {
	Status int
	Body   string
	Method string
	Path   string
}

func (e *K8sApiError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, k8sTruncateRunes(e.Body, k8sErrorBodyLimitRunes))
}

// K8sOptions carries the client settings. Zero values select the in-cluster
// defaults, i.e. the same values the Python constructor defaults to.
type K8sOptions struct {
	// BaseURL is the API server root; empty selects the in-cluster service.
	BaseURL string
	// TokenPath is the projected ServiceAccount token file, read per request
	// because projected tokens rotate.
	TokenPath string
	// CAPath is the PEM bundle used to verify the API server. Empty selects
	// the in-cluster CA file.
	CAPath string
	// Token, when non-nil, is used verbatim and the token file is never read.
	Token *string
	// ClaimAPI / SandboxAPI are ``group/version`` pairs; empty selects the
	// vendored agent-sandbox CRD group versions.
	ClaimAPI   string
	SandboxAPI string
	// TimeoutS bounds one HTTP round trip; zero selects 10 seconds.
	TimeoutS float64
	// Transport overrides the default net/http transport (tests).
	Transport K8sTransport
}

// K8sClient is a REST client for the verbs the reaper needs.
type K8sClient struct {
	BaseURL     string
	TokenPath   string
	CAPath      string
	Token       *string
	ClaimBase   string
	SandboxBase string
	TimeoutS    float64
	Transport   K8sTransport
}

// NewK8sClient builds a client, filling every unset option with the Python
// constructor's default. The transport defaults to a net/http client that
// trusts only the API server CA.
func NewK8sClient(opts K8sOptions) *K8sClient {
	client := &K8sClient{
		BaseURL:   strings.TrimRight(k8sDefaultString(opts.BaseURL, defaultK8sBaseURL), "/"),
		TokenPath: k8sDefaultString(opts.TokenPath, defaultTokenPath),
		CAPath:    k8sDefaultString(opts.CAPath, defaultCAPath),
		Token:     opts.Token,
		ClaimBase: k8sAPIPathBase(k8sDefaultString(opts.ClaimAPI, defaultClaimAPI)),
		SandboxBase: k8sAPIPathBase(
			k8sDefaultString(opts.SandboxAPI, defaultSandboxAPI),
		),
		TimeoutS:  k8sDefaultTimeout(opts.TimeoutS),
		Transport: opts.Transport,
	}
	if client.Transport == nil {
		client.Transport = client.defaultTransport
	}
	return client
}

// ClientFromEnv mirrors the Python “client_from_env“: the API root and the
// two group-versions are overridable, everything else is in-cluster defaults.
func ClientFromEnv() *K8sClient {
	return NewK8sClient(K8sOptions{
		BaseURL:    EnvStr("EGRESS_K8S_API", defaultK8sBaseURL),
		ClaimAPI:   EnvStr("EGRESS_CLAIM_API", defaultClaimAPI),
		SandboxAPI: EnvStr("EGRESS_SANDBOX_API", defaultSandboxAPI),
	})
}

func k8sDefaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func k8sDefaultTimeout(seconds float64) float64 {
	if seconds <= 0 {
		return defaultK8sTimeoutS
	}
	return seconds
}

// k8sAPIPathBase turns a “group/version“ pair into the “/apis/<group>/<version>“
// prefix. A pair without a slash keeps the Python shape (group only, empty
// version, trailing slash).
func k8sAPIPathBase(groupVersion string) string {
	group, version, _ := strings.Cut(groupVersion, "/")
	return "/apis/" + k8sQuotePath(group) + "/" + k8sQuotePath(version)
}

// k8sQuotePath percent-encodes every byte outside the RFC 3986 unreserved set —
// the same alphabet Python's “urllib.parse.quote(value, safe="")“ produces.
func k8sQuotePath(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hexDigits[c>>4])
		out.WriteByte(hexDigits[c&0x0f])
	}
	return out.String()
}

// k8sTruncateRunes cuts s to at most limit runes, mirroring Python's “s[:400]“.
func k8sTruncateRunes(s string, limit int) string {
	// Rune count never exceeds byte count, so a short string is returned as-is
	// without materializing the rune slice.
	if len(s) <= limit {
		return s
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// defaultTransport is the real round trip: a net/http client trusting only
// CAFile. An HTTP status is returned as a value; only a transport failure is
// an error, mirroring the Python urllib error split.
func (c *K8sClient) defaultTransport(method, reqURL string, headers map[string]string, body []byte) (int, []byte, error) {
	var tlsConfig *tls.Config
	if c.CAPath != "" {
		pem, err := os.ReadFile(c.CAPath)
		if err != nil {
			return 0, nil, fmt.Errorf("cannot read API server CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return 0, nil, fmt.Errorf("no certificates in API server CA %s", c.CAPath)
		}
		tlsConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, reqURL, reader)
	if err != nil {
		return 0, nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	client := &http.Client{
		Timeout:   time.Duration(c.TimeoutS * float64(time.Second)),
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return 0, nil, err
	}
	return response.StatusCode, raw, nil
}

// bearer returns the credential for this request. The token file is read per
// request because projected ServiceAccount tokens rotate.
func (c *K8sClient) bearer() (string, error) {
	if c.Token != nil {
		return *c.Token, nil
	}
	token, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return "", &K8sApiError{Status: 0, Body: fmt.Sprintf("cannot read service account token: %v", err)}
	}
	return strings.TrimSpace(string(token)), nil
}

// raw issues one request and returns the status with the response bytes. The
// Authorization and Accept headers are always set; Content-Type only when a
// body is sent.
func (c *K8sClient) raw(method, path string, body []byte) (int, []byte, error) {
	token, err := c.bearer()
	if err != nil {
		return 0, nil, err
	}
	headers := map[string]string{
		"Authorization": "Bearer " + token,
		"Accept":        "application/json",
	}
	if body != nil {
		headers["Content-Type"] = "application/json"
	}
	return c.Transport(method, c.BaseURL+path, headers, body)
}

// jsonObject issues one request and decodes a JSON object response. A status
// >= 400 is an error carrying the response body; an empty body is the empty
// object; a non-object payload is an error.
func (c *K8sClient) jsonObject(method, path string, body []byte) (map[string]any, error) {
	status, raw, err := c.raw(method, path, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, &K8sApiError{Status: status, Body: k8sDecodeBody(raw), Method: method, Path: path}
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &K8sApiError{
			Status: status,
			Body:   "invalid JSON from API server: " + err.Error(),
			Method: method,
			Path:   path,
		}
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return nil, &K8sApiError{
			Status: status,
			Body:   "expected a JSON object from API server",
			Method: method,
			Path:   path,
		}
	}
	return object, nil
}

// k8sDecodeBody renders an error body the way Python's
// “raw.decode("utf-8", "replace")“ does: invalid byte sequences become the
// replacement character instead of vanishing.
func k8sDecodeBody(raw []byte) string {
	return strings.ToValidUTF8(string(raw), "\uFFFD")
}

// ListClaims returns the SandboxClaims in namespace, optionally filtered by a
// label selector. Non-object list entries are dropped, matching the Python
// filter.
func (c *K8sClient) ListClaims(namespace, labelSelector string) ([]map[string]any, error) {
	ns := k8sQuotePath(namespace)
	path := c.ClaimBase + "/namespaces/" + ns + "/sandboxclaims"
	if labelSelector != "" {
		path += "?" + url.Values{"labelSelector": {labelSelector}}.Encode()
	}
	payload, err := c.jsonObject("GET", path, nil)
	if err != nil {
		return nil, err
	}
	items, ok := payload["items"].([]any)
	if !ok {
		return []map[string]any{}, nil
	}
	claims := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if claim, ok := item.(map[string]any); ok {
			claims = append(claims, claim)
		}
	}
	return claims, nil
}

// GetSandbox returns one Sandbox by name.
func (c *K8sClient) GetSandbox(namespace, name string) (map[string]any, error) {
	ns := k8sQuotePath(namespace)
	sandbox := k8sQuotePath(name)
	return c.jsonObject("GET", c.SandboxBase+"/namespaces/"+ns+"/sandboxes/"+sandbox, nil)
}

// DeleteClaim deletes one SandboxClaim, refusing if its UID changed (the UID
// precondition prevents deleting a claim that was recycled meanwhile). It
// returns false when the claim is already gone.
func (c *K8sClient) DeleteClaim(namespace, name, uid string) (bool, error) {
	ns := k8sQuotePath(namespace)
	claim := k8sQuotePath(name)
	body, err := json.Marshal(k8sDeleteOptions{APIVersion: "v1", Kind: "DeleteOptions", Preconditions: k8sPreconditions{UID: uid}})
	if err != nil {
		return false, err
	}
	status, raw, err := c.raw("DELETE", c.ClaimBase+"/namespaces/"+ns+"/sandboxclaims/"+claim, body)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status >= 400 {
		return false, &K8sApiError{Status: status, Body: k8sDecodeBody(raw), Method: "DELETE", Path: claim}
	}
	return true, nil
}

// GetConfigMap returns the ConfigMap, or nil when it does not exist (a missing
// ledger is the normal first-run state, not an error).
func (c *K8sClient) GetConfigMap(namespace, name string) (map[string]any, error) {
	ns := k8sQuotePath(namespace)
	cm := k8sQuotePath(name)
	status, raw, err := c.raw("GET", "/api/v1/namespaces/"+ns+"/configmaps/"+cm, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status >= 400 {
		return nil, &K8sApiError{Status: status, Body: k8sDecodeBody(raw), Method: "GET", Path: cm}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, &K8sApiError{
			Status: status,
			Body:   "invalid ConfigMap JSON: " + err.Error(),
			Method: "GET",
			Path:   cm,
		}
	}
	return payload, nil
}

// CreateConfigMap creates the ledger ConfigMap with the given data.
func (c *K8sClient) CreateConfigMap(namespace, name string, data map[string]string) (map[string]any, error) {
	ns := k8sQuotePath(namespace)
	body, err := json.Marshal(k8sConfigMapBody{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata:   k8sConfigMapMetadata{Name: name, Namespace: namespace},
		Data:       data,
	})
	if err != nil {
		return nil, err
	}
	return c.jsonObject("POST", "/api/v1/namespaces/"+ns+"/configmaps", body)
}

// ReplaceConfigMap replaces the ledger ConfigMap, asserting the resource
// version it was read at so a concurrent writer is not clobbered (the caller
// re-reads and merges on a 409).
func (c *K8sClient) ReplaceConfigMap(namespace, name string, data map[string]string, resourceVersion string) (map[string]any, error) {
	ns := k8sQuotePath(namespace)
	cm := k8sQuotePath(name)
	body, err := json.Marshal(k8sConfigMapBody{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: k8sConfigMapMetadata{
			Name:            name,
			Namespace:       namespace,
			ResourceVersion: resourceVersion,
		},
		Data: data,
	})
	if err != nil {
		return nil, err
	}
	return c.jsonObject("PUT", "/api/v1/namespaces/"+ns+"/configmaps/"+cm, body)
}

// k8sDeleteOptions is the DELETE body; field order matches Python's json.dumps of
// the equivalent dict.
type k8sDeleteOptions struct {
	APIVersion    string           `json:"apiVersion"`
	Kind          string           `json:"kind"`
	Preconditions k8sPreconditions `json:"preconditions"`
}

type k8sPreconditions struct {
	UID string `json:"uid"`
}

// k8sConfigMapBody is the create/replace body; field order matches Python.
type k8sConfigMapBody struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   k8sConfigMapMetadata `json:"metadata"`
	Data       map[string]string    `json:"data"`
}

type k8sConfigMapMetadata struct {
	Name            string `json:"name"`
	Namespace       string `json:"namespace"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}
