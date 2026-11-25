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
//
// Auth, when non-nil, enables API-key authentication and tenant isolation.
// nil (the demo default) leaves the API open, as before.
type Config struct {
	Node          *raftnode.Node
	PeerHTTPAddrs map[string]string
	Auth          *AuthConfig
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
	mux.HandleFunc("POST /v1/journals", s.handleAssertJournal)
	mux.HandleFunc("POST /v1/reversals", s.handleReverse)
	mux.HandleFunc("POST /v1/pending-transfers", s.handleCreatePending)
	mux.HandleFunc("POST /v1/pending-transfers/{id}/post", s.handlePostPending)
	mux.HandleFunc("POST /v1/pending-transfers/{id}/void", s.handleVoidPending)
	mux.HandleFunc("POST /v1/settlements", s.handleCreateSettlement)
	mux.HandleFunc("GET /v1/settlements/{id}", s.handleGetSettlement)
	mux.HandleFunc("POST /v1/settlements/{id}/mark-processing", s.handleMarkSettlementProcessing)
	mux.HandleFunc("POST /v1/settlements/{id}/settle-outcome", s.handleSettleOutcome)
	mux.HandleFunc("GET /v1/leader", s.handleGetLeader)
	mux.HandleFunc("GET /v1/events/stream", s.handleEventStream)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.withAuth(mux)
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

	cmd := fsm.Command{Type: fsm.CmdCreateAccount, CreateAccount: &req, TenantID: tenantFromContext(r.Context())}
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
	if !ok || !s.canReadAccount(r, account) {
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
	if !s.checkAccountsOwned(w, r, req.FromAccount, req.ToAccount) {
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

func (s *Server) handleAssertJournal(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	var req ledgertypes.JournalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" || len(req.Lines) < 2 {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "dedup_id and at least two lines are required", "")
		return
	}
	ids := make([]string, 0, len(req.Lines))
	for _, ln := range req.Lines {
		ids = append(ids, ln.AccountID)
	}
	if !s.checkAccountsOwned(w, r, ids...) {
		return
	}

	cmd := fsm.Command{Type: fsm.CmdAssertJournal, AssertJournal: &req}
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

func (s *Server) handleReverse(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	var req ledgertypes.ReverseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" || req.OriginalDedupID == "" {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "dedup_id and original_dedup_id are required", "")
		return
	}
	if lines, ok := s.cfg.Node.FSM.OriginalLines(req.OriginalDedupID); ok {
		ids := make([]string, 0, len(lines))
		for _, ln := range lines {
			ids = append(ids, ln.AccountID)
		}
		if !s.checkAccountsOwned(w, r, ids...) {
			return
		}
	}

	cmd := fsm.Command{Type: fsm.CmdReverse, Reverse: &req}
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

func (s *Server) handleCreatePending(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	var req ledgertypes.PendingTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" || req.FromAccount == "" || req.ToAccount == "" || req.AmountMinor <= 0 || req.TimeoutSeconds <= 0 {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest,
			"dedup_id, from_account, to_account are required; amount_minor and timeout_seconds must be positive", "")
		return
	}
	if !s.checkAccountsOwned(w, r, req.FromAccount, req.ToAccount) {
		return
	}

	cmd := fsm.Command{Type: fsm.CmdCreatePending, CreatePending: &req}
	result, err := s.apply(cmd)
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	if result.IsError() {
		writeError(w, statusForErrCode(result.ErrCode), result.ErrCode, result.ErrMsg, "")
		return
	}
	writeJSON(w, http.StatusCreated, result.Pending)
}

func (s *Server) handlePostPending(w http.ResponseWriter, r *http.Request) {
	s.handlePendingAction(w, r, fsm.CmdPostPending)
}

func (s *Server) handleVoidPending(w http.ResponseWriter, r *http.Request) {
	s.handlePendingAction(w, r, fsm.CmdVoidPending)
}

func (s *Server) handlePendingAction(w http.ResponseWriter, r *http.Request, cmdType fsm.CommandType) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}

	pendingID := r.PathValue("id")
	var req ledgertypes.PendingActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "dedup_id is required", "")
		return
	}
	if p, ok := s.cfg.Node.FSM.Pending(pendingID); ok {
		if !s.checkAccountsOwned(w, r, p.FromAccount, p.ToAccount) {
			return
		}
	}

	cmd := fsm.Command{Type: cmdType, PendingAction: &req, PendingID: pendingID}
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

func (s *Server) handleCreateSettlement(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}
	var req ledgertypes.CreateSettlementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" || req.AccountID == "" || req.FundingAccount == "" || req.AmountMinor <= 0 || req.TimeoutSeconds <= 0 {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest,
			"dedup_id, account_id, funding_account required; amount_minor and timeout_seconds must be positive", "")
		return
	}
	if !s.checkAccountsOwned(w, r, req.AccountID, req.FundingAccount) {
		return
	}
	cmd := fsm.Command{Type: fsm.CmdCreateSettlement, CreateSettlement: &req}
	result, err := s.apply(cmd)
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	if result.IsError() {
		writeError(w, statusForErrCode(result.ErrCode), result.ErrCode, result.ErrMsg, "")
		return
	}
	writeJSON(w, http.StatusCreated, result.Settlement)
}

func (s *Server) handleGetSettlement(w http.ResponseWriter, r *http.Request) {
	stl, ok := s.cfg.Node.FSM.Settlement(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, ledgertypes.ErrSettlementNotFound, "settlement not found", "")
		return
	}
	if a, ok := s.cfg.Node.FSM.Account(stl.AccountID); ok && !s.canReadAccount(r, a) {
		writeError(w, http.StatusNotFound, ledgertypes.ErrSettlementNotFound, "settlement not found", "")
		return
	}
	writeJSON(w, http.StatusOK, stl)
}

func (s *Server) handleMarkSettlementProcessing(w http.ResponseWriter, r *http.Request) {
	s.handleSettlementAction(w, r, fsm.CmdMarkSettlementProcessing)
}

func (s *Server) handleSettleOutcome(w http.ResponseWriter, r *http.Request) {
	s.handleSettlementAction(w, r, fsm.CmdSettleOutcome)
}

func (s *Server) handleSettlementAction(w http.ResponseWriter, r *http.Request, cmdType fsm.CommandType) {
	if s.cfg.Node.Raft.State() != raft.Leader {
		writeError(w, http.StatusConflict, ledgertypes.ErrNotLeader, "this node is not the raft leader", s.leaderHint())
		return
	}
	settlementID := r.PathValue("id")
	var req ledgertypes.SettlementActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, err.Error(), "")
		return
	}
	if req.DedupID == "" {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "dedup_id is required", "")
		return
	}
	if cmdType == fsm.CmdSettleOutcome && req.Outcome != "settled" && req.Outcome != "failed" {
		writeError(w, http.StatusBadRequest, ledgertypes.ErrInvalidRequest, "outcome must be settled or failed", "")
		return
	}
	if stl, ok := s.cfg.Node.FSM.Settlement(settlementID); ok {
		if !s.checkAccountsOwned(w, r, stl.AccountID, stl.FundingAccount) {
			return
		}
	}
	cmd := fsm.Command{Type: cmdType, SettlementAction: &req, SettlementID: settlementID}
	result, err := s.apply(cmd)
	if err != nil {
		s.writeApplyError(w, err)
		return
	}
	if result.IsError() {
		writeError(w, statusForErrCode(result.ErrCode), result.ErrCode, result.ErrMsg, "")
		return
	}
	writeJSON(w, http.StatusOK, result.Settlement)
}

// checkAccountsOwned enforces tenant isolation for non-admin callers: every
// referenced account must belong to the caller's tenant. Missing accounts
// pass through so the FSM reports account_not_found authoritatively. Admin
// callers (internal services like the gateway) and auth-disabled mode skip
// the check.
func (s *Server) checkAccountsOwned(w http.ResponseWriter, r *http.Request, ids ...string) bool {
	if s.cfg.Auth == nil || isAdminContext(r.Context()) {
		return true
	}
	tenant := tenantFromContext(r.Context())
	for _, id := range ids {
		a, ok := s.cfg.Node.FSM.Account(id)
		if ok && a.TenantID != tenant {
			writeError(w, http.StatusForbidden, ledgertypes.ErrForbidden,
				fmt.Sprintf("account %q belongs to another tenant", id), "")
			return false
		}
	}
	return true
}

func (s *Server) canReadAccount(r *http.Request, a *ledgertypes.Account) bool {
	if s.cfg.Auth == nil || isAdminContext(r.Context()) {
		return true
	}
	return a.TenantID == tenantFromContext(r.Context())
}

// apply submits a command through raft and waits for it to commit and be
// applied on this (the leader's) FSM, returning the typed ApplyResult. The
// leader stamps its wall clock here, at admission time — the timestamp then
// travels inside the log entry, so every replica's Apply sees the identical
// clock value (FSM.Apply itself never calls time.Now).
func (s *Server) apply(cmd fsm.Command) (*fsm.ApplyResult, error) {
	cmd.Timestamp = time.Now().UTC()
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
	case ledgertypes.ErrAccountNotFound, ledgertypes.ErrPendingNotFound, ledgertypes.ErrSettlementNotFound:
		return http.StatusNotFound
	case ledgertypes.ErrAccountExists, ledgertypes.ErrConflict, ledgertypes.ErrAlreadyReversed:
		return http.StatusConflict
	case ledgertypes.ErrInsufficientFunds, ledgertypes.ErrUnbalancedJournal:
		return http.StatusUnprocessableEntity
	case ledgertypes.ErrInvalidRequest:
		return http.StatusBadRequest
	case ledgertypes.ErrUnauthorized:
		return http.StatusUnauthorized
	case ledgertypes.ErrForbidden:
		return http.StatusForbidden
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
