package syntheticproxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func resp429(body, retryAfter string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestClassify429 pins the three-way discrimination. Getting this wrong in
// either direction is a real cost: misreading parallel_limit as failover-worthy
// burns an OpenRouter failover on a condition that clears in seconds, and
// missing a genuine rate limit serves a 429 to the client.
func TestClassify429(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		retryAfter   string
		wantReason   string
		wantFailover bool
	}{
		{
			name:       "parallel limit is the flavour carrying Retry-After",
			body:       `{"error":"max 4 parallel requests per model"}`,
			retryAfter: "1",
			// no failover: transient, clears as in-flight calls finish
			wantReason: reasonParallelLimit, wantFailover: false,
		},
		{
			name: "quota exhausted is identified by its body",
			body: `{"error":"You've exceeded your subscription rate limits. Upgrade, or try again later."}`,
			// failover: the allowance is spent
			wantReason: reasonQuotaExhausted, wantFailover: true,
		},
		{
			name:       "generic rate limit is identified by its body",
			body:       `{"error":"Too many requests, try again later"}`,
			wantReason: reasonRateLimited, wantFailover: true,
		},
		{
			name: "an unrecognised 429 still fails over",
			body: `{"error":"something Synthetic has not said before"}`,
			// the safe default: one wasted failover beats serving a 429
			wantReason: reasonRateLimited, wantFailover: true,
		},
		{
			name:       "an empty body still fails over",
			body:       ``,
			wantReason: reasonRateLimited, wantFailover: true,
		},
		{
			name:       "Retry-After outranks the body",
			body:       `{"error":"You've exceeded your subscription rate limits"}`,
			retryAfter: "5",
			// the header is the verified discriminator for the transient case
			wantReason: reasonParallelLimit, wantFailover: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := resp429(tc.body, tc.retryAfter)
			reason, failover := classify429(resp)
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if failover != tc.wantFailover {
				t.Errorf("failover = %v, want %v", failover, tc.wantFailover)
			}

			// Whatever the verdict, the caller must still see the whole body:
			// peekBody's prefix has to be re-attached, or a passed-through 429
			// reaches the client truncated.
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read restored body: %v", err)
			}
			if string(got) != tc.body {
				t.Errorf("body after classification = %q, want %q", got, tc.body)
			}
		})
	}
}

// TestClassify429RestoresBodiesLargerThanThePeek guards the peek window: a 429
// body bigger than bodyPeekBytes must come back complete, and a quota marker
// past the window degrades only the LABEL (both flavours fail over), never the
// bytes.
func TestClassify429RestoresBodiesLargerThanThePeek(t *testing.T) {
	pad := strings.Repeat("x", bodyPeekBytes+1024)

	big := `{"error":"Too many requests, try again later","pad":"` + pad + `"}`
	resp := resp429(big, "")
	if reason, failover := classify429(resp); reason != reasonRateLimited || !failover {
		t.Fatalf("reason=%q failover=%v, want %q/true", reason, failover, reasonRateLimited)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != len(big) || string(got) != big {
		t.Fatalf("body truncated: got %d bytes, want %d", len(got), len(big))
	}

	// Marker beyond the peek window: mislabelled as rate_limited, but still
	// failover-worthy and still byte-complete.
	late := `{"pad":"` + pad + `","error":"exceeded your subscription rate limits"}`
	resp2 := resp429(late, "")
	reason, failover := classify429(resp2)
	if !failover {
		t.Fatalf("late-marker body must still fail over, got reason=%q failover=%v", reason, failover)
	}
	got2, _ := io.ReadAll(resp2.Body)
	if string(got2) != late {
		t.Fatalf("late-marker body truncated: got %d bytes, want %d", len(got2), len(late))
	}
}

// TestApplyFailoverRuleReports429Reasons pins the metric the reasons exist for:
// each flavour is counted under its own label, and only the failover-worthy two
// are rewritten to a 503.
func TestApplyFailoverRuleReports429Reasons(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		retryAfter string
		wantReason string
		wantStatus int
	}{
		{"quota_exhausted", `{"error":"exceeded your subscription rate limits"}`, "", reasonQuotaExhausted, 503},
		{"rate_limited", `{"error":"Too many requests, try again later"}`, "", reasonRateLimited, 503},
		{"parallel_limit", `{"error":"max 4 parallel requests per model"}`, "1", reasonParallelLimit, 429},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProxy(t)
			req, _ := http.NewRequest(http.MethodPost, "https://api.synthetic.new/v1/chat/completions", nil)
			info := &reqInfo{kh: "cccc3333"}
			req = req.WithContext(context.WithValue(req.Context(), infoKey, info))

			resp := resp429(tc.body, tc.retryAfter)
			resp.Request = req
			p.applyFailoverRule(info, resp)

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			var sb strings.Builder
			p.metrics.Write(&sb, p.store.Len())
			want := `synthetic_proxy_upstream_429_total{reason="` + tc.wantReason + `"} 1`
			if !strings.Contains(sb.String(), want) {
				t.Errorf("metrics missing %q\n---\n%s", want, sb.String())
			}
			// One flavour must not be counted under another's label.
			for _, other := range []string{reasonQuotaExhausted, reasonRateLimited, reasonParallelLimit} {
				if other == tc.wantReason {
					continue
				}
				if strings.Contains(sb.String(), `synthetic_proxy_upstream_429_total{reason="`+other+`"}`) {
					t.Errorf("flavour %s also counted as %s", tc.wantReason, other)
				}
			}
		})
	}
}
