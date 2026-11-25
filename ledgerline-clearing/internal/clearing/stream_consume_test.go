package clearing

import (
	"testing"

	"github.com/ledgerline/ledgerline-clearing/internal/stream"
)

func TestCapturesFromEvent_ExtractsPayableCredit(t *testing.T) {
	ev := stream.Event{
		RaftIndex: 5, Kind: "journal_applied", DedupID: "cap-1-journal",
		Lines: []stream.Line{
			{AccountID: "scheme-receivable", Direction: "debit", AmountMinor: 10000, Currency: "USD", GLAccount: "1100"},
			{AccountID: "merchant-payable", Direction: "credit", AmountMinor: 9700, Currency: "USD", GLAccount: "2200", CostCenter: "m1"},
			{AccountID: "acquiring-fee-revenue", Direction: "credit", AmountMinor: 300, Currency: "USD", GLAccount: "4000"},
		},
	}
	got := CapturesFromEvent(ev, "merchant-payable")
	if len(got) != 1 {
		t.Fatalf("expected 1 capture, got %d: %+v", len(got), got)
	}
	c := got[0]
	if c.MerchantID != "m1" || c.NetMinor != 9700 || c.Currency != "USD" || c.Ref != "cap-1-journal" {
		t.Errorf("capture wrong: %+v", c)
	}
}

func TestCapturesFromEvent_ReversalIsNegative(t *testing.T) {
	ev := stream.Event{
		Kind: "journal_applied", DedupID: "rev-1",
		Lines: []stream.Line{
			{AccountID: "merchant-payable", Direction: "debit", AmountMinor: 9700, Currency: "USD", CostCenter: "m1"},
			{AccountID: "scheme-receivable", Direction: "credit", AmountMinor: 9700, Currency: "USD"},
		},
	}
	got := CapturesFromEvent(ev, "merchant-payable")
	if len(got) != 1 || got[0].NetMinor != -9700 {
		t.Errorf("reversal should net negative: %+v", got)
	}
}

func TestCapturesFromEvent_IgnoresNonJournal(t *testing.T) {
	ev := stream.Event{Kind: "transfer_applied", DedupID: "t1"}
	if got := CapturesFromEvent(ev, "merchant-payable"); len(got) != 0 {
		t.Errorf("non-journal events yield no captures: %+v", got)
	}
}

func TestHandleEvent_IdempotentIngest(t *testing.T) {
	svc := NewService(newStubEngine())
	ev := stream.Event{
		Kind: "journal_applied", DedupID: "cap-1-journal",
		Lines: []stream.Line{
			{AccountID: "merchant-payable", Direction: "credit", AmountMinor: 9700, Currency: "USD", CostCenter: "m1"},
		},
	}
	svc.HandleEvent(ev)
	svc.HandleEvent(ev) // replay
	if len(svc.Pending()) != 1 {
		t.Errorf("replay must not double-ingest: %+v", svc.Pending())
	}
}
