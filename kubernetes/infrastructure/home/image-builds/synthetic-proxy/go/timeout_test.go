package syntheticproxy

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUpstreamHeaderTimeoutBoundsASilentUpstream is the one behaviour that
// cannot be reasoned about from config alone: an upstream that ACCEPTS the
// connection and then never writes response headers. Without
// ResponseHeaderTimeout the request hangs forever, the gateway holds a retry
// slot with no status code to retry on, and the client waits indefinitely —
// the "endless" failure mode rather than a fast error.
//
// Drives the reverse proxy directly (not decide()) so the measurement is the
// transport's, not the quota/probe contexts'.
func TestUpstreamHeaderTimeoutBoundsASilentUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	var mu sync.Mutex
	var accepted []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept and say nothing, ever.
			mu.Lock()
			accepted = append(accepted, c)
			mu.Unlock()
		}
	}()
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range accepted {
			_ = c.Close()
		}
	}()

	p := NewProxy(Config{
		Upstream:              "http://" + ln.Addr().String(),
		QuotaPath:             "/v2/quotas",
		HealthPath:            "/v1/models",
		PollInterval:          time.Minute,
		Bind:                  "127.0.0.1",
		Port:                  8080,
		UnhealthyFor:          time.Minute,
		StoreCap:              8,
		UpstreamHeaderTimeout: 200 * time.Millisecond,
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"x"}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		p.rp.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reverse proxy hung on a silent upstream: UpstreamHeaderTimeout did not bound it")
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("request took %s; the 200ms header timeout did not bound it", elapsed)
	}
	// ReverseProxy calls ErrorHandler on a transport error, which answers 503 —
	// a status the gateway's health policy evicts on, so the failover happens
	// instead of the client waiting.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (the ErrorHandler's unhealthy answer)", rec.Code, http.StatusServiceUnavailable)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "unavailable") {
		t.Errorf("body = %q, want the unavailable payload", body)
	}
}
