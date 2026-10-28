package translate

import (
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
)

func TestAccountCreatedToSeed(t *testing.T) {
	ev := events.LedgerEvent{
		RaftIndex: 3,
		Kind:      events.KindAccountCreated,
		AccountID: "alice",
		Currency:  "USD",
	}
	seed, err := AccountCreatedToSeed(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seed.AccountID != "alice" || seed.Currency != "USD" {
		t.Fatalf("unexpected seed: %+v", seed)
	}
}

func TestAccountCreatedToSeed_WrongKind(t *testing.T) {
	ev := events.LedgerEvent{Kind: events.KindTransferApplied}
	if _, err := AccountCreatedToSeed(ev); err != ErrWrongKind {
		t.Fatalf("expected ErrWrongKind, got %v", err)
	}
}

func TestAccountCreatedToSeed_MissingCurrency(t *testing.T) {
	ev := events.LedgerEvent{Kind: events.KindAccountCreated, AccountID: "alice"}
	if _, err := AccountCreatedToSeed(ev); err != ErrMissingCurrency {
		t.Fatalf("expected ErrMissingCurrency, got %v", err)
	}
}

func TestTransferToJournalRows(t *testing.T) {
	ts := time.Date(2026, 10, 28, 5, 33, 42, 0, time.UTC)
	ev := events.LedgerEvent{
		RaftIndex:   7,
		Kind:        events.KindTransferApplied,
		Timestamp:   ts,
		DedupID:     "txn-1",
		JournalID:   "jrn-7",
		FromAccount: "alice",
		ToAccount:   "bob",
		AmountMinor: 2500,
		GLAccount:   "4000",
		CostCenter:  "CC-1",
	}

	debit, credit, err := TransferToJournalRows(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if debit.AccountID != "alice" || debit.Direction != Debit {
		t.Fatalf("unexpected debit row: %+v", debit)
	}
	if credit.AccountID != "bob" || credit.Direction != Credit {
		t.Fatalf("unexpected credit row: %+v", credit)
	}
	for _, row := range []JournalEntryRow{debit, credit} {
		if row.TransferDedupID != "txn-1" {
			t.Errorf("expected shared dedup id, got %q", row.TransferDedupID)
		}
		if row.AmountMinor != 2500 {
			t.Errorf("expected amount 2500, got %d", row.AmountMinor)
		}
		if row.GLAccount == nil || *row.GLAccount != "4000" {
			t.Errorf("expected gl_account 4000, got %v", row.GLAccount)
		}
		if row.CostCenter == nil || *row.CostCenter != "CC-1" {
			t.Errorf("expected cost_center CC-1, got %v", row.CostCenter)
		}
		if !row.PostedAt.Equal(ts) {
			t.Errorf("expected posted_at %v, got %v", ts, row.PostedAt)
		}
		if row.RaftIndex != 7 {
			t.Errorf("expected raft_index 7, got %d", row.RaftIndex)
		}
	}
}

func TestTransferToJournalRows_OptionalFieldsAbsent(t *testing.T) {
	ev := events.LedgerEvent{
		Kind:        events.KindTransferApplied,
		DedupID:     "txn-2",
		FromAccount: "alice",
		ToAccount:   "bob",
		AmountMinor: 100,
	}
	debit, credit, err := TransferToJournalRows(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if debit.GLAccount != nil || debit.CostCenter != nil {
		t.Fatalf("expected nil optional fields on debit row, got %+v", debit)
	}
	if credit.GLAccount != nil || credit.CostCenter != nil {
		t.Fatalf("expected nil optional fields on credit row, got %+v", credit)
	}
}

func TestTransferToJournalRows_WrongKind(t *testing.T) {
	ev := events.LedgerEvent{Kind: events.KindAccountCreated}
	if _, _, err := TransferToJournalRows(ev); err != ErrWrongKind {
		t.Fatalf("expected ErrWrongKind, got %v", err)
	}
}

func TestTransferToBalanceDeltas(t *testing.T) {
	ev := events.LedgerEvent{
		RaftIndex:   9,
		Kind:        events.KindTransferApplied,
		FromAccount: "alice",
		ToAccount:   "bob",
		AmountMinor: 500,
	}
	from, to, err := TransferToBalanceDeltas(ev)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if from.AccountID != "alice" || from.DeltaMinor != -500 || from.RaftIndex != 9 {
		t.Fatalf("unexpected from delta: %+v", from)
	}
	if to.AccountID != "bob" || to.DeltaMinor != 500 || to.RaftIndex != 9 {
		t.Fatalf("unexpected to delta: %+v", to)
	}
	// Reconciliation invariant: debits and credits implied by a single
	// transfer must net to zero.
	if from.DeltaMinor+to.DeltaMinor != 0 {
		t.Fatalf("expected deltas to net to zero, got %d", from.DeltaMinor+to.DeltaMinor)
	}
}

func TestShouldApplyBalanceUpdate(t *testing.T) {
	cases := []struct {
		name                 string
		eventRaftIndex       int64
		lastAppliedRaftIndex int64
		want                 bool
	}{
		{"fresh row never applied", 5, 0, true},
		{"strictly newer index applies", 10, 9, true},
		{"equal index is a replay, must not reapply", 9, 9, false},
		{"older index is a stale replay, must not reapply", 3, 9, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldApplyBalanceUpdate(tc.eventRaftIndex, tc.lastAppliedRaftIndex)
			if got != tc.want {
				t.Errorf("ShouldApplyBalanceUpdate(%d, %d) = %v, want %v",
					tc.eventRaftIndex, tc.lastAppliedRaftIndex, got, tc.want)
			}
		})
	}
}
