package syntheticproxy

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// quotaExhaustedBody is the refusal the gateway's eviction CEL matches: it
// contains "exceeded your subscription rate limits" and carries NO Retry-After
// header, so the chain fails over to OpenRouter instead of treating it as the
// transient parallel-limit flavor.
const quotaExhaustedBody = `{"error":"You've exceeded your subscription rate limits. Upgrade, or try again later."}`

// unavailableBody is the 503 body for an unreachable upstream (also evicts, via
// the CEL's response.code >= 500 clause).
const unavailableBody = `{"error":"upstream unavailable"}`

// statusKey carries a *statusHolder into the outbound request context so
// ModifyResponse can record the upstream status for the access log WITHOUT
// wrapping the ResponseWriter. (Wrapping the writer would put an
// obviously-safe passthrough through a function CodeQL treats as an
// untrusted-data sink; letting ReverseProxy write directly keeps the
// sanctioned API as the only writer and the log accurate.)
type ctxKey int

const statusKey ctxKey = 0

type statusHolder struct{ code int }

// Proxy is the running reverse proxy plus its per-key state.
type Proxy struct {
	cfg    Config
	store  *Store
	client *http.Client // quota + probe calls (short timeouts)
	rp     *httputil.ReverseProxy
	upHost string // "host:port" of the upstream, for logging
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
	}

	p := &Proxy{
		cfg:    cfg,
		store:  NewStore(cfg.StoreCap),
		upHost: target.Host,
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
			if h, ok := resp.Request.Context().Value(statusKey).(*statusHolder); ok {
				h.code = resp.StatusCode
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("msg=synthetic-proxy event=upstream_error upstream=%s err=%q", p.upHost, err.Error())
			writeJSON(w, http.StatusServiceUnavailable, unavailableBody)
		},
	}
	return p
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
	mux.HandleFunc("/", p.handle)
	return mux
}

// handle logs the outcome of the per-key gate and transparent forward.
func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	holder := &statusHolder{code: http.StatusOK}
	r = r.WithContext(context.WithValue(r.Context(), statusKey, holder))

	auth := r.Header.Get("Authorization")
	refusal, verdict := p.decide(r.Context(), auth, w, r)

	status := refusal
	if status == 0 {
		status = holder.code
	}
	log.Printf("msg=synthetic-proxy kh=%s verdict=%s status=%d upstream=%s dur_ms=%d",
		shortHash(HashAuth(auth)), verdict, status, p.upHost, time.Since(start).Milliseconds())
}

// decide applies the gate and, when allowed, forwards the request. It returns
// the refusal status (0 when the request was forwarded) and the verdict label.
func (p *Proxy) decide(ctx context.Context, auth string, w http.ResponseWriter, r *http.Request) (int, string) {
	now := time.Now()

	// No credential on the request: nothing to key state on. Forward and let
	// Synthetic (and the gateway's CEL) arbitrate — this is a misconfiguration
	// upstream of this proxy, not a reason to deny here.
	if auth == "" {
		p.rp.ServeHTTP(w, r)
		return 0, "passthrough"
	}

	kh := HashAuth(auth)
	st, seen := p.store.Get(kh)

	// An in-force refusal short-circuits without touching Synthetic.
	if seen && now.Before(st.RefusedUntil) {
		writeJSON(w, http.StatusTooManyRequests, quotaExhaustedBody)
		return http.StatusTooManyRequests, st.Verdict.String()
	}

	// Reuse a recent verdict rather than re-querying on every request.
	if seen && now.Sub(st.LastCheck) < p.cfg.PollInterval {
		switch st.Verdict {
		case VerdictQuotaExhausted:
			writeJSON(w, http.StatusTooManyRequests, quotaExhaustedBody)
			return http.StatusTooManyRequests, "quota_exhausted"
		case VerdictUnhealthy:
			writeJSON(w, http.StatusServiceUnavailable, unavailableBody)
			return http.StatusServiceUnavailable, "unhealthy"
		default:
			p.rp.ServeHTTP(w, r)
			return 0, "healthy"
		}
	}

	// Fresh evaluation.
	snap, err := p.CheckQuota(ctx, auth)
	if err == nil {
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
			nst.RefusedUntil = now.Add(p.cfg.UnhealthyFor)
			p.store.Set(kh, nst)
			writeJSON(w, http.StatusTooManyRequests, quotaExhaustedBody)
			return http.StatusTooManyRequests, "quota_exhausted"
		}
		p.store.Set(kh, nst)
		p.rp.ServeHTTP(w, r)
		return 0, "healthy"
	}

	// Quota endpoint indeterminate: only positive evidence of a DOWN upstream
	// justifies refusing. Otherwise fail open and let Synthetic answer.
	if perr := p.Probe(ctx, auth); perr != nil {
		p.store.Set(kh, KeyState{
			Verdict:      VerdictUnhealthy,
			LastCheck:    now,
			RefusedUntil: now.Add(p.cfg.UnhealthyFor),
		})
		writeJSON(w, http.StatusServiceUnavailable, unavailableBody)
		return http.StatusServiceUnavailable, "unhealthy"
	}

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

// writeJSON writes a JSON refusal with the exact body and no Retry-After.
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
