package guard

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- jsonResult -------------------------------------------------------------

func TestJsonResultSortsKeysAndSetsContentType(t *testing.T) {
	res := jsonResult(http.StatusOK, map[string]any{"zebra": 1, "alpha": "two", "middle": true}, nil)

	if res.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	if res.ContentType != "application/json" {
		t.Fatalf("content type = %q, want application/json", res.ContentType)
	}
	// Python json.dumps uses ", "/": " separators; the bytes must match.
	if got, want := string(res.Body), `{"alpha": "two", "middle": true, "zebra": 1}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestJsonResultKeepsNestedSortOrder(t *testing.T) {
	payload := map[string]any{
		"b": map[string]any{"y": 2, "x": 1},
		"a": []any{map[string]any{"q": 1, "p": 2}},
	}

	res := jsonResult(http.StatusForbidden, payload, nil)
	want := `{"a": [{"p": 2, "q": 1}], "b": {"x": 1, "y": 2}}`
	if got := string(res.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// TestJsonResultEscapesLikePython pins the ensure_ascii contract: every rune
// outside printable ASCII is escaped, `/`, `&`, `<`, `>` are not.
func TestJsonResultEscapesLikePython(t *testing.T) {
	payload := map[string]any{
		"host":   "münchen.de",
		"detail": "bad <host> & url/",
		"ctrl":   "\x00\x1f\x7f",
		"emoji":  "\U0001f600",
		"max":    "\U0010ffff",
		"plain":  "~/x?y&z",
	}
	want := `{"ctrl": "\u0000\u001f\u007f", "detail": "bad <host> & url/", "emoji": "\ud83d\ude00", "host": "m\u00fcnchen.de", "max": "\udbff\udfff", "plain": "~/x?y&z"}`
	res := jsonResult(http.StatusOK, payload, nil)
	if got := string(res.Body); got != want {
		t.Fatalf("body = %s\n want %s", got, want)
	}
}

func TestJsonResultNullsAndBooleans(t *testing.T) {
	// emit_event sends null session fields on an unauthenticated deny.
	res := jsonResult(http.StatusOK, map[string]any{"session_hash": nil, "ok": true, "n": 0}, nil)
	want := `{"n": 0, "ok": true, "session_hash": null}`
	if got := string(res.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// TestJsonResultFormatsFloatsLikePython pins repr semantics: a whole float
// keeps its ".0" and a very small float uses exponent form.
func TestJsonResultFormatsFloatsLikePython(t *testing.T) {
	res := jsonResult(http.StatusOK, map[string]any{"stripes": 1.0, "ratio": 0.5, "tiny": 1e-5, "big": 1e16}, nil)
	want := `{"big": 1e+16, "ratio": 0.5, "stripes": 1.0, "tiny": 1e-05}`
	if got := string(res.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestJsonResultSerializesTypedStructsSorted(t *testing.T) {
	type health struct {
		Status        string   `json:"status"`
		Role          string   `json:"role"`
		PolicyVersion string   `json:"policy_version"`
		Profiles      []string `json:"profiles"`
		hidden        string   // unexported: never emitted
	}

	res := jsonResult(http.StatusOK, health{
		Status:        "ok",
		Role:          "authorizer",
		PolicyVersion: "v1",
		Profiles:      []string{"offline", "python"},
		hidden:        "skip",
	}, nil)
	want := `{"policy_version": "v1", "profiles": ["offline", "python"], "role": "authorizer", "status": "ok"}`
	if got := string(res.Body); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestJsonResultPreservesExtraHeaders(t *testing.T) {
	res := jsonResult(http.StatusForbidden, map[string]any{"decision": "deny"}, map[string]string{
		"x-egress-decision": "deny",
		"x-egress-kill":     "1",
	})
	if res.Headers["x-egress-decision"] != "deny" || res.Headers["x-egress-kill"] != "1" {
		t.Fatalf("headers = %v", res.Headers)
	}
	if got := string(res.Body); got != `{"decision": "deny"}` {
		t.Fatalf("body = %s", got)
	}
}

// TestJsonResultRejectsUnsupportedMaps keeps a bad payload visible instead of
// silently emitting a broken document.
func TestJsonResultRejectsUnsupportedMaps(t *testing.T) {
	res := jsonResult(http.StatusOK, map[string]any{"a": map[int]string{1: "x"}}, nil)
	if res.Status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.Status)
	}
	if string(res.Body) != `{"error": "internal-error"}` {
		t.Fatalf("body = %s", res.Body)
	}
}

// --- writeResult ------------------------------------------------------------

func TestWriteResultSetsBodyHeadersAndStatus(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResult(recorder, jsonResult(http.StatusConflict, map[string]any{"error": "replay"}, map[string]string{"x-egress-reason": "replay"}))

	response := recorder.Result()
	defer response.Body.Close()

	if response.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if got, want := response.Header.Get("Content-Length"), "19"; got != want {
		t.Fatalf("content length = %q, want %q", got, want)
	}
	if got := response.Header.Get("x-egress-reason"); got != "replay" {
		t.Fatalf("x-egress-reason = %q", got)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The header must equal the real body: Python sends this same pair.
	if len(body) != 19 || string(body) != `{"error": "replay"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestWriteResultShipsRawBytesWithCustomContentType(t *testing.T) {
	// /metrics goes through HttpResult directly with Prometheus text.
	recorder := httptest.NewRecorder()
	writeResult(recorder, &HttpResult{
		Status:      http.StatusOK,
		Body:        []byte("# HELP hermes_quarantine_total x\nhermes_quarantine_total 0\n"),
		ContentType: "text/plain; version=0.0.4",
	})

	if got := recorder.Header().Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("content type = %q", got)
	}
	if recorder.Body.String() != "# HELP hermes_quarantine_total x\nhermes_quarantine_total 0\n" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}

func TestWriteResultHandlesEmptyBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResult(recorder, &HttpResult{Status: http.StatusNoContent})

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Length"); got != "0" {
		t.Fatalf("content length = %q, want 0", got)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

// --- readBody ---------------------------------------------------------------

// newRequest builds a request the way net/http's server does, so ContentLength
// and Body carry the same relationship the real server produces.
func newRequest(body []byte, contentLength int64) *http.Request {
	return &http.Request{
		Method:        http.MethodPost,
		ContentLength: contentLength,
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
}

func TestReadBodyRejectsBodyOverTheCap(t *testing.T) {
	// 64 KiB + 1: ContentLength alone decides, the body is never read.
	oversized := make([]byte, maxBodyBytes+1)
	_, err := readBody(newRequest(oversized, int64(len(oversized))))
	if err == nil {
		t.Fatal("expected an error for an oversized body")
	}
	if err.Error() != "request body too large: 65537 bytes" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestReadBodyAcceptsExactlyTheCap(t *testing.T) {
	body := bytes.Repeat([]byte("a"), maxBodyBytes)
	got, err := readBody(newRequest(body, int64(maxBodyBytes)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != maxBodyBytes {
		t.Fatalf("len = %d, want %d", len(got), maxBodyBytes)
	}
}

func TestReadBodyRejectsTruncatedBody(t *testing.T) {
	// Content-Length promises 100 bytes, the stream carries 10.
	_, err := readBody(newRequest([]byte("0123456789"), 100))
	if err == nil {
		t.Fatal("expected an error for a truncated body")
	}
	if err.Error() != "truncated request body" {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestReadBodyRejectsMissingOrEmptyBody(t *testing.T) {
	for _, length := range []int64{0, -1} {
		_, err := readBody(newRequest(nil, length))
		if err == nil {
			t.Fatalf("length %d: expected an error", length)
		}
		if err.Error() != "missing or empty request body" {
			t.Fatalf("length %d: error = %q", length, err.Error())
		}
	}
}

func TestReadBodyReturnsTheRawBytes(t *testing.T) {
	raw := []byte(`{"ts": 1700000000, "kind":"kill"}`)
	got, err := readBody(newRequest(raw, int64(len(raw))))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Ordering and spacing must survive: the reaper signs these exact bytes.
	if string(got) != string(raw) {
		t.Fatalf("body = %q, want %q", got, raw)
	}
}

// --- readJSON / readInto ----------------------------------------------------

func TestReadJSONDecodesAnObject(t *testing.T) {
	raw := []byte(`{"kind":"kill","ts":1700000000}`)
	payload, err := readJSON(newRequest(raw, int64(len(raw))))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	object, ok := payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", payload)
	}
	if object["kind"] != "kill" {
		t.Fatalf("kind = %v", object["kind"])
	}
}

func TestReadJSONRejectsUnparseableBody(t *testing.T) {
	raw := []byte("{not json")
	_, err := readJSON(newRequest(raw, int64(len(raw))))
	if err == nil {
		t.Fatal("expected an error for unparseable JSON")
	}
	if !strings.HasPrefix(err.Error(), "invalid JSON body: ") {
		t.Fatalf("error = %q", err.Error())
	}
}

func TestReadJSONPropagatesTheBodyError(t *testing.T) {
	_, err := readJSON(newRequest(nil, 0))
	if err == nil || err.Error() != "missing or empty request body" {
		t.Fatalf("error = %v", err)
	}
}

func TestReadIntoDecodesATypedStruct(t *testing.T) {
	type event struct {
		Kind        string   `json:"kind"`
		TS          int64    `json:"ts"`
		SessionHash string   `json:"session_hash"`
		Strikes     int      `json:"strikes"`
		TargetPort  int      `json:"target_port"`
		Extra       []string `json:"extra"`
	}
	raw := []byte(`{"kind":"kill","ts":1700000000,"session_hash":"d","strikes":1,"target_port":443,"extra":["a"],"unknown":"ignored"}`)
	var decoded event
	if err := readInto(newRequest(raw, int64(len(raw))), &decoded); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if decoded.Kind != "kill" || decoded.TS != 1700000000 || decoded.SessionHash != "d" || decoded.TargetPort != 443 {
		t.Fatalf("decoded = %+v", decoded)
	}
	if len(decoded.Extra) != 1 || decoded.Extra[0] != "a" {
		t.Fatalf("extra = %v", decoded.Extra)
	}
}

func TestReadIntoPropagatesTheBodyError(t *testing.T) {
	var target map[string]any
	if err := readInto(newRequest([]byte("a"), 100), &target); err == nil || err.Error() != "truncated request body" {
		t.Fatalf("error = %v", err)
	}
}

// --- route helpers ----------------------------------------------------------

func TestNotFoundAndBadRequestResults(t *testing.T) {
	notFound := notFoundResult()
	if notFound.Status != http.StatusNotFound || string(notFound.Body) != `{"error": "not-found"}` {
		t.Fatalf("not found = %d %s", notFound.Status, notFound.Body)
	}

	bad := badRequestResult("truncated request body")
	if bad.Status != http.StatusBadRequest {
		t.Fatalf("bad request status = %d", bad.Status)
	}
	// Sorted keys put "detail" before "error", exactly like the Python.
	if got, want := string(bad.Body), `{"detail": "truncated request body", "error": "bad-request"}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// TestHandlerInterfaceMatchesNetHTTP pins the routing contract end to end: a
// typed Handler value is handed straight to an http server, and the response a
// real client observes carries the same bytes as the Python service.
func TestHandlerInterfaceMatchesNetHTTP(t *testing.T) {
	var server Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			writeResult(w, jsonResult(http.StatusOK, map[string]any{"status": "ok", "role": "authorizer"}, nil))
		case r.Method == http.MethodPost && r.URL.Path == "/check":
			var payload map[string]any
			if err := readInto(r, &payload); err != nil {
				writeResult(w, badRequestResult(err.Error()))
				return
			}
			writeResult(w, jsonResult(http.StatusForbidden, map[string]any{"decision": "deny", "strikes": 1, "target": "example.org:443"}, nil))
		default:
			writeResult(w, notFoundResult())
		}
	})

	live := httptest.NewServer(server)
	defer live.Close()

	response, err := http.Get(live.URL + "/healthz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := string(body), `{"role": "authorizer", "status": "ok"}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content type = %q", response.Header.Get("Content-Type"))
	}

	oversized, _ := json.Marshal(map[string]string{"pad": strings.Repeat("a", maxBodyBytes)})
	post, err := http.Post(live.URL+"/check", "application/json", bytes.NewReader(oversized))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	postBody, err := io.ReadAll(post.Body)
	post.Body.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if post.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized status = %d, body = %s", post.StatusCode, postBody)
	}
	if !strings.Contains(string(postBody), `"request body too large: `) {
		t.Fatalf("oversized body = %s", postBody)
	}

	missing, err := http.Get(live.URL + "/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	missingBody, _ := io.ReadAll(missing.Body)
	missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound || string(missingBody) != `{"error": "not-found"}` {
		t.Fatalf("missing = %d %s", missing.StatusCode, missingBody)
	}
}

// TestPythonFloatMatchesRepr is the reference table from Python's json.dumps.
func TestPythonFloatMatchesRepr(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0"},
		{math.Copysign(0, -1), "-0.0"},
		{1, "1.0"},
		{0.5, "0.5"},
		{-1.5, "-1.5"},
		{100, "100.0"},
		{1e15, "1000000000000000.0"},
		{1e16, "1e+16"},
		{1e-4, "0.0001"},
		{1e-5, "1e-05"},
		{1e30, "1e+30"},
		{5e-324, "5e-324"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.NaN(), "NaN"},
	}
	for _, c := range cases {
		if got := pythonFloat(c.in); got != c.want {
			t.Fatalf("pythonFloat(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestMaxBodyBytesIs64KiB(t *testing.T) {
	if maxBodyBytes != 64*1024 {
		t.Fatalf("maxBodyBytes = %d, want 65536", maxBodyBytes)
	}
	if contentTypeJSON != "application/json" {
		t.Fatalf("contentTypeJSON = %q", contentTypeJSON)
	}
}