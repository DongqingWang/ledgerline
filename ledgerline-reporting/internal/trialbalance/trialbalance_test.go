package trialbalance

import "testing"

func TestBuildTrialBalance_MultipleGLAccounts(t *testing.T) {
	rows := []JournalRow{
		{GLAccount: "1000", Currency: "USD", Direction: "debit", AmountMinor: 100000},
		{GLAccount: "1000", Currency: "USD", Direction: "credit", AmountMinor: 100000},
		{GLAccount: "4000", Currency: "USD", Direction: "debit", AmountMinor: 1500},
		{GLAccount: "4000", Currency: "USD", Direction: "debit", AmountMinor: 1250},
		{GLAccount: "4000", Currency: "USD", Direction: "credit", AmountMinor: 2750},
	}

	got := BuildTrialBalance(rows)

	want := []GLBalance{
		{Currency: "USD", GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000, NetMinor: 0},
		{Currency: "USD", GLAccount: "4000", TotalDebitMinor: 2750, TotalCreditMinor: 2750, NetMinor: 0},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d balances, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("balance[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestBuildTrialBalance_GroupsPerCurrency(t *testing.T) {
	rows := []JournalRow{
		{GLAccount: "1000", Currency: "USD", Direction: "debit", AmountMinor: 100},
		{GLAccount: "1000", Currency: "USD", Direction: "credit", AmountMinor: 100},
		{GLAccount: "1000", Currency: "EUR", Direction: "debit", AmountMinor: 90},
		{GLAccount: "1000", Currency: "EUR", Direction: "credit", AmountMinor: 90},
	}
	got := BuildTrialBalance(rows)
	if len(got) != 2 {
		t.Fatalf("same GL in two currencies must produce two rows, got %+v", got)
	}
	if got[0].Currency != "EUR" || got[1].Currency != "USD" {
		t.Fatalf("expected currency-sorted output, got %+v", got)
	}
}

func TestBuildTrialBalance_NullGLAccountBucketsAsUnspecified(t *testing.T) {
	rows := []JournalRow{
		{GLAccount: "", Currency: "USD", Direction: "debit", AmountMinor: 300},
		{GLAccount: "", Currency: "USD", Direction: "credit", AmountMinor: 300},
		{GLAccount: "1000", Currency: "USD", Direction: "debit", AmountMinor: 500},
		{GLAccount: "1000", Currency: "USD", Direction: "credit", AmountMinor: 500},
	}

	got := BuildTrialBalance(rows)

	if len(got) != 2 {
		t.Fatalf("got %d balances, want 2: %+v", len(got), got)
	}

	// Sorted order: "1000" < "UNSPECIFIED" lexically? '1' (0x31) < 'U' (0x55), so "1000" comes first.
	if got[0].GLAccount != "1000" {
		t.Errorf("got[0].GLAccount = %q, want %q", got[0].GLAccount, "1000")
	}
	if got[1].GLAccount != UnspecifiedGLAccount {
		t.Errorf("got[1].GLAccount = %q, want %q", got[1].GLAccount, UnspecifiedGLAccount)
	}
	if got[1].TotalDebitMinor != 300 || got[1].TotalCreditMinor != 300 {
		t.Errorf("unspecified bucket totals = debit:%d credit:%d, want 300/300", got[1].TotalDebitMinor, got[1].TotalCreditMinor)
	}
}

func TestBuildTrialBalance_EmptyInput(t *testing.T) {
	got := BuildTrialBalance(nil)
	if len(got) != 0 {
		t.Fatalf("got %d balances for empty input, want 0: %+v", len(got), got)
	}
	if checks := ReconcileByCurrency(got); len(checks) != 0 {
		t.Errorf("ReconcileByCurrency(empty) = %+v, want no checks", checks)
	}
}

func TestReconcileByCurrency_Balanced(t *testing.T) {
	balances := []GLBalance{
		{Currency: "USD", GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000},
		{Currency: "USD", GLAccount: "4000", TotalDebitMinor: 2750, TotalCreditMinor: 2750},
		{Currency: "USD", GLAccount: UnspecifiedGLAccount, TotalDebitMinor: 600, TotalCreditMinor: 600},
	}

	checks := ReconcileByCurrency(balances)
	if len(checks) != 1 {
		t.Fatalf("expected 1 currency check, got %+v", checks)
	}
	c := checks[0]
	if !c.OK || c.Debits != 103350 || c.Credits != 103350 {
		t.Errorf("USD check = %+v, want OK with 103350/103350", c)
	}
}

func TestReconcileByCurrency_UnbalancedCatchesDiff(t *testing.T) {
	balances := []GLBalance{
		{Currency: "USD", GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000},
		{Currency: "USD", GLAccount: "9999", TotalDebitMinor: 5000, TotalCreditMinor: 3000},
	}

	checks := ReconcileByCurrency(balances)
	if len(checks) != 1 || checks[0].OK {
		t.Fatalf("expected a failing USD check, got %+v", checks)
	}
	if checks[0].Debits != 105000 || checks[0].Credits != 103000 {
		t.Errorf("totals = %d/%d, want 105000/103000", checks[0].Debits, checks[0].Credits)
	}
}

// TestReconcileByCurrency_CrossCurrencyTrap is the regression test for the
// original bug: a single global sum can "balance" even when every individual
// currency is broken, because diffs in different currencies cancel out.
func TestReconcileByCurrency_CrossCurrencyTrap(t *testing.T) {
	balances := []GLBalance{
		{Currency: "USD", GLAccount: "1000", TotalDebitMinor: 100, TotalCreditMinor: 90}, // USD off by +10
		{Currency: "EUR", GLAccount: "1000", TotalDebitMinor: 90, TotalCreditMinor: 100}, // EUR off by -10
	}
	// Global sums would be 190 == 190 (false PASS). Per-currency must fail both.
	checks := ReconcileByCurrency(balances)
	if len(checks) != 2 {
		t.Fatalf("expected 2 checks, got %+v", checks)
	}
	for _, c := range checks {
		if c.OK {
			t.Errorf("currency %s must FAIL despite cancelling global sums: %+v", c.Currency, c)
		}
	}
}
