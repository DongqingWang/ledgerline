package main

import "testing"

func mustApply(t *testing.T, c *BalanceCache, ev LedgerEvent) {
	t.Helper()
	if err := c.Apply(ev); err != nil {
		t.Fatalf("apply index %d (%s): %v", ev.RaftIndex, ev.Kind, err)
	}
}

// TestGapIsRejected: the engine emits one event per log entry, so a skipped
// index means lost delivery. The cache must refuse to apply past a gap
// rather than silently drift.
func TestGapIsRejected(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 2, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "USD"})

	err := c.Apply(LedgerEvent{
		RaftIndex: 4, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD",
	})
	if err == nil {
		t.Fatalf("expected a gap error for index 4 after index 2")
	}
	if idx, _ := c.LastProcessedIndex(); idx != 2 {
		t.Fatalf("watermark must not advance past a gap, got %d", idx)
	}
	alice, _ := c.Get("alice")
	if alice.BalanceMinor != 0 {
		t.Fatalf("gap event must not be applied, alice=%d", alice.BalanceMinor)
	}

	// The missing index arriving late heals the stream.
	mustApply(t, c, LedgerEvent{RaftIndex: 3, Kind: eventKindCommandRejected, ErrCode: "invalid_request"})
	mustApply(t, c, LedgerEvent{
		RaftIndex: 4, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD",
	})
}

func TestFirstEventMayStartAtAnyIndex(t *testing.T) {
	c := NewBalanceCache()
	// Raft's index 1 is typically a configuration entry that never reaches
	// the FSM, so the first business event can legitimately be index 2+.
	mustApply(t, c, LedgerEvent{RaftIndex: 7, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 8, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "USD"})
}

func TestJournalAppliedPerLeg(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindJournalApplied, Lines: []EventJournalLine{
		{AccountID: "platform", Direction: "debit", AmountMinor: 10000, Currency: "USD"},
		{AccountID: "seller", Direction: "credit", AmountMinor: 9700, Currency: "USD"},
		{AccountID: "fees", Direction: "credit", AmountMinor: 300, Currency: "USD"},
	}})
	platform, _ := c.Get("platform")
	seller, _ := c.Get("seller")
	fees, _ := c.Get("fees")
	if platform.BalanceMinor != -10000 || seller.BalanceMinor != 9700 || fees.BalanceMinor != 300 {
		t.Fatalf("journal legs wrong: platform=%d seller=%d fees=%d", platform.BalanceMinor, seller.BalanceMinor, fees.BalanceMinor)
	}
	if seller.Currency != "USD" {
		t.Fatalf("journal leg currency not propagated: %q", seller.Currency)
	}
}

func TestPendingLifecycle(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 2, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 3, Kind: eventKindTransferApplied, FromAccount: "ext", ToAccount: "alice", AmountMinor: 1000, Currency: "USD"})

	mustApply(t, c, LedgerEvent{RaftIndex: 4, Kind: eventKindPendingCreated, PendingID: "pnd-4", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD"})
	alice, _ := c.Get("alice")
	if alice.BalanceMinor != 1000 || alice.PendingMinor != 600 || alice.AvailableMinor != 400 {
		t.Fatalf("hold should reserve without posting: %+v", alice)
	}

	mustApply(t, c, LedgerEvent{RaftIndex: 5, Kind: eventKindPendingPosted, PendingID: "pnd-4", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD"})
	alice, _ = c.Get("alice")
	bob, _ := c.Get("bob")
	if alice.BalanceMinor != 400 || alice.PendingMinor != 0 || bob.BalanceMinor != 600 {
		t.Fatalf("post wrong: alice=%+v bob=%+v", alice, bob)
	}

	// Void path.
	mustApply(t, c, LedgerEvent{RaftIndex: 6, Kind: eventKindPendingCreated, PendingID: "pnd-6", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 7, Kind: eventKindPendingVoided, PendingID: "pnd-6", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD"})
	alice, _ = c.Get("alice")
	if alice.PendingMinor != 0 || alice.AvailableMinor != 400 {
		t.Fatalf("void should release hold: %+v", alice)
	}
}

func TestExpiredPendingsReleasedBeforeBody(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 2, Kind: eventKindTransferApplied, FromAccount: "ext", ToAccount: "alice", AmountMinor: 1000, Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 3, Kind: eventKindPendingCreated, PendingID: "pnd-3", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD"})

	// An unrelated command whose application swept the expired hold.
	mustApply(t, c, LedgerEvent{
		RaftIndex: 4, Kind: eventKindAccountCreated, AccountID: "carol", Currency: "USD",
		ExpiredPendings: []ExpiredPending{{PendingID: "pnd-3", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD"}},
	})
	alice, _ := c.Get("alice")
	if alice.PendingMinor != 0 || alice.AvailableMinor != 1000 {
		t.Fatalf("expired hold not released: %+v", alice)
	}
}

func TestRejectedAndDuplicateAdvanceWatermark(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	mustApply(t, c, LedgerEvent{RaftIndex: 2, Kind: eventKindCommandRejected, ErrCode: "account_exists"})
	mustApply(t, c, LedgerEvent{RaftIndex: 3, Kind: eventKindCommandDuplicate, DedupID: "k1"})
	if idx, _ := c.LastProcessedIndex(); idx != 3 {
		t.Fatalf("watermark should be 3, got %d", idx)
	}
}

func TestResetClearsEverything(t *testing.T) {
	c := NewBalanceCache()
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	c.Reset()
	if c.Count() != 0 {
		t.Fatalf("reset should clear balances")
	}
	if _, ok := c.LastProcessedIndex(); ok {
		t.Fatalf("reset should clear the watermark")
	}
	// Full resync from 0 works after reset.
	mustApply(t, c, LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
}
