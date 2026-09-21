package syntheticproxy

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func debugProxy(t *testing.T, debug bool) *Proxy {
	t.Helper()
	return NewProxy(Config{
		Upstream:              "https://api.synthetic.new:443",
		QuotaPath:             "/v2/quotas",
		HealthPath:            "/v1/models",
		PollInterval:          time.Minute,
		Bind:                  "127.0.0.1",
		Port:                  8080,
		UnhealthyFor:          2 * time.Minute,
		HoldOffFailover:       time.Hour,
		UpstreamHeaderTimeout: time.Second,
		StoreCap:              8,
		DebugErrors:           debug,
	})
}

// TestErrorDumpShowsWhatArrived is the point of the flag: when a new 429
// flavour appears, the status line, the Retry-After value and the body wording
// are what identify it. None of those is available from a metric.
func TestErrorDumpShowsWhatArrived(t *testing.T) {
	p := debugProxy(t, true)
	info := &reqInfo{kh: "eeee5555"}
	resp := resp429(`{"error":"max 4 parallel requests per model","detail":"concurrency"}`, "1")
	resp.Header.Set("X-Request-Id", "req-abc123")

	out := captureLog(t, func() { p.applyFailoverRule(info, resp) })

	for _, want := range []string{
		"event=upstream_error_detail",
		`status="429 Too Many Requests"`,
		"reason=parallel_limit",
		"action=pass_through",
		`retry_after="1"`,
		"X-Request-Id=req-abc123",
		"max 4 parallel requests per model", // the body, verbatim
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n---\n%s", want, out)
		}
	}
}

// TestErrorDumpNeverPrintsACredential guards the one thing a debug dump must not
// do. The Authorization header IS the credential this proxy exists to keep out
// of logs, so it is omitted entirely rather than truncated, and the header is
// named as redacted so the absence is visible rather than silent.
func TestErrorDumpNeverPrintsACredential(t *testing.T) {
	const secret = "Bearer syn_THIS_MUST_NEVER_APPEAR_IN_ANY_LOG"
	p := debugProxy(t, true)
	info := &reqInfo{kh: "ffff6666"}
	resp := resp429(`{"error":"Too many requests, try again later"}`, "")
	resp.Header.Set("Authorization", secret)
	resp.Header.Set("Set-Cookie", "session=also-secret")
	resp.Header.Set("Content-Type", "application/json")

	out := captureLog(t, func() { p.applyFailoverRule(info, resp) })

	if strings.Contains(out, "syn_THIS_MUST_NEVER_APPEAR") {
		t.Fatalf("the dump leaked the credential:\n%s", out)
	}
	if strings.Contains(out, "also-secret") {
		t.Fatalf("the dump leaked a cookie:\n%s", out)
	}
	for _, want := range []string{"event=upstream_error_detail", "redacted_headers=[Authorization Set-Cookie]"} {
		if !strings.Contains(out, want) {
			t.Errorf("dump missing %q\n---\n%s", want, out)
		}
	}
	// ...while the non-sensitive headers still show, or the dump is useless.
	if !strings.Contains(out, "Content-Type=application/json") {
		t.Errorf("dump dropped a harmless header:\n%s", out)
	}
}

// TestErrorDumpOffByDefault keeps the flag from becoming ambient noise.
func TestErrorDumpOffByDefault(t *testing.T) {
	p := debugProxy(t, false)
	info := &reqInfo{kh: "aaaa7777"}
	resp := resp429(`{"error":"Too many requests, try again later"}`, "")

	out := captureLog(t, func() { p.applyFailoverRule(info, resp) })
	if strings.Contains(out, "event=upstream_error_detail") {
		t.Fatalf("detail dumped with DEBUG_ERRORS off:\n%s", out)
	}
	// The ordinary line is still emitted: only the dump is gated.
	if !strings.Contains(out, "event=upstream_429") {
		t.Errorf("the ordinary 429 line disappeared:\n%s", out)
	}
}

// TestErrorDumpIsReadOnlyForTheClient proves the dump does not disturb the
// response: peekBody re-attaches the prefix, so a passed-through 429 still
// carries its whole body with the dump enabled.
func TestErrorDumpIsReadOnlyForTheClient(t *testing.T) {
	const body = `{"error":"max 4 parallel requests per model","pad":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"}`
	p := debugProxy(t, true)
	info := &reqInfo{kh: "bbbb8888"}
	resp := resp429(body, "1")

	captureLog(t, func() { p.applyFailoverRule(info, resp) })

	got := make([]byte, len(body))
	n, _ := resp.Body.Read(got)
	if string(got[:n]) != body {
		t.Fatalf("body after the dump = %q, want %q", string(got[:n]), body)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (parallel limit passes through)", resp.StatusCode)
	}
}
