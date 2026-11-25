// Package acquiring implements the card-acquiring domain: it processes a
// payment (auth -> capture) against a simulated card scheme and records the
// resulting double-entry journal in the ledger. The ledger remains the single
// source of fund truth; this service owns the payment-processing lifecycle.
//
// Dual-write protection: a durable intent is recorded BEFORE any scheme
// action, and the journal write to the ledger is idempotent (dedup_id) and
// retried, so "captured but unrecorded" never silently loses money.
package acquiring

import "time"

// Status is the PaymentIntent lifecycle (P1 stops at captured; clearing and
// settled are later phases).
//
//	pending -> authorized -> captured
//	        \-> failed     \-> failed
//	pending/authorized -> voided
type Status string

const (
	StatusPending    Status = "pending"    // durable intent recorded, pre-scheme
	StatusAuthorized Status = "authorized" // scheme auth succeeded
	StatusCaptured   Status = "captured"   // capture succeeded + journal recorded
	StatusFailed     Status = "failed"
	StatusVoided     Status = "voided"
)

// PaymentIntent is one card payment and its lifecycle state.
type PaymentIntent struct {
	ID             string    `json:"id"`
	DedupID        string    `json:"dedup_id"`
	MerchantID     string    `json:"merchant_id"`
	AmountMinor    int64     `json:"amount_minor"`
	FeeMinor       int64     `json:"fee_minor"`
	Currency       string    `json:"currency"`
	Status         Status    `json:"status"`
	SchemeRef      string    `json:"scheme_ref,omitempty"`
	JournalDedupID string    `json:"journal_dedup_id,omitempty"`
	FailureReason  string    `json:"failure_reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

var validTransitions = map[Status][]Status{
	StatusPending:    {StatusAuthorized, StatusFailed, StatusVoided},
	StatusAuthorized: {StatusCaptured, StatusFailed, StatusVoided},
	StatusCaptured:   {StatusVoided}, // refund: reverse the journal
	StatusFailed:     {},
	StatusVoided:     {},
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to Status) bool {
	for _, a := range validTransitions[from] {
		if a == to {
			return true
		}
	}
	return false
}
