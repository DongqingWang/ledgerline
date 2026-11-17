package translate

import (
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
)

func TestJournalToRows_PerLegGLAndLineIndex(t *testing.T) {
	ts := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	ev := events.LedgerEvent{
		RaftIndex: 12, Kind: events.KindJournalApplied, Timestamp: ts,
		DedupID: "payout-1", JournalID: "jrn-12",
		Lines: []events.JournalLine{
			{AccountID: "platform", Direction: "debit", AmountMinor: 10000, Currency: "USD", GLAccount: "2100", CostCenter: "CC-PAYOUT-US"},
			{AccountID: "seller", Direction: "credit", AmountMinor: 9700, Currency: "USD", GLAccount: "4000"},
			{AccountID: "fees", Direction: "credit", AmountMinor: 300, Currency: "USD", GLAccount: "4900"},
		},
	}
	rows, err := JournalToRows(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(rows))
	}
	wantGL := []string{"2100", "4000", "4900"}
	for i, row := range rows {
		if row.LineIndex != i {
			t.Errorf("row %d: line_index = %d", i, row.LineIndex)
		}
		if row.TransferDedupID != "payout-1" || row.Currency != "USD" || row.RaftIndex != 12 {
			t.Errorf("row %d: wrong base fields: %+v", i, row)
		}
		if row.GLAccount == nil || *row.GLAccount != wantGL[i] {
			t.Errorf("row %d: GL = %v, want %s", i, row.GLAccount, wantGL[i])
		}
		if row.ReversalOf != nil {
			t.Errorf("row %d: non-reversal should have nil reversal_of", i)
		}
	}
}

func TestJournalToRows_ReversalLink(t *testing.T) {
	ev := events.LedgerEvent{
		RaftIndex: 13, Kind: events.KindJournalApplied, DedupID: "rev-1", ReversalOf: "t1",
		Lines: []events.JournalLine{
			{AccountID: "platform", Direction: "credit", AmountMinor: 10000, Currency: "USD"},
			{AccountID: "seller", Direction: "debit", AmountMinor: 9700, Currency: "USD"},
			{AccountID: "fees", Direction: "debit", AmountMinor: 300, Currency: "USD"},
		},
	}
	rows, err := JournalToRows(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, row := range rows {
		if row.ReversalOf == nil || *row.ReversalOf != "t1" {
			t.Fatalf("reversal rows must link to original entry's dedup id: %+v", row)
		}
	}
}

func TestTransferToJournalRows_MissingCurrencyIsHardError(t *testing.T) {
	ev := events.LedgerEvent{
		Kind: events.KindTransferApplied, DedupID: "t", FromAccount: "a", ToAccount: "b", AmountMinor: 1,
	}
	if _, _, err := TransferToJournalRows(ev); err == nil {
		t.Fatalf("missing currency must be a hard error, not an empty-string fallback")
	}
}

func TestEventToAccountDeltas_JournalNetsSameAccount(t *testing.T) {
	ev := events.LedgerEvent{
		RaftIndex: 5, Kind: events.KindJournalApplied, DedupID: "j",
		Lines: []events.JournalLine{
			{AccountID: "a", Direction: "debit", AmountMinor: 100, Currency: "USD"},
			{AccountID: "b", Direction: "credit", AmountMinor: 60, Currency: "USD"},
			{AccountID: "a", Direction: "credit", AmountMinor: 40, Currency: "USD"},
		},
	}
	deltas, err := EventToAccountDeltas(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected 2 netted deltas, got %d: %+v", len(deltas), deltas)
	}
	// Sorted by account id: a nets -60, b +60.
	if deltas[0].AccountID != "a" || deltas[0].BalanceMinor != -60 {
		t.Fatalf("a should net -60: %+v", deltas[0])
	}
	if deltas[1].AccountID != "b" || deltas[1].BalanceMinor != 60 {
		t.Fatalf("b should net +60: %+v", deltas[1])
	}
}

func TestEventToAccountDeltas_PendingLifecycle(t *testing.T) {
	created := events.LedgerEvent{RaftIndex: 1, Kind: events.KindPendingCreated, FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"}
	deltas, err := EventToAccountDeltas(created)
	if err != nil || len(deltas) != 1 || deltas[0].PendingMinor != 100 || deltas[0].BalanceMinor != 0 {
		t.Fatalf("created: %+v err=%v", deltas, err)
	}

	posted := events.LedgerEvent{RaftIndex: 2, Kind: events.KindPendingPosted, FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"}
	deltas, err = EventToAccountDeltas(posted)
	if err != nil || len(deltas) != 2 {
		t.Fatalf("posted: %+v err=%v", deltas, err)
	}
	if deltas[0].AccountID != "a" || deltas[0].BalanceMinor != -100 || deltas[0].PendingMinor != -100 {
		t.Fatalf("posted from-side wrong: %+v", deltas[0])
	}
	if deltas[1].AccountID != "b" || deltas[1].BalanceMinor != 100 || deltas[1].PendingMinor != 0 {
		t.Fatalf("posted to-side wrong: %+v", deltas[1])
	}

	voided := events.LedgerEvent{RaftIndex: 3, Kind: events.KindPendingVoided, FromAccount: "a", ToAccount: "b", AmountMinor: 100, Currency: "USD"}
	deltas, err = EventToAccountDeltas(voided)
	if err != nil || len(deltas) != 1 || deltas[0].PendingMinor != -100 || deltas[0].BalanceMinor != 0 {
		t.Fatalf("voided: %+v err=%v", deltas, err)
	}
}

func TestEventToAccountDeltas_ExpiredRideAlong(t *testing.T) {
	ev := events.LedgerEvent{
		RaftIndex: 9, Kind: events.KindCommandRejected, ErrCode: "invalid_request",
		ExpiredPendings: []events.ExpiredPending{
			{PendingID: "pnd-3", FromAccount: "a", ToAccount: "b", AmountMinor: 70, Currency: "USD"},
		},
	}
	deltas, err := EventToAccountDeltas(ev)
	if err != nil || len(deltas) != 1 || deltas[0].PendingMinor != -70 {
		t.Fatalf("expired hold must release even on a rejected entry: %+v err=%v", deltas, err)
	}
}

func TestEventToAccountDeltas_NoOpKinds(t *testing.T) {
	for _, kind := range []events.Kind{events.KindCommandRejected, events.KindCommandDuplicate, events.KindAccountCreated} {
		deltas, err := EventToAccountDeltas(events.LedgerEvent{RaftIndex: 1, Kind: kind})
		if err != nil || len(deltas) != 0 {
			t.Fatalf("%s should produce no deltas: %+v err=%v", kind, deltas, err)
		}
	}
	if _, err := EventToAccountDeltas(events.LedgerEvent{RaftIndex: 1, Kind: "mystery"}); err == nil {
		t.Fatalf("unknown kind must error, not silently skip")
	}
}
