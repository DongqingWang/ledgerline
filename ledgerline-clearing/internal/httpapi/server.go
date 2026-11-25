// Package httpapi exposes the clearing service's REST API.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ledgerline/ledgerline-clearing/internal/clearing"
	"github.com/ledgerline/ledgerline-clearing/internal/engineclient"
)

// Server wires the clearing service to HTTP handlers.
type Server struct {
	Svc *clearing.Service
}

// NewMux builds the routing.
func (s *Server) NewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/clearing/captures", s.handleIngest)
	mux.HandleFunc("POST /v1/clearing/run", s.handleRun)
	mux.HandleFunc("GET /v1/clearing/batches", s.handleBatches)
	return mux
}

type ingestRequest struct {
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	NetMinor   int64  `json:"net_minor"`
	Ref        string `json:"ref"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.MerchantID == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "merchant_id and currency required")
		return
	}
	s.Svc.Ingest(clearing.Capture{MerchantID: req.MerchantID, Currency: req.Currency, NetMinor: req.NetMinor, Ref: req.Ref})
	writeJSON(w, http.StatusOK, map[string]string{"state": "ingested"})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	batch, err := s.Svc.RunClearing(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, batch)
}

func (s *Server) handleBatches(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.Batches())
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
