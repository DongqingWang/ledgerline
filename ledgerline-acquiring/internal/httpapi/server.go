// Package httpapi exposes the acquiring service's REST API.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ledgerline/ledgerline-acquiring/internal/acquiring"
	"github.com/ledgerline/ledgerline-acquiring/internal/engineclient"
)

// Server wires the acquiring service to HTTP handlers.
type Server struct {
	Svc *acquiring.Service
}

// NewMux builds the routing.
func (s *Server) NewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/payment-intents", s.handleCreate)
	mux.HandleFunc("GET /v1/payment-intents", s.handleList)
	mux.HandleFunc("GET /v1/payment-intents/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/payment-intents/{id}/void", s.handleVoid)
	return mux
}

type createRequest struct {
	DedupID     string `json:"dedup_id"`
	MerchantID  string `json:"merchant_id"`
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor"`
	Currency    string `json:"currency"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.DedupID == "" || req.MerchantID == "" || req.Currency == "" || req.AmountMinor <= 0 {
		writeError(w, http.StatusBadRequest, "dedup_id, merchant_id, currency required; amount_minor must be positive")
		return
	}
	pi, err := s.Svc.CreatePayment(r.Context(), req.DedupID, req.MerchantID, req.AmountMinor, req.FeeMinor, req.Currency)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pi)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	pi, ok := s.Svc.Store.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "intent not found")
		return
	}
	writeJSON(w, http.StatusOK, pi)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.Store.List())
}

func (s *Server) handleVoid(w http.ResponseWriter, r *http.Request) {
	pi, err := s.Svc.VoidPayment(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pi)
}

func writeErr(w http.ResponseWriter, err error) {
	var le *engineclient.Error
	if errors.As(err, &le) {
		writeError(w, le.StatusCode, le.Message)
		return
	}
	writeError(w, http.StatusBadGateway, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
