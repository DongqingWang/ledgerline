// Package recon matches the ledger's recorded journals against external
// authoritative records (card-scheme clearing files, bank statements) and
// surfaces breaks. Reconciliation is the backstop that catches anything the
// idempotent-retry path missed: money that moved externally but was never
// recorded, or recorded but never moved.
//
// The matching engine is pure and deterministic (sorted output) so it can be
// exhaustively unit tested.
package recon

import "sort"

// Record is one side's view of a money movement, keyed by a shared reference.
type Record struct {
	Ref         string `json:"ref"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// BreakType classifies a reconciliation difference.
type BreakType string

const (
	BreakMissingInLedger   BreakType = "missing_in_ledger"   // external has it, ledger doesn't -> 补记
	BreakMissingInExternal BreakType = "missing_in_external" // ledger has it, external doesn't -> investigate/void
	BreakAmountMismatch    BreakType = "amount_mismatch"     // both have it, amounts differ -> adjust
	BreakDuplicate         BreakType = "duplicate"           // same ref appears twice on one side
)

// Break is one reconciliation difference.
type Break struct {
	Type           BreakType `json:"type"`
	Ref            string    `json:"ref"`
	LedgerAmount   int64     `json:"ledger_amount,omitempty"`
	ExternalAmount int64     `json:"external_amount,omitempty"`
	Currency       string    `json:"currency,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

// Reconcile matches ledger records against external records by Ref and returns
// every break, sorted deterministically by (type, ref).
func Reconcile(ledger, external []Record) []Break {
	var breaks []Break

	ledgerMap, ledgerDups := index(ledger)
	externalMap, externalDups := index(external)
	for _, ref := range ledgerDups {
		breaks = append(breaks, Break{Type: BreakDuplicate, Ref: ref, Detail: "ledger"})
	}
	for _, ref := range externalDups {
		breaks = append(breaks, Break{Type: BreakDuplicate, Ref: ref, Detail: "external"})
	}

	// External records: present in ledger? amounts match?
	for _, e := range external {
		l, ok := ledgerMap[e.Ref]
		if !ok {
			breaks = append(breaks, Break{Type: BreakMissingInLedger, Ref: e.Ref, ExternalAmount: e.AmountMinor, Currency: e.Currency})
			continue
		}
		if l.AmountMinor != e.AmountMinor || l.Currency != e.Currency {
			breaks = append(breaks, Break{Type: BreakAmountMismatch, Ref: e.Ref, LedgerAmount: l.AmountMinor, ExternalAmount: e.AmountMinor, Currency: e.Currency})
		}
	}
	// Ledger records with no external counterpart.
	for _, l := range ledger {
		if _, ok := externalMap[l.Ref]; !ok {
			breaks = append(breaks, Break{Type: BreakMissingInExternal, Ref: l.Ref, LedgerAmount: l.AmountMinor, Currency: l.Currency})
		}
	}

	sort.Slice(breaks, func(i, j int) bool {
		if breaks[i].Type != breaks[j].Type {
			return breaks[i].Type < breaks[j].Type
		}
		return breaks[i].Ref < breaks[j].Ref
	})
	return breaks
}

// index builds a ref->record map and reports duplicate refs.
func index(records []Record) (map[string]Record, []string) {
	m := make(map[string]Record, len(records))
	seen := make(map[string]bool)
	var dups []string
	for _, r := range records {
		if _, ok := m[r.Ref]; ok && !seen[r.Ref] {
			seen[r.Ref] = true
			dups = append(dups, r.Ref)
		}
		m[r.Ref] = r
	}
	sort.Strings(dups)
	return m, dups
}
