package syntheticproxy

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Probe checks that Synthetic is reachable and accepts the key, using the
// cheapest documented endpoint (/v1/models, ~0.5s observed). It is the
// fallback signal when the quota endpoint itself fails, so the proxy can still
// distinguish "Synthetic is down" (answer 503) from "we simply could not read
// the balance" (forward, fail-open).
func (p *Proxy) Probe(ctx context.Context, authHeader string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Upstream+p.cfg.HealthPath, nil)
	if err != nil {
		return err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("probe HTTP %d", resp.StatusCode)
	}
	return nil
}
