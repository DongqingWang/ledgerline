// Package httpapi exposes the orchestrator's own HTTP surface: the
// routing table, the split endpoint, and a health check.
package httpapi

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-orchestrator/internal/controller"
	"github.com/ledgerline/ledgerline-orchestrator/internal/shard"
)

// Server wires the HTTP handlers to the shared routing table and
// orchestrator controller.
type Server struct {
	Table        *shard.Table
	Orchestrator *controller.Orchestrator
	Logger       *log.Logger
}

// NewMux builds the http.Handler for the orchestrator.
func (s *Server) NewMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/routing", s.handleRouting)
	mux.HandleFunc("/v1/split", s.handleSplit)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.Table.Snapshot())
}

func (s *Server) handleSplit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req controller.SplitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
		return
	}

	if s.Logger != nil {
		s.Logger.Printf("split requested: split_point=%d new_nodes=%v", req.SplitPoint, req.NewShardLedgerNodes)
	}

	resp := s.Orchestrator.Split(r.Context(), req)

	if s.Logger != nil {
		s.Logger.Printf("split result: status=%s reason=%q migrated_accounts=%d migrated_transfers=%d",
			resp.Status, resp.Reason, resp.MigratedAccounts, resp.MigratedTransfers)
	}

	status := http.StatusOK
	if resp.Status == "aborted" {
		status = http.StatusConflict
	}
	writeJSON(w, status, resp)
}
