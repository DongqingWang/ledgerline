package fxsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests run against a local httptest.Server (loopback only) so they
// never make a real network call to the live FX API.

func TestLiveFetchRatesSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"success","base_code":"USD","rates":{"USD":1,"EUR":0.9,"GBP":0.8}}`))
	}))
	defer ts.Close()

	src := NewLive(ts.URL)
	rates, err := src.FetchRates(context.Background())
	if err != nil {
		t.Fatalf("FetchRates: %v", err)
	}
	if rates["EUR"] != 0.9 {
		t.Errorf("EUR rate = %v, want 0.9", rates["EUR"])
	}
}

func TestLiveFetchRatesNonSuccessResult(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"error","base_code":"USD","rates":{}}`))
	}))
	defer ts.Close()

	src := NewLive(ts.URL)
	if _, err := src.FetchRates(context.Background()); err == nil {
		t.Fatal("expected error for result != success, got nil")
	}
}

func TestLiveFetchRatesHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	src := NewLive(ts.URL)
	if _, err := src.FetchRates(context.Background()); err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

func TestLiveFetchRatesWrongBase(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"success","base_code":"EUR","rates":{"EUR":1}}`))
	}))
	defer ts.Close()

	src := NewLive(ts.URL)
	if _, err := src.FetchRates(context.Background()); err == nil {
		t.Fatal("expected error for non-USD base, got nil")
	}
}
