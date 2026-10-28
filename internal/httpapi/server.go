// Package httpapi exposes the ledgerline-fx HTTP API: rate lookups,
// conversion, and a health check.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ledgerline/ledgerline-fx/internal/fxstore"
)

// Server wires the fxstore.Store to HTTP handlers.
type Server struct {
	store          *fxstore.Store
	refreshEnabled bool
	mux            *http.ServeMux
}

// NewServer builds a Server and registers its routes.
func NewServer(store *fxstore.Store, refreshEnabled bool) *Server {
	s := &Server{store: store, refreshEnabled: refreshEnabled, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/rates/{base}/{quote}", s.handleRate)
	s.mux.HandleFunc("POST /v1/convert", s.handleConvert)
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
}

type rateResponse struct {
	Base  string  `json:"base"`
	Quote string  `json:"quote"`
	Rate  float64 `json:"rate"`
	AsOf  string  `json:"as_of"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

func (s *Server) handleRate(w http.ResponseWriter, r *http.Request) {
	base := r.PathValue("base")
	quote := r.PathValue("quote")

	rate, asOf, err := s.store.Rate(base, quote)
	if err != nil {
		if errors.Is(err, fxstore.ErrUnknownCurrency) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, rateResponse{
		Base:  normalizeCode(base),
		Quote: normalizeCode(quote),
		Rate:  rate,
		AsOf:  asOf.UTC().Format(time.RFC3339),
	})
}

type convertRequest struct {
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	AmountMinor int64  `json:"amount_minor"`
}

type convertResponse struct {
	Base                 string  `json:"base"`
	Quote                string  `json:"quote"`
	AmountMinor          int64   `json:"amount_minor"`
	ConvertedAmountMinor int64   `json:"converted_amount_minor"`
	Rate                 float64 `json:"rate"`
}

func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	var req convertRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.Base == "" || req.Quote == "" {
		writeError(w, http.StatusBadRequest, "base and quote are required")
		return
	}

	converted, rate, _, err := s.store.ConvertMinor(req.Base, req.Quote, req.AmountMinor)
	if err != nil {
		if errors.Is(err, fxstore.ErrUnknownCurrency) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, convertResponse{
		Base:                 normalizeCode(req.Base),
		Quote:                normalizeCode(req.Quote),
		AmountMinor:          req.AmountMinor,
		ConvertedAmountMinor: converted,
		Rate:                 rate,
	})
}

type healthzResponse struct {
	Status         string `json:"status"`
	RatesAsOf      string `json:"rates_as_of"`
	RefreshEnabled bool   `json:"refresh_enabled"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthzResponse{
		Status:         "ok",
		RatesAsOf:      s.store.AsOf().UTC().Format(time.RFC3339),
		RefreshEnabled: s.refreshEnabled,
	})
}

func normalizeCode(code string) string {
	out := make([]byte, 0, len(code))
	for i := 0; i < len(code); i++ {
		c := code[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
