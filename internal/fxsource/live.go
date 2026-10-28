// Package fxsource provides the live, network-backed implementation of
// fxstore.RateSource. It is kept separate from fxstore so the core rate
// logic and its tests never need to import net/http, and so tests can
// inject a fake source instead of this one.
package fxsource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// DefaultEndpoint is a free, no-API-key-required FX rate endpoint
// (verified reachable via curl during development: returns USD-anchored
// rates for ~160 currencies, updated roughly daily).
const DefaultEndpoint = "https://open.er-api.com/v6/latest/USD"

// liveAPIResponse mirrors the subset of the open.er-api.com response shape
// that we care about.
type liveAPIResponse struct {
	Result   string             `json:"result"`
	BaseCode string             `json:"base_code"`
	Rates    map[string]float64 `json:"rates"`
}

// Live fetches a USD-anchored rate table from a public HTTP FX API. It
// implements fxstore.RateSource.
type Live struct {
	Endpoint string
	Client   *http.Client
}

// NewLive constructs a Live source with sane defaults.
func NewLive(endpoint string) *Live {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Live{
		Endpoint: endpoint,
		Client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// FetchRates performs the HTTP call and parses the response into a
// USD-anchored map[currency]rate.
func (l *Live) FetchRates(ctx context.Context) (map[string]float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching live rates: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live rate endpoint returned status %d", resp.StatusCode)
	}

	var parsed liveAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding live rate response: %w", err)
	}
	if parsed.Result != "success" {
		return nil, fmt.Errorf("live rate endpoint reported result=%q", parsed.Result)
	}
	if parsed.BaseCode != "USD" {
		return nil, fmt.Errorf("live rate endpoint returned unexpected base %q, want USD", parsed.BaseCode)
	}
	if len(parsed.Rates) == 0 {
		return nil, fmt.Errorf("live rate endpoint returned no rates")
	}
	return parsed.Rates, nil
}
