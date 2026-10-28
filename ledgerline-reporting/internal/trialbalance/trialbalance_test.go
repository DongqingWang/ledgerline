package trialbalance

import "testing"

func TestBuildTrialBalance_MultipleGLAccounts(t *testing.T) {
	rows := []JournalRow{
		{GLAccount: "1000", Direction: "debit", AmountMinor: 100000},
		{GLAccount: "1000", Direction: "credit", AmountMinor: 100000},
		{GLAccount: "4000", Direction: "debit", AmountMinor: 1500},
		{GLAccount: "4000", Direction: "debit", AmountMinor: 1250},
		{GLAccount: "4000", Direction: "credit", AmountMinor: 2750},
	}

	got := BuildTrialBalance(rows)

	want := []GLBalance{
		{GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000, NetMinor: 0},
		{GLAccount: "4000", TotalDebitMinor: 2750, TotalCreditMinor: 2750, NetMinor: 0},
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

func TestBuildTrialBalance_NullGLAccountBucketsAsUnspecified(t *testing.T) {
	rows := []JournalRow{
		{GLAccount: "", Direction: "debit", AmountMinor: 300},
		{GLAccount: "", Direction: "credit", AmountMinor: 300},
		{GLAccount: "1000", Direction: "debit", AmountMinor: 500},
		{GLAccount: "1000", Direction: "credit", AmountMinor: 500},
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

	ok, debits, credits := Reconcile(got)
	if !ok {
		t.Errorf("Reconcile(empty) ok = false, want true")
	}
	if debits != 0 || credits != 0 {
		t.Errorf("Reconcile(empty) = (debits=%d, credits=%d), want (0, 0)", debits, credits)
	}
}

func TestReconcile_Balanced(t *testing.T) {
	balances := []GLBalance{
		{GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000},
		{GLAccount: "4000", TotalDebitMinor: 2750, TotalCreditMinor: 2750},
		{GLAccount: UnspecifiedGLAccount, TotalDebitMinor: 600, TotalCreditMinor: 600},
	}

	ok, debits, credits := Reconcile(balances)
	if !ok {
		t.Errorf("Reconcile() ok = false, want true (debits=%d, credits=%d)", debits, credits)
	}
	if debits != 103350 || credits != 103350 {
		t.Errorf("Reconcile() = (debits=%d, credits=%d), want (103350, 103350)", debits, credits)
	}
}

func TestReconcile_UnbalancedCatchesDiff(t *testing.T) {
	// Intentionally unbalanced fixture: GL account "9999" has more debits
	// than credits, simulating a missing offsetting entry or a bug upstream.
	balances := []GLBalance{
		{GLAccount: "1000", TotalDebitMinor: 100000, TotalCreditMinor: 100000},
		{GLAccount: "9999", TotalDebitMinor: 5000, TotalCreditMinor: 3000},
	}

	ok, debits, credits := Reconcile(balances)
	if ok {
		t.Fatalf("Reconcile() ok = true, want false for unbalanced input")
	}
	if debits != 105000 {
		t.Errorf("debits = %d, want 105000", debits)
	}
	if credits != 103000 {
		t.Errorf("credits = %d, want 103000", credits)
	}
	if diff := debits - credits; diff != 2000 {
		t.Errorf("diff = %d, want 2000", diff)
	}
}
