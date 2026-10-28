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

// JournalRow is the minimal shape of a journal_entries row needed to build
// a trial balance. It intentionally excludes columns (account_id, currency,
// cost_center, raft_index, etc.) that are not needed for this aggregation.
type JournalRow struct {
	GLAccount   string // already normalized: "" or NULL should be passed as UnspecifiedGLAccount
	Direction   string // "debit" or "credit"
	AmountMinor int64
}

// GLBalance is one row of the trial balance report: the aggregated debit and
// credit totals (and their net) for a single GL account.
type GLBalance struct {
	GLAccount        string `json:"gl_account"`
	TotalDebitMinor  int64  `json:"total_debit_minor"`
	TotalCreditMinor int64  `json:"total_credit_minor"`
	NetMinor         int64  `json:"net_minor"`
}

// BuildTrialBalance groups the given journal rows by GL account and sums
// debits and credits for each group. Rows with an empty GLAccount are
// treated as UnspecifiedGLAccount. The result is sorted by GL account name
// for stable, deterministic output. Unrecognized directions are ignored
// (defensive; the DB schema enforces a CHECK constraint so this should not
// happen in practice).
func BuildTrialBalance(rows []JournalRow) []GLBalance {
	totals := make(map[string]*GLBalance)
	order := make([]string, 0)

	for _, r := range rows {
		gl := r.GLAccount
		if gl == "" {
			gl = UnspecifiedGLAccount
		}

		b, ok := totals[gl]
		if !ok {
			b = &GLBalance{GLAccount: gl}
			totals[gl] = b
			order = append(order, gl)
		}

		switch r.Direction {
		case "debit":
			b.TotalDebitMinor += r.AmountMinor
		case "credit":
			b.TotalCreditMinor += r.AmountMinor
		}
	}

	out := make([]GLBalance, 0, len(order))
	for _, gl := range order {
		b := totals[gl]
		b.NetMinor = b.TotalDebitMinor - b.TotalCreditMinor
		out = append(out, *b)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].GLAccount < out[j].GLAccount })

	return out
}

// Reconcile checks the global double-entry invariant: the sum of all debits
// across every GL account must equal the sum of all credits. It returns
// whether the books balance, plus the total debits and credits so callers
// can report a precise diff on failure.
func Reconcile(balances []GLBalance) (ok bool, debits int64, credits int64) {
	for _, b := range balances {
		debits += b.TotalDebitMinor
		credits += b.TotalCreditMinor
	}
	return debits == credits, debits, credits
}
