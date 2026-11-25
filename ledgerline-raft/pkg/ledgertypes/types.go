// Package ledgertypes defines the wire types shared between the ledgerline-raft
// engine, its HTTP API, and the client library. Other Ledgerline repos
// (gateway, balance-reader, persistence-sync, orchestrator) depend on this
// package's JSON shapes as the contract for talking to the engine.
package ledgertypes

import "time"

// AccountType classifies an account in the chart of accounts. Empty means
// unclassified (allowed for backward compatibility); the engine treats the
// type as reporting metadata, while balance constraints are governed by the
// Unlimited flag.
type AccountType string

const (
	AccountAsset     AccountType = "asset"
	AccountLiability AccountType = "liability"
	AccountEquity    AccountType = "equity"
	AccountRevenue   AccountType = "revenue"
	AccountExpense   AccountType = "expense"
)

// ValidAccountType reports whether t is a recognized classification (or empty).
func ValidAccountType(t AccountType) bool {
	switch t {
	case "", AccountAsset, AccountLiability, AccountEquity, AccountRevenue, AccountExpense:
		return true
	}
	return false
}

// Account is the current balance state for a single account.
//
// BalanceMinor is the posted balance. PendingDebitsMinor is the sum of active
// two-phase holds against the account. AvailableMinor = BalanceMinor -
// PendingDebitsMinor is what a new debit (transfer, journal, or hold) may draw
// against; the engine enforces debits against the available balance, never the
// posted balance alone.
type Account struct {
	AccountID          string      `json:"account_id"`
	Currency           string      `json:"currency"`
	BalanceMinor       int64       `json:"balance_minor"`
	PendingDebitsMinor int64       `json:"pending_debits_minor"`
	AvailableMinor     int64       `json:"available_minor"`
	Unlimited          bool        `json:"unlimited,omitempty"`
	Type               AccountType `json:"type,omitempty"`
	TenantID           string      `json:"tenant_id,omitempty"`
}

// CreateAccountRequest creates a new zero-balance account. Unlimited marks a
// clearing/external-funding account (e.g. "EXTERNAL_DEPOSIT") that is
// allowed to go negative — the standard double-entry way to let money enter
// or leave the ledger from outside the system, rather than a special-cased
// "mint" bypass. Ordinary accounts should never set this.
//
// TenantID is deliberately absent: tenant ownership is assigned server-side
// from the authenticated caller, never trusted from the request body.
type CreateAccountRequest struct {
	AccountID string      `json:"account_id"`
	Currency  string      `json:"currency"`
	Unlimited bool        `json:"unlimited,omitempty"`
	Type      AccountType `json:"type,omitempty"`
}

// Direction is which side of a double-entry journal line a leg belongs to.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// ValidDirection reports whether d is a recognized journal direction.
func ValidDirection(d Direction) bool { return d == Debit || d == Credit }

// JournalLine is one leg of a multi-leg journal. Each leg carries its own GL
// dimensions, so a fee-split payout's debit leg and credit legs can be
// classified to different GL accounts.
type JournalLine struct {
	AccountID   string    `json:"account_id"`
	Direction   Direction `json:"direction"`
	AmountMinor int64     `json:"amount_minor"`
	GLAccount   string    `json:"gl_account,omitempty"`
	CostCenter  string    `json:"cost_center,omitempty"`
}

// JournalRequest is the AssertJournal command: an idempotent, atomic,
// multi-leg journal. All legs commit in a single raft log entry or none do.
// The engine validates that debits equal credits per currency (a journal may
// span currencies — each currency must balance independently, which is how
// FX conversions post through position accounts), and that every account's
// aggregate net debit fits its available balance.
type JournalRequest struct {
	DedupID string        `json:"dedup_id"`
	Lines   []JournalLine `json:"lines"`
}

// ReverseRequest is the ReverseJournal command: an idempotent reversal
// of a previously applied transfer or journal, identified by its original
// DedupID. The engine posts the exact opposite legs as a new journal linked
// via reversal_of; the original entry is never mutated or deleted. An entry
// can be reversed at most once.
type ReverseRequest struct {
	DedupID         string `json:"dedup_id"`
	OriginalDedupID string `json:"original_dedup_id"`
}

// TransferRequest is the AssertTransfer command: an idempotent, conditional
// move of funds from one account to another. DedupID makes retries safe —
// submitting the same DedupID twice returns the original result rather than
// applying the transfer twice. Submitting the same DedupID with different
// parameters is a conflict error, not a silent return of the original result.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// PendingTransferRequest creates a two-phase hold: the amount is reserved
// against the sender's available balance immediately, but posted balances do
// not move until the hold is posted. The hold expires deterministically after
// TimeoutSeconds, evaluated against the leader-stamped command timestamps in
// the raft log (never against a replica's wall clock).
type PendingTransferRequest struct {
	DedupID        string `json:"dedup_id"`
	FromAccount    string `json:"from_account"`
	ToAccount      string `json:"to_account"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
}

// PendingTransfer is the outcome of a successful hold creation.
type PendingTransfer struct {
	PendingID   string    `json:"pending_id"`
	DedupID     string    `json:"dedup_id"`
	FromAccount string    `json:"from_account"`
	ToAccount   string    `json:"to_account"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// PendingActionRequest posts or voids an existing hold (the hold's ID comes
// from the URL path). DedupID is the idempotency key of the post/void action
// itself, so retrying a post after a timeout never double-posts.
type PendingActionRequest struct {
	DedupID string `json:"dedup_id"`
}

// SettlementKind distinguishes money entering vs leaving the ledger.
type SettlementKind string

const (
	SettlementDeposit    SettlementKind = "deposit"
	SettlementWithdrawal SettlementKind = "withdrawal"
)

// SettlementStatus is the fat-ledger settlement lifecycle, tracked INSIDE the
// FSM so in-flight state is raft-durable (never lost on crash):
//
//	pending -> processing -> posted
//	        \-> voided      \-> voided
//	pending/processing -> expired (deterministic sweep)
//
// pending/processing reserve funds for a withdrawal (PendingDebits) but only
// record intent for a deposit; posted moves posted balance; voided/expired
// release the reservation. A delayed return after posted is handled by the
// ordinary Reverse primitive on the posted entry.
type SettlementStatus string

const (
	SettlementPending    SettlementStatus = "pending"
	SettlementProcessing SettlementStatus = "processing"
	SettlementPosted     SettlementStatus = "posted"
	SettlementVoided     SettlementStatus = "voided"
	SettlementExpired    SettlementStatus = "expired"
)

// CreateSettlementRequest records a settlement intent in the ledger BEFORE any
// external rail action, so the in-flight state is durable.
type CreateSettlementRequest struct {
	DedupID        string         `json:"dedup_id"`
	Kind           SettlementKind `json:"kind"`
	AccountID      string         `json:"account_id"`
	FundingAccount string         `json:"funding_account"`
	AmountMinor    int64          `json:"amount_minor"`
	Currency       string         `json:"currency"`
	TimeoutSeconds int64          `json:"timeout_seconds"`
}

// SettlementActionRequest marks a settlement processing (submitted to rail) or
// applies a rail outcome. SettlementID comes from the URL path; DedupID is the
// action's own idempotency key.
type SettlementActionRequest struct {
	DedupID string `json:"dedup_id"`
	Outcome string `json:"outcome,omitempty"` // "settled" or "failed" (settle-outcome only)
	Reason  string `json:"reason,omitempty"`
}

// Settlement is the wire view of a settlement entry.
type Settlement struct {
	SettlementID   string           `json:"settlement_id"`
	DedupID        string           `json:"dedup_id"`
	Kind           SettlementKind   `json:"kind"`
	AccountID      string           `json:"account_id"`
	FundingAccount string           `json:"funding_account"`
	AmountMinor    int64            `json:"amount_minor"`
	Currency       string           `json:"currency"`
	Status         SettlementStatus `json:"status"`
	ExpiresAt      time.Time        `json:"expires_at"`
	JournalDedupID string           `json:"journal_dedup_id,omitempty"`
}

// TransferStatus is the outcome of an AssertTransfer/AssertJournal call.
type TransferStatus string

const (
	TransferStatusApplied   TransferStatus = "applied"
	TransferStatusDuplicate TransferStatus = "duplicate" // DedupID already processed; returning prior result.
)

// TransferResult is returned from a successful transfer, journal, reversal,
// or pending post/void call.
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
	// ErrConflict: a DedupID was reused with different parameters.
	ErrConflict ErrorCode = "conflict"
	// ErrUnbalancedJournal: journal debits != credits in some currency.
	ErrUnbalancedJournal ErrorCode = "unbalanced_journal"
	// ErrAlreadyReversed: the original entry was reversed before.
	ErrAlreadyReversed ErrorCode = "already_reversed"
	// ErrPendingNotFound: the hold does not exist (posted, voided, or expired).
	ErrPendingNotFound ErrorCode = "pending_not_found"
	// ErrSettlementNotFound: the settlement entry does not exist.
	ErrSettlementNotFound ErrorCode = "settlement_not_found"
	ErrUnauthorized       ErrorCode = "unauthorized"
	ErrForbidden          ErrorCode = "forbidden"
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

// EventKind distinguishes the ledger event types emitted on the stream.
//
// Exactly one event is emitted per committed raft log entry — including
// rejected commands (command_rejected) and idempotent duplicates
// (command_duplicate), which carry no balance effect. This makes the event
// stream 1:1 with log indices, so consumers can detect any delivery gap by
// checking raft_index contiguity.
type EventKind string

const (
	EventAccountCreated   EventKind = "account_created"
	EventTransferApplied  EventKind = "transfer_applied"
	EventJournalApplied   EventKind = "journal_applied"
	EventPendingCreated   EventKind = "pending_created"
	EventPendingPosted    EventKind = "pending_posted"
	EventPendingVoided    EventKind = "pending_voided"
	EventCommandRejected  EventKind = "command_rejected"
	EventCommandDuplicate EventKind = "command_duplicate"

	EventSettlementCreated    EventKind = "settlement_created"
	EventSettlementProcessing EventKind = "settlement_processing"
	EventSettlementPosted     EventKind = "settlement_posted"
	EventSettlementVoided     EventKind = "settlement_voided"
)

// EventJournalLine is one leg of an applied journal as emitted on the event
// stream. Unlike the request-side JournalLine it carries the account's
// currency, so projections never need to look currency up elsewhere.
type EventJournalLine struct {
	AccountID   string    `json:"account_id"`
	Direction   Direction `json:"direction"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	GLAccount   string    `json:"gl_account,omitempty"`
	CostCenter  string    `json:"cost_center,omitempty"`
}

// ExpiredPending describes a hold released by deterministic timeout expiry.
// Expiries ride along on the event of the command whose application triggered
// the sweep (ExpiredPendings field), preserving one-event-per-log-entry.
type ExpiredPending struct {
	PendingID   string `json:"pending_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// ExpiredSettlement describes a settlement released by deterministic timeout
// (rail never responded). Like ExpiredPending, it rides on the event of the
// command whose application triggered the sweep.
type ExpiredSettlement struct {
	SettlementID string         `json:"settlement_id"`
	Kind         SettlementKind `json:"kind"`
	AccountID    string         `json:"account_id"`
	AmountMinor  int64          `json:"amount_minor"`
	Currency     string         `json:"currency"`
}

// LedgerEvent is one entry on the engine's committed-event stream. It is the
// CQRS fan-out tap that ledgerline-balance-reader and ledgerline-persistence-sync
// both subscribe to via GET /v1/events/stream (newline-delimited JSON).
type LedgerEvent struct {
	RaftIndex uint64    `json:"raft_index"`
	Kind      EventKind `json:"kind"`
	Timestamp time.Time `json:"timestamp"`

	// Populated for EventAccountCreated.
	AccountID   string      `json:"account_id,omitempty"`
	Currency    string      `json:"currency,omitempty"`
	AccountType AccountType `json:"account_type,omitempty"`
	TenantID    string      `json:"tenant_id,omitempty"`

	// Populated for EventTransferApplied and pending events. Currency is
	// also set on transfer/pending events so projections never need to look
	// it up from account state.
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`

	// Populated for EventJournalApplied. ReversalOf is the DedupID of the
	// original entry being reversed (empty for ordinary journals).
	Lines      []EventJournalLine `json:"lines,omitempty"`
	ReversalOf string             `json:"reversal_of,omitempty"`

	// Populated for pending events.
	PendingID  string     `json:"pending_id,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	VoidReason string     `json:"void_reason,omitempty"` // "void" or "expired"

	// Populated for EventCommandRejected.
	ErrCode ErrorCode `json:"err_code,omitempty"`
	ErrMsg  string    `json:"err_msg,omitempty"`

	// Holds that expired during this log entry's application, if any.
	// Consumers must release these holds before applying the event body.
	ExpiredPendings []ExpiredPending `json:"expired_pendings,omitempty"`

	// Populated for settlement events.
	SettlementID   string         `json:"settlement_id,omitempty"`
	SettlementKind SettlementKind `json:"settlement_kind,omitempty"`
	FundingAccount string         `json:"funding_account,omitempty"`

	// Settlements expired during this log entry's application, if any.
	// Consumers must release these before applying the event body.
	ExpiredSettlements []ExpiredSettlement `json:"expired_settlements,omitempty"`
}
