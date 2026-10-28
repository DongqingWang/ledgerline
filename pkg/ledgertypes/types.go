// Package ledgertypes defines the wire types shared between the ledgerline-raft
// engine, its HTTP API, and the client library. Other Ledgerline repos
// (gateway, balance-reader, persistence-sync, orchestrator) depend on this
// package's JSON shapes as the contract for talking to the engine.
package ledgertypes

import "time"

// Account is the current balance state for a single account.
type Account struct {
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Unlimited    bool   `json:"unlimited,omitempty"`
}

// CreateAccountRequest creates a new zero-balance account. Unlimited marks a
// clearing/external-funding account (e.g. "EXTERNAL_DEPOSIT") that is
// allowed to go negative — the standard double-entry way to let money enter
// or leave the ledger from outside the system, rather than a special-cased
// "mint" bypass. Ordinary accounts should never set this.
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited,omitempty"`
}

// TransferRequest is the AssertTransfer command: an idempotent, conditional
// move of funds from one account to another. DedupID makes retries safe —
// submitting the same DedupID twice returns the original result rather than
// applying the transfer twice.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// TransferStatus is the outcome of an AssertTransfer call.
type TransferStatus string

const (
	TransferStatusApplied   TransferStatus = "applied"
	TransferStatusDuplicate TransferStatus = "duplicate" // DedupID already processed; returning prior result.
)

// TransferResult is returned from a successful AssertTransfer call.
type TransferResult struct {
	Status    TransferStatus `json:"status"`
	JournalID string         `json:"journal_id"`
	RaftIndex uint64         `json:"raft_index"`
}

// ErrorCode enumerates the stable error codes the engine's HTTP API returns.
type ErrorCode string

const (
	ErrNotLeader         ErrorCode = "not_leader"
	ErrAccountNotFound   ErrorCode = "account_not_found"
	ErrAccountExists     ErrorCode = "account_exists"
	ErrInsufficientFunds ErrorCode = "insufficient_funds"
	ErrInvalidRequest    ErrorCode = "invalid_request"
	ErrInternal          ErrorCode = "internal"
)

// ErrorBody is the JSON error envelope every Ledgerline HTTP service returns.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code       ErrorCode `json:"code"`
	Message    string    `json:"message"`
	LeaderHint string    `json:"leader_hint,omitempty"`
}

// LeaderInfo reports the engine's current known raft leader, letting clients
// short-circuit retries instead of round-robining blindly.
type LeaderInfo struct {
	LeaderID       string `json:"leader_id"`
	LeaderHTTPAddr string `json:"leader_http_addr"`
}

// EventKind distinguishes the two ledger event types emitted on the stream.
type EventKind string

const (
	EventAccountCreated  EventKind = "account_created"
	EventTransferApplied EventKind = "transfer_applied"
)

// LedgerEvent is one entry on the engine's committed-event stream. It is the
// CQRS fan-out tap that ledgerline-balance-reader and ledgerline-persistence-sync
// both subscribe to via GET /v1/events/stream (newline-delimited JSON).
type LedgerEvent struct {
	RaftIndex uint64    `json:"raft_index"`
	Kind      EventKind `json:"kind"`
	Timestamp time.Time `json:"timestamp"`

	// Populated for EventAccountCreated.
	AccountID string `json:"account_id,omitempty"`
	Currency  string `json:"currency,omitempty"`

	// Populated for EventTransferApplied.
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}
