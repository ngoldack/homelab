package syntheticproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"
)

// unavailableBody is the 503 body for a transport-level failure to reach
// Synthetic (connection refused, TLS, timeout).
const unavailableBody = `{"error":"upstream unavailable"}`

// unhealthyBody is the single error shape this proxy returns when it has
// decided the Synthetic leg must not be used.
//
// WHY one shape for every reason: all the 429-flavour defensive logic now lives
// HERE, and the gateway's health policy is reduced to the single rule "the
// proxy reported the leg unhealthy -> evict and fail over". The gateway
// therefore never sniffs a body or a header, and — because agentgateway
// populates a response body field only when a CEL references it — the path
// contains no body buffering at all. The reason is still carried for the logs
// and for a human reading the error.
func unhealthyBody(reason string) string {
	return `{"error":"upstream unhealthy","reason":"` + reason + `"}`
}

// infoKey carries a *reqInfo into the outbound request context so
// ModifyResponse can (a) record the upstream status for the access log without
// wrapping the ResponseWriter — wrapping it would put an obviously-safe
// passthrough through a function CodeQL treats as an untrusted-data sink — and
// (b) apply the failover decision to the upstream answer in flight.
type ctxKey int

const infoKey ctxKey = 0

// reqInfo is the per-request scratch ModifyResponse needs.
type reqInfo struct {
	// kh is the caller's key hash; "" when the request carried no credential.
	kh   string
	code int
}

// Proxy is the running reverse proxy plus its per-key state.
type Proxy struct {
	cfg     Config
	store   *Store
	client  *http.Client // quota + probe calls (short timeouts)
	rp      *httputil.ReverseProxy
	upHost  string // "host:port" of the upstream, for logging
	metrics *Metrics
}

// NewProxy wires the proxy against cfg.
//
// NOTE on the per-key freshness model: the plan described a background poller
// that re-checks each cached key on an interval. That is not implementable
// without retaining the raw credential (the quota call needs the key), which
// the same design forbids: this process persists only SHA-256 hashes. The
// coherent realisation is used instead — a key's verdict is (re)computed when a
// request actually arrives for it, and is reused for PollInterval (the same
// knob, now a freshness TTL). This is also strictly better for the goal: the
// proxy never issues an unprompted request to Synthetic, so it cannot poke a
// key that is sitting out its rate-limit recovery window.
func NewProxy(cfg Config) *Proxy {
	target, err := url.Parse(cfg.Upstream)
	if err != nil {
		// Callers pass a value already validated by LoadConfig, but be loud.
		panic("synthetic-proxy: bad upstream " + cfg.Upstream + ": " + err.Error())
	}

	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
		// Time-to-FIRST-BYTE only. Without this, an upstream that accepts the
		// connection and then never answers hangs the request forever: the
		// gateway would hold a retry slot with no status code to retry on, and
		// the client would wait indefinitely. It does not bound the body, so a
		// long SSE completion still streams for as long as it needs.
		ResponseHeaderTimeout: cfg.UpstreamHeaderTimeout,
	}

	p := &Proxy{
		cfg:     cfg,
		store:   NewStore(cfg.StoreCap),
		upHost:  target.Host,
		metrics: NewMetrics(target.Host),
		// No overall client timeout: quota/probe calls carry their own ctx.
		client: &http.Client{Transport: transport},
	}

	p.rp = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			// Present Synthetic with its own hostname (the inbound Host is the
			// in-cluster proxy address). Authorization is left untouched.
			req.Host = target.Host
		},
		Transport: transport,
		// -1 flushes every write immediately; required for SSE token streaming.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			info, _ := resp.Request.Context().Value(infoKey).(*reqInfo)
			p.metrics.IncResponse(resp.StatusCode)
			p.applyFailoverRule(info, resp)
			if info != nil {
				info.code = resp.StatusCode
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// No response exists to dump, so report what WOULD have been called:
			// this is the shape of a connect/DNS/TLS failure.
			log.Printf("msg=synthetic-proxy event=upstream_error upstream=%s method=%s path=%s err=%q",
				p.upHost, r.Method, r.URL.Path, err.Error())
			writeJSON(w, http.StatusServiceUnavailable, unavailableBody)
		},
	}
	return p
}

// applyFailoverRule is the whole point of this service: it reads Synthetic's
// answer, decides whether the leg should be used again, remembers that per key,
// and rewrites the answer the gateway will see.
//
// The rule (header-only, so the SSE body is never read):
//   - 5xx                       -> Synthetic is broken        -> unhealthy
//   - 429 WITHOUT Retry-After   -> a real rate limit           -> unhealthy
//   - 429 WITH Retry-After      -> transient parallel-limit    -> pass through
//   - anything else             -> healthy
//
// The operator-verified discriminator is the Retry-After header: only the
// transient concurrency flavour sends it, and that flavour must NOT fail the
// chain over.
//
// When the verdict is "unhealthy", the response is rewritten to a 503 IN
// FLIGHT. That is what makes the gateway's policy a single code-only rule and
// still fails over on the very request that discovered the problem, rather than
// only the next one.
// The three distinct 429 conditions Synthetic returns, each with a different
// correct response. They are a closed set so they can be labelled directly in
// the metrics rather than inferred later from a body.
const (
	// reasonQuotaExhausted: the subscription allowance is spent. Fails over.
	reasonQuotaExhausted = "quota_exhausted"
	// reasonRateLimited: the generic rate-limit state, which only clears after
	// a long window with NO new request (the one that stayed 429 for 8h+ while
	// it kept being poked). Fails over.
	reasonRateLimited = "rate_limited"
	// reasonParallelLimit: per-model concurrency ("max 4 parallel requests per
	// model"). Transient — it clears as in-flight calls finish — and the ONLY
	// flavour that carries Retry-After, so it must NOT fail over.
	reasonParallelLimit = "parallel_limit"
)

// bodyPeekBytes bounds how much of a 429 body is inspected to tell "quota
// exhausted" from "generic rate limit". Both are short JSON errors at the head.
const bodyPeekBytes = 8 << 10

// peekBody reads up to n bytes from resp.Body and re-attaches a reader that
// still yields the WHOLE body (prefix included), returning the prefix. Only
// ever called on a 429, whose body is a short JSON error — never on the success
// path, which stays an untouched stream.
func peekBody(resp *http.Response, n int) []byte {
	if resp.Body == nil {
		return nil
	}
	var buf bytes.Buffer
	written, err := io.CopyN(&buf, resp.Body, int64(n))
	if err != nil && written == 0 {
		return nil
	}
	prefix := buf.Bytes()
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), resp.Body), resp.Body}
	return prefix
}

// classify429 names which of the three flavours this 429 is, and whether it is
// fail-over-worthy.
//
// parallel_limit is identified by the Retry-After header alone: operator-
// verified that it is the only flavour that sends one. The other two are
// separated by their bodies (Synthetic's own wording), which is why this is the
// only place the proxy reads a body — and peekBody puts the prefix back, so a
// passed-through parallel_limit reaches the client byte-identical.
//
// The default for an unrecognised failover-worthy 429 is rate_limited, i.e.
// anything that is not positively the quota text still fails over. Being wrong
// in that direction costs one failover; the other direction serves a 429 to the
// client.
func classify429(resp *http.Response) (reason string, failover bool) {
	if resp.Header.Get("Retry-After") != "" {
		return reasonParallelLimit, false
	}
	prefix := peekBody(resp, bodyPeekBytes)
	if bytes.Contains(prefix, []byte("exceeded your subscription rate limits")) {
		return reasonQuotaExhausted, true
	}
	return reasonRateLimited, true
}

// verdictForReason maps a failover-worthy 429 reason onto the verdict this
// proxy reports for the key (and therefore onto the refusal body).
func verdictForReason(reason string) Verdict {
	if reason == reasonQuotaExhausted {
		return VerdictQuotaExhausted
	}
	return VerdictRateLimited
}

// sensitiveHeaders are dropped from the debug dump rather than printed in any
// form: Authorization IS the credential this proxy exists to keep out of logs,
// and a dump is exactly where it would otherwise leak.
var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"X-Api-Key":           true,
	"Proxy-Authorization": true,
}

// debugBodyBytes bounds the body sample in the dump. Much larger than the
// verdict-relevant head needs to be, because the point is to SEE the payload.
const debugBodyBytes = 8 << 10

// truncateBytes renders a body sample as a string, capped, so one pathological
// error body cannot flood the log.
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "...(truncated)"
}

// logErrorDetail reports what an upstream error actually looked like — the one
// thing no metric can show. On a 429 the interesting facts are the status line,
// whether Retry-After is present and what it says, and the body wording, since
// those are precisely what distinguishes Synthetic's three flavours. When a new
// flavour appears, this dump is what says so.
//
// Gated by DEBUG_ERRORS (verbose). The body sample is capped and re-attached by
// peekBody, so the client's response is byte-identical with the dump on or off,
// and the credential headers are omitted entirely.
func (p *Proxy) logErrorDetail(info *reqInfo, resp *http.Response, reason, action string) {
	if !p.cfg.DebugErrors || resp.StatusCode < 400 {
		return
	}
	sample := peekBody(resp, debugBodyBytes)

	hdrs := make([]string, 0, len(resp.Header))
	redacted := make([]string, 0, 1)
	for k, v := range resp.Header {
		if sensitiveHeaders[http.CanonicalHeaderKey(k)] {
			redacted = append(redacted, k)
			continue
		}
		hdrs = append(hdrs, k+"="+strings.Join(v, ";"))
	}
	sort.Strings(hdrs)
	sort.Strings(redacted)

	if reason == "" {
		reason = "-"
	}
	if action == "" {
		action = "-"
	}
	// A real http.Response always carries Status, but synthesise it if not: an
	// empty status in a diagnostic dump is worse than useless.
	status := resp.Status
	if status == "" {
		status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	log.Printf("msg=synthetic-proxy event=upstream_error_detail kh=%s status=%q reason=%s action=%s retry_after=%q redacted_headers=%v header_count=%d headers={%s} body_bytes=%d body=%q",
		shortHash(keyHash(info)), status, reason, action,
		resp.Header.Get("Retry-After"), redacted, len(resp.Header),
		strings.Join(hdrs, " "), len(sample), truncateBytes(sample, 700))
}

// holdOffFor picks how long a verdict keeps its key off. Two different clocks
// are wanted, and conflating them is expensive in both directions:
//
//   - the failover flavours (a spent allowance, the generic rate limit) need
//     the LONG one, because that state only clears after a long no-poke
//     stretch; re-probing early wastes the attempt and holds the window open;
//   - transport unreachability and 5xx are transient and get the SHORT one, so
//     a blip does not park the chain for an hour.
func (p *Proxy) holdOffFor(v Verdict) time.Duration {
	switch v {
	case VerdictQuotaExhausted, VerdictRateLimited:
		return p.cfg.HoldOffFailover
	default:
		return p.cfg.UnhealthyFor
	}
}

// keyHash is info.kh with a nil guard, for the log lines in this file.
func keyHash(info *reqInfo) string {
	if info == nil {
		return ""
	}
	return info.kh
}

func (p *Proxy) applyFailoverRule(info *reqInfo, resp *http.Response) {
	var verdict Verdict
	switch {
	case resp.StatusCode >= 500:
		verdict = VerdictUnhealthy
		p.logErrorDetail(info, resp, "", "hold_off")
	case resp.StatusCode == http.StatusTooManyRequests:
		reason, failover := classify429(resp)
		p.metrics.Inc429(reason)
		if !failover {
			// Transient concurrency: hand it straight back and remember
			// nothing, so the chain does not fail over and does not evict.
			log.Printf("msg=synthetic-proxy event=upstream_429 kh=%s reason=%s action=pass_through",
				shortHash(keyHash(info)), reason)
			p.logErrorDetail(info, resp, reason, "pass_through")
			return
		}
		log.Printf("msg=synthetic-proxy event=upstream_429 kh=%s reason=%s action=hold_off",
			shortHash(keyHash(info)), reason)
		// Dump BEFORE the rewrite below, so the dump is Synthetic's answer and
		// not the 503 this proxy is about to substitute for it.
		p.logErrorDetail(info, resp, reason, "hold_off")
		verdict = verdictForReason(reason)
	default:
		// Any other error status passes through, but is still worth dumping
		// when debugging: a 400/401/403 here means our own request shape is
		// wrong, which no 429 handling would reveal.
		p.logErrorDetail(info, resp, "", "pass_through")
		return
	}

	if info != nil && info.kh != "" {
		now := time.Now()
		hold := p.holdOffFor(verdict)
		p.store.Set(info.kh, KeyState{
			Verdict:      verdict,
			RefusedUntil: now.Add(hold),
			LastCheck:    now,
		})
		log.Printf("msg=synthetic-proxy event=hold_off kh=%s verdict=%s upstream_status=%d holdoff=%s",
			shortHash(info.kh), verdict, resp.StatusCode, hold)
	}

	rebaseToUnhealthy(resp, verdict.String())
}

// rebaseToUnhealthy rewrites an upstream answer into this proxy's single
// unhealthy shape (503 + a small JSON body).
//
// The upstream body is closed and replaced rather than read: the original may
// be a streaming SSE error, and an unconsumed body must not be left for the
// transport. Content-Encoding/Transfer-Encoding are dropped because the
// replacement body is neither compressed nor chunked, and Retry-After is
// dropped so nothing downstream can re-classify this as the transient flavour.
func rebaseToUnhealthy(resp *http.Response, reason string) {
	if resp.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}
	body := []byte(unhealthyBody(reason))
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.StatusCode = http.StatusServiceUnavailable
	resp.Status = "503 Service Unavailable"

	h := resp.Header
	h.Del("Content-Encoding")
	h.Del("Transfer-Encoding")
	h.Del("Retry-After")
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
}

// Handler returns the decision-then-forward handler.
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Liveness/readiness for the kubelet: this process is up and ready to
		// proxy. It deliberately does NOT call Synthetic (that would be a poke
		// on every probe interval).
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", p.metricsHandler)
	mux.HandleFunc("/", p.handle)
	return mux
}

// metricsHandler serves the Prometheus exposition. It is registered before the
// catch-all, which would otherwise forward /metrics to the upstream.
func (p *Proxy) metricsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	p.metrics.Write(w, p.store.Len())
}

// handle logs the outcome of the per-key gate and transparent forward.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	auth := r.Header.Get("Authorization")
	kh := ""
	if auth != "" {
		kh = HashAuth(auth)
	}
	info := &reqInfo{kh: kh, code: http.StatusOK}
	r = r.WithContext(context.WithValue(r.Context(), infoKey, info))

	refusal, verdict := p.decide(r.Context(), auth, kh, w, r)

	status := refusal
	if status == 0 {
		status = info.code
	}
	elapsed := time.Since(start)
	p.metrics.IncRequests(verdict)
	p.metrics.ObserveDuration(elapsed.Seconds())
	log.Printf("msg=synthetic-proxy kh=%s verdict=%s status=%d upstream=%s dur_ms=%d",
		shortHash(kh), verdict, status, p.upHost, elapsed.Milliseconds())
}

// decide applies the gate and, when allowed, forwards the request. It returns
// the refusal status (0 when the request was forwarded) and the verdict label.
func (p *Proxy) decide(ctx context.Context, auth, kh string, w http.ResponseWriter, r *http.Request) (int, string) {
	now := time.Now()

	// No credential on the request: nothing to key state on. Forward and let
	// Synthetic (and the gateway) arbitrate — this is a misconfiguration
	// upstream of this proxy, not a reason to deny here.
	if auth == "" {
		p.rp.ServeHTTP(w, r)
		return 0, "passthrough"
	}

	st, seen := p.store.Get(kh)

	// An in-force refusal short-circuits without touching Synthetic. This is
	// the hold-off that lets the gateway's eviction be short.
	if seen && now.Before(st.RefusedUntil) {
		return refuse(w, st.Verdict)
	}

	// Reuse a recent verdict rather than re-querying on every request.
	if seen && now.Sub(st.LastCheck) < p.cfg.PollInterval {
		if st.Verdict != VerdictHealthy {
			return refuse(w, st.Verdict)
		}
		p.rp.ServeHTTP(w, r)
		return 0, "healthy"
	}

	// Fresh evaluation.
	snap, err := p.CheckQuota(ctx, auth)
	if err == nil {
		p.metrics.IncQuota("ok")
		nst := KeyState{
			Verdict:                VerdictHealthy,
			QuotaUsed:              snap.Requests,
			QuotaLimit:             snap.Limit,
			QuotaRenewsAt:          snap.RenewsAt,
			RollingLimited:         snap.RollingLimited,
			WeeklyPercentRemaining: snap.WeeklyPercentRemaining,
			LastCheck:              now,
		}
		if snap.Exhausted() {
			nst.Verdict = VerdictQuotaExhausted
			nst.RefusedUntil = now.Add(p.holdOffFor(VerdictQuotaExhausted))
			p.store.Set(kh, nst)
			return refuse(w, VerdictQuotaExhausted)
		}
		p.store.Set(kh, nst)
		p.rp.ServeHTTP(w, r)
		return 0, "healthy"
	}
	p.metrics.IncQuota("error")

	// Quota endpoint indeterminate: only positive evidence of a DOWN upstream
	// justifies refusing. Otherwise fail open and let Synthetic answer (its
	// answer then goes through applyFailoverRule like any other).
	if perr := p.Probe(ctx, auth); perr != nil {
		p.metrics.IncProbe("error")
		p.store.Set(kh, KeyState{
			Verdict:      VerdictUnhealthy,
			LastCheck:    now,
			RefusedUntil: now.Add(p.holdOffFor(VerdictUnhealthy)),
		})
		return refuse(w, VerdictUnhealthy)
	}
	p.metrics.IncProbe("ok")

	p.store.Set(kh, KeyState{Verdict: VerdictHealthy, LastCheck: now})
	p.rp.ServeHTTP(w, r)
	return 0, "healthy"
}

// Serve runs the HTTP server until ctx is cancelled, then shuts it down.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           p.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// No ReadTimeout/WriteTimeout: request bodies and SSE responses are
		// long-lived by design.
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// refuse answers a request from local state, without touching Synthetic. Every
// refusal is a 503 carrying the reason, for the single code-only rule the
// gateway now applies.
func refuse(w http.ResponseWriter, v Verdict) (int, string) {
	writeJSON(w, http.StatusServiceUnavailable, unhealthyBody(v.String()))
	return http.StatusServiceUnavailable, v.String()
}

// writeJSON writes a JSON body with the given status.
func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// shortHash trims a key hash to its first 8 hex chars for logging (never the
// raw value, never the full hash).
func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
