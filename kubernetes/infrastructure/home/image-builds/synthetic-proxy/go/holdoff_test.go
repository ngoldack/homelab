package syntheticproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	return NewProxy(Config{
		Upstream:              "https://api.synthetic.new:443",
		QuotaPath:             "/v2/quotas",
		HealthPath:            "/v1/models",
		PollInterval:          time.Minute,
		Bind:                  "127.0.0.1",
		Port:                  8080,
		UnhealthyFor:          time.Minute,
		HoldOffFailover:       10 * time.Minute,
		UpstreamHeaderTimeout: time.Second,
		StoreCap:              8,
	})
}

// upstreamResp drives applyFailoverRule the way ModifyResponse does and returns
// the response the gateway would see.
func upstreamResp(t *testing.T, p *Proxy, kh string, code int, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.synthetic.new/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	info := &reqInfo{kh: kh}
	req = req.WithContext(context.WithValue(req.Context(), infoKey, info))
	h := http.Header{}
	for k, v := range header {
		h.Set(k, v)
	}
	resp := &http.Response{
		StatusCode: code,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("upstream original body")),
		Request:    req,
	}
	p.applyFailoverRule(info, resp)
	return resp
}

func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// TestApplyFailoverRule pins the whole 429-flavour contract, which now lives
// only here. The gateway applies a single code-only rule, so this is the one
// place that must get the discrimination right.
func TestApplyFailoverRule(t *testing.T) {
	tests := []struct {
		name         string
		upstreamCode int
		header       map[string]string
		wantCode     int
		wantReason   string // "" => pass through untouched
		wantHoldoff  bool
	}{
		{
			name:         "generic rate-limit 429 fails over",
			upstreamCode: 429,
			wantCode:     503,
			wantReason:   "rate_limited",
			wantHoldoff:  true,
		},
		{
			name:         "transient parallel-limit 429 passes through",
			upstreamCode: 429,
			header:       map[string]string{"Retry-After": "1"},
			wantCode:     429,
			wantHoldoff:  false,
		},
		{
			name:         "5xx fails over",
			upstreamCode: 502,
			wantCode:     503,
			wantReason:   "unhealthy",
			wantHoldoff:  true,
		},
		{
			name:         "2xx untouched",
			upstreamCode: 200,
			wantCode:     200,
			wantHoldoff:  false,
		},
		{
			name:         "4xx other than 429 untouched",
			upstreamCode: 400,
			wantCode:     400,
			wantHoldoff:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestProxy(t)
			const kh = "aaaa1111"
			before := time.Now()
			resp := upstreamResp(t, p, kh, tt.upstreamCode, tt.header)

			if resp.StatusCode != tt.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantCode)
			}
			body := bodyOf(t, resp)
			if tt.wantReason == "" {
				if !strings.Contains(body, "upstream original body") {
					t.Fatalf("passed-through body was rewritten: %q", body)
				}
			} else {
				if !strings.Contains(body, tt.wantReason) {
					t.Fatalf("body %q does not name reason %q", body, tt.wantReason)
				}
				// A Retry-After on the rewritten answer would let anything
				// downstream re-classify it as the transient flavour.
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					t.Fatalf("rewritten answer carries Retry-After %q", ra)
				}
				if resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Transfer-Encoding") != "" {
					t.Fatalf("rewritten answer kept stale framing headers: %v", resp.Header)
				}
			}

			st, seen := p.store.Get(kh)
			heldOff := seen && st.RefusedUntil.After(before)
			if heldOff != tt.wantHoldoff {
				t.Fatalf("held off = %v (verdict=%s), want %v", heldOff, st.Verdict, tt.wantHoldoff)
			}
		})
	}
}

// TestApplyFailoverRuleWithoutKey guards the no-credential path: there is no
// key to remember, but the failover decision must still apply.
func TestApplyFailoverRuleWithoutKey(t *testing.T) {
	p := newTestProxy(t)
	resp := upstreamResp(t, p, "", 429, nil)
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if p.store.Len() != 0 {
		t.Fatalf("store has %d entries, want 0", p.store.Len())
	}
}

// TestRefuseIsAlwaysUnhealthy pins the gateway-facing contract: every local
// refusal is a 503 naming the reason, so the gateway needs no body or header
// sniffing and may apply a single code-only rule.
func TestRefuseIsAlwaysUnhealthy(t *testing.T) {
	for _, v := range []Verdict{VerdictRateLimited, VerdictQuotaExhausted, VerdictUnhealthy} {
		rec := httptest.NewRecorder()
		code, verdict := refuse(rec, v)
		if code != http.StatusServiceUnavailable || rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: code = %d (recorder %d), want 503", v, code, rec.Code)
		}
		if verdict != v.String() {
			t.Errorf("%s: verdict label = %q, want %q", v, verdict, v.String())
		}
		if !strings.Contains(rec.Body.String(), v.String()) {
			t.Errorf("%s: body %q does not name the reason", v, rec.Body.String())
		}
		if ra := rec.Header().Get("Retry-After"); ra != "" {
			t.Errorf("%s: refusal must not carry Retry-After, got %q", v, ra)
		}
	}
}

// TestHoldOffShortCircuitsWithoutUpstream proves the point of holding state:
// once a key has been held off, the next decision refuses from local state, so
// Synthetic is not poked again while it recovers.
func TestHoldOffShortCircuitsWithoutUpstream(t *testing.T) {
	p := newTestProxy(t)
	const kh = "bbbb2222"
	upstreamResp(t, p, kh, 429, nil) // records the hold-off

	// A second call through decide() must refuse locally. The upstream here is
	// unreachable on purpose: if decide tried to talk to it, this would fail
	// differently (it would still refuse, but only after a network attempt).
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer syn_whatever")
	status, verdict := p.decide(req.Context(), "Bearer syn_whatever", kh, rec, req)

	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if verdict != "rate_limited" {
		t.Fatalf("verdict = %q, want rate_limited", verdict)
	}
}
