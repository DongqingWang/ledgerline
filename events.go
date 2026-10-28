package main

// LedgerEvent mirrors the JSON wire contract emitted by ledgerline-raft's
// GET /v1/events/stream endpoint. It is a hand-written local copy of the
// contract (not an imported type) so this service has no Go module
// dependency on ledgerline-raft.
//
// kind == "account_created" uses: AccountID, Currency.
// kind == "transfer_applied" uses: DedupID, JournalID, FromAccount,
// ToAccount, AmountMinor, GLAccount (optional), CostCenter (optional).
type LedgerEvent struct {
	RaftIndex uint64 `json:"raft_index"`
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp"`

	// account_created fields
	AccountID string `json:"account_id,omitempty"`
	Currency  string `json:"currency,omitempty"`

	// transfer_applied fields
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

const (
	eventKindAccountCreated  = "account_created"
	eventKindTransferApplied = "transfer_applied"
)
