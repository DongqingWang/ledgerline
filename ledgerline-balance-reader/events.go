package main

// LedgerEvent mirrors the JSON wire contract emitted by ledgerline-raft's
// GET /v1/events/stream endpoint. It is a hand-written local copy of the
// contract (not an imported type) so this service has no Go module
// dependency on ledgerline-raft.
//
// The engine emits exactly one event per committed raft log entry —
// including command_rejected / command_duplicate, which carry no balance
// effect — so raft_index is contiguous and any gap means lost delivery.
type LedgerEvent struct {
	RaftIndex uint64 `json:"raft_index"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp"`

	// account_created fields
	AccountID   string `json:"account_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`

	// transfer_applied / pending_* fields
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`

	// journal_applied fields
	Lines      []EventJournalLine `json:"lines,omitempty"`
	ReversalOf string             `json:"reversal_of,omitempty"`

	// pending_* fields
	PendingID  string `json:"pending_id,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	VoidReason string `json:"void_reason,omitempty"`

	// command_rejected fields
	ErrCode string `json:"err_code,omitempty"`
	ErrMsg  string `json:"err_msg,omitempty"`

	// Holds released by deterministic expiry during this entry's
	// application. Must be applied BEFORE the event body.
	ExpiredPendings []ExpiredPending `json:"expired_pendings,omitempty"`
}

// EventJournalLine is one leg of an applied journal; it carries the
// account's currency so the projection never needs to look it up.
type EventJournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

type ExpiredPending struct {
	PendingID   string `json:"pending_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

const (
	eventKindAccountCreated   = "account_created"
	eventKindTransferApplied  = "transfer_applied"
	eventKindJournalApplied   = "journal_applied"
	eventKindPendingCreated   = "pending_created"
	eventKindPendingPosted    = "pending_posted"
	eventKindPendingVoided    = "pending_voided"
	eventKindCommandRejected  = "command_rejected"
	eventKindCommandDuplicate = "command_duplicate"
)
