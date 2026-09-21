package syntheticproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// QuotaSnapshot is the parsed view of Synthetic's /v2/quotas response that the
// verdict depends on. Only the fields with a fail-over meaning are kept.
//
// Observed live schema (2026-09-21, HTTP 200):
//
//	{"subscription":{"limit":2000,"requests":0,"renewsAt":"..."},
//	 "rollingFiveHourLimit":{"remaining":1131.9,"max":2000,"limited":false},
//	 "weeklyTokenLimit":{"percentRemaining":60.7,...}}
type QuotaSnapshot struct {
	Limit                  int
	Requests               int
	RenewsAt               time.Time
	RollingLimited         bool
	WeeklyPercentRemaining float64
	HaveWeekly             bool
}

// Exhausted reports whether the snapshot is positive evidence that Synthetic
// will (or does) refuse this key. Any single trigger is enough.
//
// NOTE: a zero Limit means "no subscription ceiling configured" and must NOT
// by itself read as exhausted — otherwise a missing/renamed field would black
// hole all traffic.
func (q QuotaSnapshot) Exhausted() bool {
	if q.Limit > 0 && q.Requests >= q.Limit {
		return true
	}
	if q.RollingLimited {
		return true
	}
	if q.HaveWeekly && q.WeeklyPercentRemaining <= 0 {
		return true
	}
	return false
}

// quotaWire mirrors the observed response. Pointers distinguish "absent" from
// "zero" so an absent block never triggers a spurious verdict.
type quotaWire struct {
	Subscription *struct {
		Limit    int    `json:"limit"`
		Requests int    `json:"requests"`
		RenewsAt string `json:"renewsAt"`
	} `json:"subscription"`
	RollingFiveHourLimit *struct {
		Limited bool `json:"limited"`
	} `json:"rollingFiveHourLimit"`
	WeeklyTokenLimit *struct {
		PercentRemaining *float64 `json:"percentRemaining"`
	} `json:"weeklyTokenLimit"`
}

// CheckQuota queries Synthetic's authoritative balance endpoint with the given
// Authorization header (forwarded verbatim). A non-2xx response or a body that
// will not parse is returned as an error; the caller decides what an
// indeterminate answer means.
func (p *Proxy) CheckQuota(ctx context.Context, authHeader string) (QuotaSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.Upstream+p.cfg.QuotaPath, nil)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return QuotaSnapshot{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return QuotaSnapshot{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return QuotaSnapshot{}, fmt.Errorf("quota endpoint HTTP %d", resp.StatusCode)
	}

	var w quotaWire
	if err := json.Unmarshal(body, &w); err != nil {
		return QuotaSnapshot{}, fmt.Errorf("quota parse: %w", err)
	}

	var q QuotaSnapshot
	if w.Subscription != nil {
		q.Limit = w.Subscription.Limit
		q.Requests = w.Subscription.Requests
		if t, err := time.Parse(time.RFC3339, w.Subscription.RenewsAt); err == nil {
			q.RenewsAt = t
		}
	}
	if w.RollingFiveHourLimit != nil {
		q.RollingLimited = w.RollingFiveHourLimit.Limited
	}
	if w.WeeklyTokenLimit != nil && w.WeeklyTokenLimit.PercentRemaining != nil {
		q.HaveWeekly = true
		q.WeeklyPercentRemaining = *w.WeeklyTokenLimit.PercentRemaining
	}
	return q, nil
}
