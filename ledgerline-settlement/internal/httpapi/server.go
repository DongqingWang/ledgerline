// Package httpapi exposes the settlement driver's REST API. Settlement state
// lives in the ledger FSM; these handlers submit commands and read state back
// from the engine, so the service itself is stateless and crash-safe.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ledgerline/ledgerline-settlement/internal/engineclient"
	"github.com/ledgerline/ledgerline-settlement/internal/settlement"
)

// Server wires the orchestrator to HTTP handlers.
type Server struct {
	Orch *settlement.Orchestrator
}

// NewMux builds the routing.
func (s *Server) NewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/deposits", s.handleCreate(engineclient.SettlementDeposit))
	mux.HandleFunc("POST /v1/withdrawals", s.handleCreate(engineclient.SettlementWithdrawal))
	mux.HandleFunc("GET /v1/settlements/{id}", s.handleGet)
	mux.HandleFunc("POST /v1/settlements/{id}/return", s.handleReturn)
	return mux
}

type createRequest struct {
	DedupID     string `json:"dedup_id"`
	AccountID   string `json:"account_id"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

func (s *Server) handleCreate(kind engineclient.SettlementKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
			return
		}
		if req.DedupID == "" || req.AccountID == "" || req.Currency == "" || req.AmountMinor <= 0 {
			writeError(w, http.StatusBadRequest, "dedup_id, account_id, currency required; amount_minor must be positive")
			return
		}
		var stl *engineclient.Settlement
		var err error
		if kind == engineclient.SettlementDeposit {
			stl, err = s.Orch.CreateDeposit(r.Context(), req.DedupID, req.AccountID, req.AmountMinor, req.Currency)
		} else {
			stl, err = s.Orch.CreateWithdrawal(r.Context(), req.DedupID, req.AccountID, req.AmountMinor, req.Currency)
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, stl)
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	stl, err := s.Orch.Engine.GetSettlement(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stl)
}

func (s *Server) handleReturn(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Reason == "" {
		req.Reason = "simulated return"
	}
	stl, err := s.Orch.ReturnSettlement(r.Context(), r.PathValue("id"), req.Reason)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, stl)
}

func writeErr(w http.ResponseWriter, err error) {
	var le *engineclient.Error
	if errors.As(err, &le) {
		writeError(w, le.StatusCode, le.Message)
		return
	}
	if strings.Contains(err.Error(), "want posted") || strings.Contains(err.Error(), "not found") {
		writeError(w, http.StatusConflict, err.Error())
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
