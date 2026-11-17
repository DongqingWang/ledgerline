// Package trialbalance contains the pure, DB-free aggregation and
// reconciliation logic for the trial balance report. Nothing in this file
// touches Postgres or any other I/O; it operates entirely on in-memory
// slices of structs so it can be exhaustively unit tested without a live
// database.
package trialbalance

import "sort"

// UnspecifiedGLAccount is the bucket used for journal rows whose gl_account
// column is NULL (not set) in the source projection.
const UnspecifiedGLAccount = "UNSPECIFIED"

// UnspecifiedCurrency buckets legacy rows written before the projection
// carried currency on every event. It should be empty on a healthy,
// fully-migrated projection.
const UnspecifiedCurrency = "UNSPECIFIED"

// JournalRow is the minimal shape of a journal_entries row needed to build
// a trial balance. It intentionally excludes columns (account_id,
// cost_center, raft_index, etc.) that are not needed for this aggregation.
type JournalRow struct {
	GLAccount   string // already normalized: "" or NULL should be passed as UnspecifiedGLAccount
	Currency    string
	Direction   string // "debit" or "credit"
	AmountMinor int64
}

// GLBalance is one row of the trial balance report: the aggregated debit and
// credit totals (and their net) for a single (currency, GL account) pair.
// Amounts in different currencies are never summed together.
type GLBalance struct {
	Currency         string `json:"currency"`
	GLAccount        string `json:"gl_account"`
	TotalDebitMinor  int64  `json:"total_debit_minor"`
	TotalCreditMinor int64  `json:"total_credit_minor"`
	NetMinor         int64  `json:"net_minor"`
}

// BuildTrialBalance groups the given journal rows by (currency, GL account)
// and sums debits and credits for each group. Rows with an empty GLAccount
// are treated as UnspecifiedGLAccount; empty currencies as
// UnspecifiedCurrency. The result is sorted by currency then GL account for
// stable, deterministic output. Unrecognized directions are ignored
// (defensive; the DB schema enforces a CHECK constraint so this should not
// happen in practice).
func BuildTrialBalance(rows []JournalRow) []GLBalance {
	type key struct{ currency, gl string }
	totals := make(map[key]*GLBalance)

	for _, r := range rows {
		gl := r.GLAccount
		if gl == "" {
			gl = UnspecifiedGLAccount
		}
		ccy := r.Currency
		if ccy == "" {
			ccy = UnspecifiedCurrency
		}
		k := key{ccy, gl}

		b, ok := totals[k]
		if !ok {
			b = &GLBalance{Currency: ccy, GLAccount: gl}
			totals[k] = b
		}

		switch r.Direction {
		case "debit":
			b.TotalDebitMinor += r.AmountMinor
		case "credit":
			b.TotalCreditMinor += r.AmountMinor
		}
	}

	out := make([]GLBalance, 0, len(totals))
	for _, b := range totals {
		b.NetMinor = b.TotalDebitMinor - b.TotalCreditMinor
		out = append(out, *b)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].GLAccount < out[j].GLAccount
	})

	return out
}

// CurrencyReconciliation is the double-entry check result for one currency.
type CurrencyReconciliation struct {
	Currency string `json:"currency"`
	OK       bool   `json:"ok"`
	Debits   int64  `json:"debits"`
	Credits  int64  `json:"credits"`
}

// ReconcileByCurrency checks the double-entry invariant independently per
// currency: within each currency, total debits must equal total credits.
// Summing across currencies is meaningless (100 USD + 100 EUR != 200 of
// anything), which is exactly the bug a single global check would hide.
// Results are sorted by currency.
func ReconcileByCurrency(balances []GLBalance) []CurrencyReconciliation {
	totals := make(map[string]*CurrencyReconciliation)
	for _, b := range balances {
		r, ok := totals[b.Currency]
		if !ok {
			r = &CurrencyReconciliation{Currency: b.Currency}
			totals[b.Currency] = r
		}
		r.Debits += b.TotalDebitMinor
		r.Credits += b.TotalCreditMinor
	}
	out := make([]CurrencyReconciliation, 0, len(totals))
	for _, r := range totals {
		r.OK = r.Debits == r.Credits
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Currency < out[j].Currency })
	return out
}
