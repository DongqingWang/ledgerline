package accounting

import "fmt"

// ComposeJournal classifies a transfer via the rule set and builds a
// balanced journal from it. This is the single choke point every transfer
// must pass through before being submitted to the ledger engine: nothing
// posts without first being correctly classified.
func (rs *RuleSet) ComposeJournal(ctx TransferContext) (Journal, error) {
	if ctx.AmountMinor <= 0 {
		return Journal{}, fmt.Errorf("amount_minor must be positive")
	}
	if ctx.FeeMinor < 0 || ctx.FeeMinor > ctx.AmountMinor {
		return Journal{}, fmt.Errorf("fee_minor must be between 0 and amount_minor")
	}

	rule, err := rs.Resolve(ctx)
	if err != nil {
		return Journal{}, err
	}

	netToReceiver := ctx.AmountMinor - ctx.FeeMinor

	j := Journal{Lines: []JournalLine{
		{
			AccountID:   ctx.FromAccount,
			Direction:   Debit,
			AmountMinor: ctx.AmountMinor,
			GLAccount:   rule.Debit.GLAccount,
			CostCenter:  rule.Debit.CostCenter,
			TaxCode:     rule.Debit.TaxCode,
		},
		{
			AccountID:   ctx.ToAccount,
			Direction:   Credit,
			AmountMinor: netToReceiver,
			GLAccount:   rule.Credit.GLAccount,
			CostCenter:  rule.Credit.CostCenter,
			TaxCode:     rule.Credit.TaxCode,
		},
	}}

	if ctx.FeeMinor > 0 {
		feeDims := rule.FeeCredit
		if feeDims.GLAccount == "" {
			return Journal{}, fmt.Errorf("rule for category %q has no fee_credit dimensions but fee_minor=%d was supplied", ctx.Category, ctx.FeeMinor)
		}
		j.Lines = append(j.Lines, JournalLine{
			AccountID:   "platform-fee-revenue",
			Direction:   Credit,
			AmountMinor: ctx.FeeMinor,
			GLAccount:   feeDims.GLAccount,
			CostCenter:  feeDims.CostCenter,
			TaxCode:     feeDims.TaxCode,
		})
	}

	if err := Validate(j); err != nil {
		// A rule that produces an unbalanced journal is a configuration bug,
		// not a transient failure — fail loudly rather than posting bad data.
		return Journal{}, fmt.Errorf("composed journal failed validation: %w", err)
	}

	return j, nil
}

// Validate enforces the one invariant that can never be relaxed: total
// debits must equal total credits. Exported standalone (not just called
// from ComposeJournal) so callers can re-validate a journal that arrived
// from elsewhere — e.g. ledgerline-persistence-sync re-checking what it
// read back from Postgres during reconciliation.
func Validate(j Journal) error {
	debits := j.TotalByDirection(Debit)
	credits := j.TotalByDirection(Credit)
	if debits != credits {
		return fmt.Errorf("unbalanced journal: debits=%d credits=%d", debits, credits)
	}
	if len(j.Lines) < 2 {
		return fmt.Errorf("a journal must have at least one debit and one credit line, got %d lines", len(j.Lines))
	}
	return nil
}
