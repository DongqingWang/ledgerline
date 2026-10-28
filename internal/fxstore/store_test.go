package fxstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testRates() map[string]float64 {
	return map[string]float64{
		"USD": 1.0,
		"EUR": 0.92,
		"GBP": 0.79,
		"JPY": 150.0,
	}
}

func TestNewStoreRejectsMissingUSDAnchor(t *testing.T) {
	_, err := NewStore(map[string]float64{"EUR": 0.92}, time.Now())
	if err == nil {
		t.Fatal("expected error when USD anchor is missing, got nil")
	}
}

func TestNewStoreRejectsNonUnitUSDAnchor(t *testing.T) {
	_, err := NewStore(map[string]float64{"USD": 1.1, "EUR": 0.92}, time.Now())
	if err == nil {
		t.Fatal("expected error when USD anchor != 1.0, got nil")
	}
}

func TestNewStoreRejectsEmptyTable(t *testing.T) {
	_, err := NewStore(map[string]float64{}, time.Now())
	if err == nil {
		t.Fatal("expected error for empty rate table, got nil")
	}
}

func TestNewStoreRejectsNonPositiveRate(t *testing.T) {
	_, err := NewStore(map[string]float64{"USD": 1.0, "EUR": 0}, time.Now())
	if err == nil {
		t.Fatal("expected error for non-positive rate, got nil")
	}
}

func TestUSDAnchoredRate(t *testing.T) {
	asOf := time.Now()
	s, err := NewStore(testRates(), asOf)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	rate, gotAsOf, err := s.Rate("USD", "EUR")
	if err != nil {
		t.Fatalf("Rate(USD,EUR): %v", err)
	}
	if rate != 0.92 {
		t.Errorf("Rate(USD,EUR) = %v, want 0.92", rate)
	}
	if !gotAsOf.Equal(asOf) {
		t.Errorf("as_of = %v, want %v", gotAsOf, asOf)
	}
}

func TestDerivedCrossRate(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// EUR/GBP = (USD/GBP) / (USD/EUR) = 0.79 / 0.92
	rate, _, err := s.Rate("EUR", "GBP")
	if err != nil {
		t.Fatalf("Rate(EUR,GBP): %v", err)
	}
	want := 0.79 / 0.92
	if diff := rate - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("Rate(EUR,GBP) = %v, want %v", rate, want)
	}
}

func TestSameCurrencyRateIsOne(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rate, _, err := s.Rate("JPY", "JPY")
	if err != nil {
		t.Fatalf("Rate(JPY,JPY): %v", err)
	}
	if rate != 1.0 {
		t.Errorf("Rate(JPY,JPY) = %v, want 1.0", rate)
	}
}

func TestRateIsCaseInsensitive(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rate, _, err := s.Rate("usd", "eur")
	if err != nil {
		t.Fatalf("Rate(usd,eur): %v", err)
	}
	if rate != 0.92 {
		t.Errorf("Rate(usd,eur) = %v, want 0.92", rate)
	}
}

func TestUnknownCurrencyReturnsSentinelError(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if _, _, err := s.Rate("USD", "XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("Rate(USD,XYZ) error = %v, want ErrUnknownCurrency", err)
	}
	if _, _, err := s.Rate("XYZ", "USD"); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("Rate(XYZ,USD) error = %v, want ErrUnknownCurrency", err)
	}
}

func TestConvertMinorRounding(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// 10000 cents USD -> EUR at 0.92 => 9200 cents exactly.
	converted, rate, _, err := s.ConvertMinor("USD", "EUR", 10000)
	if err != nil {
		t.Fatalf("ConvertMinor: %v", err)
	}
	if converted != 9200 {
		t.Errorf("converted = %d, want 9200", converted)
	}
	if rate != 0.92 {
		t.Errorf("rate = %v, want 0.92", rate)
	}

	// 333 cents USD -> EUR at 0.92 => 306.36 -> rounds to 306.
	converted, _, _, err = s.ConvertMinor("USD", "EUR", 333)
	if err != nil {
		t.Fatalf("ConvertMinor: %v", err)
	}
	if converted != 306 {
		t.Errorf("converted = %d, want 306", converted)
	}

	// 1 cent USD -> EUR at 0.92 => 0.92 -> rounds to 1.
	converted, _, _, err = s.ConvertMinor("USD", "EUR", 1)
	if err != nil {
		t.Fatalf("ConvertMinor: %v", err)
	}
	if converted != 1 {
		t.Errorf("converted = %d, want 1 (0.92 rounds up)", converted)
	}
}

func TestConvertMinorUnknownCurrency(t *testing.T) {
	s, err := NewStore(testRates(), time.Now())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, _, _, err := s.ConvertMinor("USD", "XYZ", 100); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("ConvertMinor(USD,XYZ) error = %v, want ErrUnknownCurrency", err)
	}
}

func TestNewStoreFromSeedFile(t *testing.T) {
	s, err := NewStoreFromSeedFile("../../rates_seed.json", time.Now())
	if err != nil {
		t.Fatalf("NewStoreFromSeedFile: %v", err)
	}
	rate, _, err := s.Rate("USD", "JPY")
	if err != nil {
		t.Fatalf("Rate(USD,JPY): %v", err)
	}
	if rate != 150.0 {
		t.Errorf("Rate(USD,JPY) = %v, want 150.0", rate)
	}
}

// fakeSource is a test double for RateSource that never touches the network.
type fakeSource struct {
	rates map[string]float64
	err   error
	calls int
}

func (f *fakeSource) FetchRates(ctx context.Context) (map[string]float64, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rates, nil
}

func TestRefresherSwapsInNewRates(t *testing.T) {
	s, err := NewStore(testRates(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	src := &fakeSource{rates: map[string]float64{"USD": 1.0, "EUR": 0.5}}
	r := NewRefresher(s, src, time.Minute, nil)

	if err := r.RefreshOnce(context.Background()); err != nil {
		t.Fatalf("RefreshOnce: %v", err)
	}

	rate, _, err := s.Rate("USD", "EUR")
	if err != nil {
		t.Fatalf("Rate(USD,EUR): %v", err)
	}
	if rate != 0.5 {
		t.Errorf("rate after refresh = %v, want 0.5", rate)
	}
	if src.calls != 1 {
		t.Errorf("source called %d times, want 1", src.calls)
	}
}

func TestRefresherKeepsLastKnownGoodOnFetchFailure(t *testing.T) {
	originalAsOf := time.Now().Add(-time.Hour)
	s, err := NewStore(testRates(), originalAsOf)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	src := &fakeSource{err: errors.New("network unreachable")}
	r := NewRefresher(s, src, time.Minute, nil)

	if err := r.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected RefreshOnce to return the fetch error, got nil")
	}

	// The store must still serve the original seed rates untouched.
	rate, asOf, err := s.Rate("USD", "EUR")
	if err != nil {
		t.Fatalf("Rate(USD,EUR): %v", err)
	}
	if rate != 0.92 {
		t.Errorf("rate after failed refresh = %v, want unchanged 0.92", rate)
	}
	if !asOf.Equal(originalAsOf) {
		t.Errorf("as_of after failed refresh = %v, want unchanged %v", asOf, originalAsOf)
	}
}

func TestRefresherRejectsInvalidFetchedTable(t *testing.T) {
	originalAsOf := time.Now().Add(-time.Hour)
	s, err := NewStore(testRates(), originalAsOf)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// Missing USD anchor should be rejected and leave the store untouched.
	src := &fakeSource{rates: map[string]float64{"EUR": 0.5}}
	r := NewRefresher(s, src, time.Minute, nil)

	if err := r.RefreshOnce(context.Background()); err == nil {
		t.Fatal("expected RefreshOnce to reject an invalid table, got nil error")
	}

	rate, _, err := s.Rate("USD", "EUR")
	if err != nil {
		t.Fatalf("Rate(USD,EUR): %v", err)
	}
	if rate != 0.92 {
		t.Errorf("rate after rejected refresh = %v, want unchanged 0.92", rate)
	}
}
