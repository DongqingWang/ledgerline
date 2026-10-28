// Package httpapi exposes the engine's client-facing contract described in
// the Ledgerline shared contracts doc: account/transfer operations, leader
// discovery, and the committed-event stream. Deliberately plain JSON-over-
// HTTP (no protobuf/gRPC codegen) so any Ledgerline repo — or a curious
// contributor — can talk to it with nothing but curl.
package httpapi

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/hashicorp/raft"

	"github.com/ledgerline/ledgerline-raft/internal/fsm"
	"github.com/ledgerline/ledgerline-raft/internal/raftnode"
	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

const applyTimeout = 5 * time.Second

// Config wires an HTTP server to a running raft node. PeerHTTPAddrs maps
// every cluster member's raft ServerID to its HTTP address (including this
// node's own), used to resolve the leader's raft ID into an address clients
// can actually retry against.
type Config struct {
	Node          *raftnode.Node
	PeerHTTPAddrs map[string]string
}

type Server struct {
	cfg Config
}

func NewServer(cfg Config) *Server { return &Server{cfg: cfg} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/accounts", s.handleCreateAccount)
	mux.HandleFunc("GET /v1/accounts/{id}", s.handleGetAccount)
	mux.HandleFunc("POST /v1/transfers", s.handleAssertTransfer)
	mux.HandleFunc("GET /v1/leader", s.handleGetLeader)
	mux.HandleFunc("GET /v1/events/stream", s.handleEventStream)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

func (s *Server) leaderHint() string {
	_, leaderID := s.cfg.Node.Raft.LeaderWithID()
	if leaderID == "" {
		return ""
	}
	return s.cfg.PeerHTTPAddrs[string(leaderID)]
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"state": s.cfg.Node.Raft.State().String(),
	})
}

func (s *Server) handleGetLeader(w http.ResponseWriter, r *http.Request) {
	_, leaderID := s.cfg.Node.Raft.LeaderWithID()
	if leaderID == "" {
		writeError(w, http.StatusServiceUnavailable, ledgertypes.ErrInternal, "no leader elected yet", "")
		return
	}
	writeJSON(w, http.StatusOK, ledgertypes.LeaderInfo{
		LeaderID:       string(leaderID),
		LeaderHTTPAddr: s.cfg.PeerHTTPAddrs[string(leaderID)],
	})
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	var req ledgertypes.CreateAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.AccountID == "" || req.Currency == "" {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "account_id and currency are required", "")
		return
	}

	cmd := fsm.Command{Type: fsm.CmdCreateAccount, CreateAccount: &req}
	result, err := s.apply(cmd)
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	if result.IsError() {
		writeError(w, statusForErrCode(result.ErrCode), result.ErrCode, result.ErrMsg, "")
		return
	}
	writeJSON(w, http.StatusCreated, result.Account)
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	account, ok := s.cfg.Node.FSM.Account(id)
	if !ok {
		writeError(w, http.StatusNotFound, ledgertypes.ErrAccountNotFound, fmt.Sprintf("account %q not found", id), "")
		return
	}
	// Note: this reads local FSM state directly, which is only linearizable
	// on the leader — a follower may be momentarily behind. Callers that
	// need a strongly-consistent read should route through the leader (see
	// GET /v1/leader) rather than relying on this endpoint's node choice.
	writeJSON(w, http.StatusOK, account)
}

func (s *Server) handleAssertTransfer(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	var req ledgertypes.TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" || req.FromAccount == "" || req.ToAccount == "" || req.AmountMinor <= 0 {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest,
			"dedup_id, from_account, to_account are required and amount_minor must be positive", "")
		return
	}

	cmd := fsm.Command{Type: fsm.CmdAssertTransfer, AssertTransfer: &req}
	result, err := s.apply(cmd)
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	if result.IsError() {
		writeError(w, statusForErrCode(result.ErrCode), result.ErrCode, result.ErrMsg, "")
		return
	}
	writeJSON(w, http.StatusOK, result.Transfer)
}

// apply submits a command through raft and waits for it to commit and be
// applied on this (the leader's) FSM, returning the typed ApplyResult.
func (s *Server) apply(cmd fsm.Command) (*fsm.ApplyResult, error) {
	data, err := cmd.Encode()
	if err != nil {
		return nil, err
	}
	future := s.cfg.Node.Raft.Apply(data, applyTimeout)
	if err := future.Error(); err != nil {
		return nil, err
	}
	result, ok := future.Response().(*fsm.ApplyResult)
	if !ok {
		return nil, fmt.Errorf("unexpected apply response type %T", future.Response())
	}
	return result, nil
}

func (s *Server) writeApplyError(w http.ResponseWriter, err error) {
	if err == raft.ErrNotLeader || err == raft.ErrLeadershipLost {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, err.Error(), s.leaderHint())
		return
	}
	log.Printf("apply error: %v", err)
	writeError(w, http.StatusInternalServerError, ledgertypes.ErrInternal, err.Error(), "")
}

func statusForErrCode(code ledgertypes.ErrorCode) int {
	switch code {
	case ledgertypes.ErrAccountNotFound:
		return http.StatusNotFound
	case ledgertypes.ErrAccountExists:
		return http.StatusConflict
	case ledgertypes.ErrInsufficientFunds:
		return http.StatusUnprocessableEntity
	case ledgertypes.ErrInvalidRequest:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code ledgertypes.ErrorCode, msg, leaderHint string) {
	writeJSON(w, status, ledgertypes.ErrorBody{Error: ledgertypes.ErrorDetail{
		Code: code, Message: msg, LeaderHint: leaderHint,
	}})
}
