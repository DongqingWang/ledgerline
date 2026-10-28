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

// Kind enumerates the known LedgerEvent variants.
type Kind string

const (
	KindAccountCreated  Kind = "account_created"
	KindTransferApplied Kind = "transfer_applied"
)

// LedgerEvent mirrors a single line of the ledger engine's
// GET /v1/events/stream ndjson response. Fields irrelevant to a given Kind
// are simply absent (zero value) on the wire, matching the engine's
// `omitempty` encoding.
type LedgerEvent struct {
	RaftIndex int64     `json:"raft_index"`
	Kind      Kind      `json:"kind"`
	Timestamp time.Time `json:"timestamp"`

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

// HealthzResponse mirrors the ledger engine's GET /healthz response.
type HealthzResponse struct {
	State string `json:"state"`
}
