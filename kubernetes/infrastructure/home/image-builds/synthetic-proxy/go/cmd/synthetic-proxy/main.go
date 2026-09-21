// Command synthetic-proxy is the entrypoint: it resolves configuration, starts
// the reverse proxy, and shuts down cleanly on SIGINT/SIGTERM.
//
// A malformed environment value is fatal here (non-zero exit) — that is the
// CrashLoopBackOff contract a misconfigured proxy relies on.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"syntheticproxy"
)

func main() {
	cfg, err := syntheticproxy.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthetic-proxy: config:", err)
		os.Exit(1)
	}

	p := syntheticproxy.NewProxy(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", net.JoinHostPort(cfg.Bind, strconv.Itoa(cfg.Port)))
	if err != nil {
		fmt.Fprintln(os.Stderr, "synthetic-proxy: listen:", err)
		os.Exit(1)
	}

	log.Printf("msg=synthetic-proxy event=start listen=%s upstream=%s quota_path=%s health_path=%s freshness=%s refusal=%s",
		ln.Addr(), cfg.Upstream, cfg.QuotaPath, cfg.HealthPath, cfg.PollInterval, cfg.UnhealthyFor)

	if err := p.Serve(ctx, ln); err != nil {
		log.Printf("msg=synthetic-proxy event=exit err=%q", err.Error())
		os.Exit(1)
	}
	log.Printf("msg=synthetic-proxy event=stop")
}
