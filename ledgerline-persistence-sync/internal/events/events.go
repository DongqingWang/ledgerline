// Package events defines the local mirror of the ledgerline-raft wire
// contract for committed ledger events.
//
// This is intentionally a hand-maintained copy of the JSON shape documented
// by ledgerline-raft's /v1/events/stream endpoint, not an imported Go type
// from that repository. ledgerline-persistence-sync talks to the ledger
// engine purely over HTTP + JSON so that it can be built and deployed
// independently of ledgerline-raft's source tree.
package events

import "time"

// Kind enumerates the known LedgerEvent variants. The engine emits exactly
// one event per committed raft log entry — including command_rejected /
// command_duplicate (no balance effect) — so raft_index is contiguous and
// consumers can detect lost delivery by checking for gaps.
type Kind string

const (
	KindAccountCreated   Kind = "account_created"
	KindTransferApplied  Kind = "transfer_applied"
	KindJournalApplied   Kind = "journal_applied"
	KindPendingCreated   Kind = "pending_created"
	KindPendingPosted    Kind = "pending_posted"
	KindPendingVoided    Kind = "pending_voided"
	KindCommandRejected  Kind = "command_rejected"
	KindCommandDuplicate Kind = "command_duplicate"

	KindSettlementCreated    Kind = "settlement_created"
	KindSettlementProcessing Kind = "settlement_processing"
	KindSettlementPosted     Kind = "settlement_posted"
	KindSettlementVoided     Kind = "settlement_voided"
)

// JournalLine is one leg of an applied journal as emitted on the wire. It
// carries the account's currency so projections never need to look it up.
type JournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// ExpiredPending describes a two-phase hold released by deterministic
// timeout expiry. Expiries ride on the event of the command whose
// application triggered the sweep and must be applied before the body.
type ExpiredPending struct {
	PendingID   string `json:"pending_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// ExpiredSettlement describes an in-flight settlement released by deterministic
// timeout. Like ExpiredPending it rides on the triggering event, applied first.
type ExpiredSettlement struct {
	SettlementID string `json:"settlement_id"`
	Kind         string `json:"kind"`
	AccountID    string `json:"account_id"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
}

// LedgerEvent mirrors a single line of the ledger engine's
// GET /v1/events/stream ndjson response. Fields irrelevant to a given Kind
// are simply absent (zero value) on the wire, matching the engine's
// `omitempty` encoding.
type LedgerEvent struct {
	RaftIndex int64     `json:"raft_index"`
	Kind      Kind      `json:"kind"`
	Timestamp time.Time `json:"timestamp"`

	// account_created fields
	AccountID   string `json:"account_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`

	// transfer_applied / pending_* fields. Currency is now carried on the
	// wire for transfers and pending events too.
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`

	// journal_applied fields
	Lines      []JournalLine `json:"lines,omitempty"`
	ReversalOf string        `json:"reversal_of,omitempty"`

	// pending_* fields
	PendingID  string     `json:"pending_id,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	VoidReason string     `json:"void_reason,omitempty"`

	// command_rejected fields
	ErrCode string `json:"err_code,omitempty"`
	ErrMsg  string `json:"err_msg,omitempty"`

	ExpiredPendings []ExpiredPending `json:"expired_pendings,omitempty"`

	// settlement_* fields
	SettlementID   string `json:"settlement_id,omitempty"`
	SettlementKind string `json:"settlement_kind,omitempty"`
	FundingAccount string `json:"funding_account,omitempty"`

	ExpiredSettlements []ExpiredSettlement `json:"expired_settlements,omitempty"`
}

// HealthzResponse mirrors the ledger engine's GET /healthz response.
type HealthzResponse struct {
	State string `json:"state"`
}
