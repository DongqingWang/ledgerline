package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Server exposes the balance-reader's own read-only HTTP API, backed
// entirely by the in-memory BalanceCache (no network call out to the raft
// cluster on the request path -- that's the whole point of this service).
type Server struct {
	cache *BalanceCache
	mux   *http.ServeMux
}

func NewServer(cache *BalanceCache) *Server {
	s := &Server{
		cache: cache,
		mux:   http.NewServeMux(),
	}
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/v1/balances", s.handleListBalances)
	s.mux.HandleFunc("/v1/balances/", s.handleGetBalance)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

type healthzResponse struct {
	Status         string `json:"status"`
	LastRaftIndex  uint64 `json:"last_raft_index"`
	CachedAccounts int    `json:"cached_accounts"`
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	idx, _ := s.cache.LastProcessedIndex()
	writeJSON(w, http.StatusOK, healthzResponse{
		Status:         "ok",
		LastRaftIndex:  idx,
		CachedAccounts: s.cache.Count(),
	})
}

func (s *Server) handleListBalances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.cache.All())
}

func (s *Server) handleGetBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/balances/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	bal, ok := s.cache.Get(id)
	if !ok {
		http.Error(w, "account not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, bal)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
