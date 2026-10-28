package fxstore

import (
	"context"
	"log"
	"time"
)

// Refresher periodically pulls a fresh rate table from a RateSource and
// swaps it into a Store. On fetch failure it logs the error and leaves the
// Store's last-known-good table (the static seed, if no refresh has ever
// succeeded) untouched.
type Refresher struct {
	store    *Store
	source   RateSource
	interval time.Duration
	logger   *log.Logger
	now      func() time.Time // overridable for tests
}

// NewRefresher constructs a Refresher. logger may be nil, in which case the
// standard library default logger is used.
func NewRefresher(store *Store, source RateSource, interval time.Duration, logger *log.Logger) *Refresher {
	if logger == nil {
		logger = log.Default()
	}
	return &Refresher{
		store:    store,
		source:   source,
		interval: interval,
		logger:   logger,
		now:      time.Now,
	}
}

// RefreshOnce performs a single fetch-and-replace cycle. On success the
// Store is updated and nil is returned. On failure the Store is left
// untouched and the error is returned (callers running a loop should log it
// and continue rather than treat it as fatal).
func (r *Refresher) RefreshOnce(ctx context.Context) error {
	rates, err := r.source.FetchRates(ctx)
	if err != nil {
		return err
	}
	return r.store.Replace(rates, r.now())
}

// Run blocks, refreshing on r.interval until ctx is cancelled. Fetch errors
// are logged and swallowed so the last-known-good rates keep serving.
func (r *Refresher) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.RefreshOnce(ctx); err != nil {
				r.logger.Printf("fx refresh failed, keeping last-known-good rates (as_of=%s): %v", r.store.AsOf().Format(time.RFC3339), err)
			} else {
				r.logger.Printf("fx refresh succeeded, rates as_of=%s", r.store.AsOf().Format(time.RFC3339))
			}
		}
	}
}
