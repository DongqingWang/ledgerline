// Package fxstore holds the in-memory, USD-anchored FX rate table and the
// logic for deriving arbitrary cross rates and converting minor-unit amounts.
package fxstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrUnknownCurrency is returned when a requested currency code is not
// present in the current rate table.
var ErrUnknownCurrency = errors.New("unknown currency")

// baseCurrency is the currency all seed/refreshed rates are anchored to.
const baseCurrency = "USD"

// SeedFile is the on-disk JSON shape for the static fixture rate table.
type SeedFile struct {
	Base  string             `json:"base"`
	Rates map[string]float64 `json:"rates"`
}

// LoadSeedFile reads and parses a seed rate file from disk.
func LoadSeedFile(path string) (*SeedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading seed file %q: %w", path, err)
	}
	var sf SeedFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing seed file %q: %w", path, err)
	}
	return &sf, nil
}

// RateSource is implemented by anything that can fetch a fresh USD-anchored
// rate table (currency code -> units of that currency per 1 USD). The live
// HTTP implementation lives in internal/fxsource; tests inject fakes here so
// the default test run never touches the network.
type RateSource interface {
	FetchRates(ctx context.Context) (map[string]float64, error)
}

// Store holds the current USD-anchored rate table plus the timestamp it was
// last loaded/refreshed at. It is safe for concurrent use.
type Store struct {
	mu    sync.RWMutex
	rates map[string]float64
	asOf  time.Time
}

// NewStore builds a Store from a seed/refresh payload. The rates map must be
// anchored to USD (i.e. rates["USD"] == 1.0) and non-empty.
func NewStore(rates map[string]float64, asOf time.Time) (*Store, error) {
	normalized, err := normalizeRates(rates)
	if err != nil {
		return nil, err
	}
	return &Store{rates: normalized, asOf: asOf}, nil
}

// NewStoreFromSeedFile loads a Store directly from a seed JSON file on disk.
func NewStoreFromSeedFile(path string, asOf time.Time) (*Store, error) {
	sf, err := LoadSeedFile(path)
	if err != nil {
		return nil, err
	}
	if strings.ToUpper(strings.TrimSpace(sf.Base)) != baseCurrency {
		return nil, fmt.Errorf("seed file base currency must be %s, got %q", baseCurrency, sf.Base)
	}
	return NewStore(sf.Rates, asOf)
}

func normalizeRates(rates map[string]float64) (map[string]float64, error) {
	if len(rates) == 0 {
		return nil, errors.New("rate table is empty")
	}
	normalized := make(map[string]float64, len(rates))
	for code, rate := range rates {
		code = strings.ToUpper(strings.TrimSpace(code))
		if code == "" {
			continue
		}
		if rate <= 0 {
			return nil, fmt.Errorf("rate for %s must be positive, got %v", code, rate)
		}
		normalized[code] = rate
	}
	usdRate, ok := normalized[baseCurrency]
	if !ok {
		return nil, fmt.Errorf("rate table must include an anchor rate for %s", baseCurrency)
	}
	if math.Abs(usdRate-1.0) > 1e-9 {
		return nil, fmt.Errorf("rate table anchor %s must equal 1.0, got %v", baseCurrency, usdRate)
	}
	return normalized, nil
}

// Replace atomically swaps in a freshly fetched rate table. It is used by the
// refresh loop; callers of a failed fetch should simply not call Replace, so
// the previous (last-known-good) table keeps serving.
func (s *Store) Replace(rates map[string]float64, asOf time.Time) error {
	normalized, err := normalizeRates(rates)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rates = normalized
	s.asOf = asOf
	return nil
}

// AsOf returns the timestamp the currently served rate table was loaded at.
func (s *Store) AsOf() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.asOf
}

// Rate returns the exchange rate to convert 1 unit of base into quote units,
// along with the as-of timestamp of the table it was derived from. Both
// currencies must be present in the USD-anchored table; any pair derivable
// from it is supported (not just USD-as-base).
func (s *Store) Rate(base, quote string) (float64, time.Time, error) {
	base = strings.ToUpper(strings.TrimSpace(base))
	quote = strings.ToUpper(strings.TrimSpace(quote))

	s.mu.RLock()
	defer s.mu.RUnlock()

	baseRate, ok := s.rates[base]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("%w: %s", ErrUnknownCurrency, base)
	}
	quoteRate, ok := s.rates[quote]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("%w: %s", ErrUnknownCurrency, quote)
	}
	// rates are USD-anchored: rates[X] = units of X per 1 USD.
	// units of quote per 1 base = rates[quote] / rates[base].
	return quoteRate / baseRate, s.asOf, nil
}

// ConvertMinor converts an integer minor-unit amount (e.g. cents) from base
// to quote using the current table, rounding to the nearest integer minor
// unit. See README "known simplifications" for why differing minor-unit
// granularity across currencies (e.g. JPY) is not modeled here.
func (s *Store) ConvertMinor(base, quote string, amountMinor int64) (convertedMinor int64, rate float64, asOf time.Time, err error) {
	rate, asOf, err = s.Rate(base, quote)
	if err != nil {
		return 0, 0, time.Time{}, err
	}
	converted := math.Round(float64(amountMinor) * rate)
	return int64(converted), rate, asOf, nil
}

// Snapshot returns a copy of the current rate table, for introspection/tests.
func (s *Store) Snapshot() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]float64, len(s.rates))
	for k, v := range s.rates {
		out[k] = v
	}
	return out
}
