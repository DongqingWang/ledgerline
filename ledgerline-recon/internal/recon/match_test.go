package recon

import (
	"testing"
)

func TestReconcile_AllMatched(t *testing.T) {
	ledger := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t2", AmountMinor: 200, Currency: "USD"}}
	external := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t2", AmountMinor: 200, Currency: "USD"}}
	if breaks := Reconcile(ledger, external); len(breaks) != 0 {
		t.Errorf("expected no breaks, got %+v", breaks)
	}
}

func TestReconcile_MissingInLedger(t *testing.T) {
	ledger := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}}
	external := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t2", AmountMinor: 200, Currency: "USD"}}
	breaks := Reconcile(ledger, external)
	if len(breaks) != 1 || breaks[0].Type != BreakMissingInLedger || breaks[0].Ref != "t2" {
		t.Errorf("expected missing_in_ledger t2, got %+v", breaks)
	}
}

func TestReconcile_MissingInExternal(t *testing.T) {
	ledger := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t2", AmountMinor: 200, Currency: "USD"}}
	external := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}}
	breaks := Reconcile(ledger, external)
	if len(breaks) != 1 || breaks[0].Type != BreakMissingInExternal || breaks[0].Ref != "t2" {
		t.Errorf("expected missing_in_external t2, got %+v", breaks)
	}
}

func TestReconcile_AmountMismatch(t *testing.T) {
	ledger := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}}
	external := []Record{{Ref: "t1", AmountMinor: 90, Currency: "USD"}}
	breaks := Reconcile(ledger, external)
	if len(breaks) != 1 || breaks[0].Type != BreakAmountMismatch {
		t.Fatalf("expected amount_mismatch, got %+v", breaks)
	}
	if breaks[0].LedgerAmount != 100 || breaks[0].ExternalAmount != 90 {
		t.Errorf("mismatch amounts wrong: %+v", breaks[0])
	}
}

func TestReconcile_Duplicate(t *testing.T) {
	ledger := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t1", AmountMinor: 100, Currency: "USD"}}
	external := []Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}}
	breaks := Reconcile(ledger, external)
	found := false
	for _, b := range breaks {
		if b.Type == BreakDuplicate && b.Ref == "t1" && b.Detail == "ledger" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected duplicate break for t1 (ledger), got %+v", breaks)
	}
}

func TestReconcile_DeterministicSort(t *testing.T) {
	ledger := []Record{{Ref: "b", AmountMinor: 1, Currency: "USD"}}
	external := []Record{{Ref: "a", AmountMinor: 2, Currency: "USD"}, {Ref: "c", AmountMinor: 3, Currency: "USD"}}
	breaks := Reconcile(ledger, external)
	// sorted by (type, ref): missing_in_external "b" first (e < l), then
	// missing_in_ledger "a", "c".
	if len(breaks) != 3 {
		t.Fatalf("expected 3 breaks, got %+v", breaks)
	}
	if breaks[0].Type != BreakMissingInExternal || breaks[0].Ref != "b" {
		t.Errorf("breaks[0] = %+v, want missing_in_external b", breaks[0])
	}
	if breaks[1].Ref != "a" || breaks[2].Ref != "c" {
		t.Errorf("missing_in_ledger breaks should be sorted by ref: %+v", breaks[1:])
	}
}

func TestReconcile_Empty(t *testing.T) {
	if breaks := Reconcile(nil, nil); len(breaks) != 0 {
		t.Errorf("expected no breaks, got %+v", breaks)
	}
}
