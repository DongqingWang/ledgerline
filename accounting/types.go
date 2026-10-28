// Package accounting is the double-entry bookkeeping / GL-dimension rule
// engine — the Ledgerline equivalent of fascore + fasneuronsvc. It sits in
// the critical path of every transfer: given a transfer's business context,
// it decides which GL account and cost center each leg of the journal
// belongs to, and it is the single place that guarantees debits == credits
// before anything is submitted to the ledger engine.
package accounting

// Direction is which side of a double-entry journal line a leg belongs to.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// TransferContext is everything the rule engine needs to classify a
// transfer: who it's between, how much, and enough business context
// (Category, Country) to pick the right GL dimensions. FeeMinor is optional
// — when set, the composed journal splits the credit leg into a net payout
// to ToAccount and a separate fee line, demonstrating that a single transfer
// can still produce more than two journal lines while remaining balanced.
type TransferContext struct {
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor,omitempty"`
	Currency    string `json:"currency"`
	Category    string `json:"category"` // e.g. "seller_payout", "refund", "merchant_fee"
	Country     string `json:"country"`  // e.g. "US", "DE" — "*" rules match any country
}

// JournalLine is one leg of a double-entry journal entry.
type JournalLine struct {
	AccountID   string    `json:"account_id"`
	Direction   Direction `json:"direction"`
	AmountMinor int64     `json:"amount_minor"`
	GLAccount   string    `json:"gl_account"`
	CostCenter  string    `json:"cost_center"`
	TaxCode     string    `json:"tax_code,omitempty"`
}

// Journal is a complete, composed set of balanced journal lines for one
// transfer, ready to submit to ledgerline-raft's AssertTransfer-shaped API
// (the gateway maps Lines[0]/Lines[1] onto from_account/to_account/gl_account
// for the primary leg, and records any additional lines, e.g. a fee split,
// in its own ledger-side accounting metadata).
type Journal struct {
	Lines []JournalLine `json:"lines"`
}

// TotalByDirection sums all lines on one side of the journal.
func (j Journal) TotalByDirection(d Direction) int64 {
	var total int64
	for _, l := range j.Lines {
		if l.Direction == d {
			total += l.AmountMinor
		}
	}
	return total
}
