package recon

import (
	"testing"

	"github.com/ledgerline/ledgerline-recon/internal/stream"
)

func TestLedgerRecordsFromEvent_Journal(t *testing.T) {
	ev := stream.Event{
		Kind: "journal_applied", DedupID: "cap-1-journal",
		Lines: []stream.Line{
			{AccountID: "scheme-receivable", Direction: "debit", AmountMinor: 10000, Currency: "USD"},
			{AccountID: "merchant-payable", Direction: "credit", AmountMinor: 9700, Currency: "USD"},
			{AccountID: "acquiring-fee-revenue", Direction: "credit", AmountMinor: 300, Currency: "USD"},
		},
	}
	got := LedgerRecordsFromEvent(ev)
	if len(got) != 1 || got[0].Ref != "cap-1-journal" || got[0].AmountMinor != 10000 || got[0].Currency != "USD" {
		t.Errorf("record wrong: %+v", got)
	}
}

func TestLedgerRecordsFromEvent_Transfer(t *testing.T) {
	ev := stream.Event{Kind: "transfer_applied", DedupID: "t1", AmountMinor: 500, Currency: "USD"}
	got := LedgerRecordsFromEvent(ev)
	if len(got) != 1 || got[0].Ref != "t1" || got[0].AmountMinor != 500 {
		t.Errorf("record wrong: %+v", got)
	}
}

func TestLedgerRecordsFromEvent_IgnoresOtherKinds(t *testing.T) {
	if got := LedgerRecordsFromEvent(stream.Event{Kind: "command_rejected"}); len(got) != 0 {
		t.Errorf("non-money events yield no records: %+v", got)
	}
}

func TestHandleEvent_IdempotentByRef(t *testing.T) {
	svc := NewService(newStubEngine())
	ev := stream.Event{Kind: "transfer_applied", DedupID: "t1", AmountMinor: 500, Currency: "USD"}
	svc.HandleEvent(ev)
	svc.HandleEvent(ev) // replay
	breaks := svc.Run() // ledger has t1, external empty -> missing_in_external
	if len(breaks) != 1 {
		t.Fatalf("expected 1 break (t1 missing_in_external), got %+v", breaks)
	}
}
