// Package httpapi exposes the reconciliation service's REST API.
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ledgerline/ledgerline-recon/internal/engineclient"
	"github.com/ledgerline/ledgerline-recon/internal/recon"
)

// Server wires the recon service to HTTP handlers.
type Server struct {
	Svc *recon.Service
}

// NewMux builds the routing.
func (s *Server) NewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /v1/recon/ledger", s.handleIngestLedger)
	mux.HandleFunc("POST /v1/recon/external", s.handleIngestExternal)
	mux.HandleFunc("POST /v1/recon/run", s.handleRun)
	mux.HandleFunc("GET /v1/recon/breaks", s.handleBreaks)
	mux.HandleFunc("POST /v1/recon/adjust", s.handleAdjust)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

func (s *Server) handleIngestLedger(w http.ResponseWriter, r *http.Request) {
	var records []recon.Record
	if err := json.NewDecoder(r.Body).Decode(&records); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.Svc.IngestLedger(records)
	writeJSON(w, http.StatusOK, map[string]int{"ingested": len(records)})
}

func (s *Server) handleIngestExternal(w http.ResponseWriter, r *http.Request) {
	var records []recon.Record
	if err := json.NewDecoder(r.Body).Decode(&records); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	s.Svc.IngestExternal(records)
	writeJSON(w, http.StatusOK, map[string]int{"ingested": len(records)})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.Run())
}

func (s *Server) handleBreaks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.LastBreaks())
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	var b recon.Break
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if err := s.Svc.Adjust(r.Context(), b); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"state": "adjusted", "ref": b.Ref})
}

func writeErr(w http.ResponseWriter, err error) {
	var le *engineclient.Error
	if errors.As(err, &le) {
		writeError(w, le.StatusCode, le.Message)
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
