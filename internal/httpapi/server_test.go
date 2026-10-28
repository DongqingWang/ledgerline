package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-fx/internal/fxstore"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := fxstore.NewStore(map[string]float64{
		"USD": 1.0,
		"EUR": 0.92,
		"GBP": 0.79,
		"JPY": 150.0,
	}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return NewServer(store, false)
}

func TestHandleRateUSDAnchored(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/rates/USD/EUR", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body rateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Base != "USD" || body.Quote != "EUR" || body.Rate != 0.92 {
		t.Errorf("unexpected body: %+v", body)
	}
	if body.AsOf != "2026-01-01T00:00:00Z" {
		t.Errorf("as_of = %q, want 2026-01-01T00:00:00Z", body.AsOf)
	}
}

func TestHandleRateDerivedCrossRate(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/rates/EUR/GBP", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body rateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	want := 0.79 / 0.92
	if diff := body.Rate - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("rate = %v, want %v", body.Rate, want)
	}
}

func TestHandleRateUnknownCurrencyReturns404(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/rates/USD/XYZ", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestHandleConvert(t *testing.T) {
	s := newTestServer(t)
	reqBody, _ := json.Marshal(convertRequest{Base: "USD", Quote: "EUR", AmountMinor: 10000})
	req := httptest.NewRequest(http.MethodPost, "/v1/convert", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body convertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.ConvertedAmountMinor != 9200 {
		t.Errorf("converted_amount_minor = %d, want 9200", body.ConvertedAmountMinor)
	}
	if body.Rate != 0.92 {
		t.Errorf("rate = %v, want 0.92", body.Rate)
	}
}

func TestHandleConvertUnknownCurrencyReturns404(t *testing.T) {
	s := newTestServer(t)
	reqBody, _ := json.Marshal(convertRequest{Base: "USD", Quote: "XYZ", AmountMinor: 100})
	req := httptest.NewRequest(http.MethodPost, "/v1/convert", bytes.NewReader(reqBody))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleConvertInvalidBody(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/convert", bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHealthz(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body healthzResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status field = %q, want ok", body.Status)
	}
	if body.RefreshEnabled {
		t.Error("refresh_enabled = true, want false for this test server")
	}
}
