package main

import "testing"

func TestAccountCreatedInsertsZeroBalance(t *testing.T) {
	c := NewBalanceCache()
	c.Apply(LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})

	bal, ok := c.Get("alice")
	if !ok {
		t.Fatalf("expected alice to be present after account_created")
	}
	if bal.Currency != "USD" {
		t.Errorf("expected currency USD, got %q", bal.Currency)
	}
	if bal.BalanceMinor != 0 {
		t.Errorf("expected zero balance, got %d", bal.BalanceMinor)
	}
}

func TestTransferBetweenTwoAccounts(t *testing.T) {
	c := NewBalanceCache()
	c.Apply(LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	c.Apply(LedgerEvent{RaftIndex: 2, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "USD"})
	c.Apply(LedgerEvent{RaftIndex: 3, Kind: eventKindAccountCreated, AccountID: "external-deposit", Currency: "USD"})

	// Fund alice from an external deposit account.
	c.Apply(LedgerEvent{
		RaftIndex: 4, Kind: eventKindTransferApplied,
		FromAccount: "external-deposit", ToAccount: "alice", AmountMinor: 10000,
	})
	// Alice pays Bob.
	c.Apply(LedgerEvent{
		RaftIndex: 5, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 2500,
	})

	aliceBal, ok := c.Get("alice")
	if !ok {
		t.Fatalf("expected alice present")
	}
	if aliceBal.BalanceMinor != 7500 {
		t.Errorf("expected alice balance 7500, got %d", aliceBal.BalanceMinor)
	}

	bobBal, ok := c.Get("bob")
	if !ok {
		t.Fatalf("expected bob present")
	}
	if bobBal.BalanceMinor != 2500 {
		t.Errorf("expected bob balance 2500, got %d", bobBal.BalanceMinor)
	}

	extBal, ok := c.Get("external-deposit")
	if !ok {
		t.Fatalf("expected external-deposit present")
	}
	if extBal.BalanceMinor != -10000 {
		t.Errorf("expected external-deposit balance -10000, got %d", extBal.BalanceMinor)
	}
}

func TestTransferCreatesMissingAccountsDefensively(t *testing.T) {
	c := NewBalanceCache()
	// No account_created events at all -- the cache should still cope.
	c.Apply(LedgerEvent{
		RaftIndex: 1, Kind: eventKindTransferApplied,
		FromAccount: "ghost-a", ToAccount: "ghost-b", AmountMinor: 500,
	})

	a, ok := c.Get("ghost-a")
	if !ok || a.BalanceMinor != -500 {
		t.Errorf("expected ghost-a balance -500, got %+v (ok=%v)", a, ok)
	}
	b, ok := c.Get("ghost-b")
	if !ok || b.BalanceMinor != 500 {
		t.Errorf("expected ghost-b balance 500, got %+v (ok=%v)", b, ok)
	}
}

func TestDuplicateOrStaleIndexIsIgnored(t *testing.T) {
	c := NewBalanceCache()
	c.Apply(LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	c.Apply(LedgerEvent{RaftIndex: 2, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "USD"})
	c.Apply(LedgerEvent{
		RaftIndex: 3, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 1000,
	})

	// Replay the exact same transfer event again (e.g. a reconnect glitch
	// that re-delivers an index we've already applied). It must be ignored.
	c.Apply(LedgerEvent{
		RaftIndex: 3, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 1000,
	})

	// Also try an out-of-order event with a lower index than the last
	// processed one.
	c.Apply(LedgerEvent{
		RaftIndex: 2, Kind: eventKindTransferApplied,
		FromAccount: "alice", ToAccount: "bob", AmountMinor: 999999,
	})

	alice, _ := c.Get("alice")
	bob, _ := c.Get("bob")
	if alice.BalanceMinor != -1000 {
		t.Errorf("expected alice balance -1000 after duplicate/stale events ignored, got %d", alice.BalanceMinor)
	}
	if bob.BalanceMinor != 1000 {
		t.Errorf("expected bob balance 1000 after duplicate/stale events ignored, got %d", bob.BalanceMinor)
	}

	idx, ok := c.LastProcessedIndex()
	if !ok || idx != 3 {
		t.Errorf("expected last processed index 3, got %d (ok=%v)", idx, ok)
	}
}

func TestUnknownAccountLookupReturnsNotFound(t *testing.T) {
	c := NewBalanceCache()
	_, ok := c.Get("nobody")
	if ok {
		t.Errorf("expected nobody to be absent from an empty cache")
	}
}

func TestAllReturnsSnapshotOfEveryAccount(t *testing.T) {
	c := NewBalanceCache()
	c.Apply(LedgerEvent{RaftIndex: 1, Kind: eventKindAccountCreated, AccountID: "alice", Currency: "USD"})
	c.Apply(LedgerEvent{RaftIndex: 2, Kind: eventKindAccountCreated, AccountID: "bob", Currency: "EUR"})

	all := c.All()
	if len(all) != 2 {
		t.Fatalf("expected 2 balances, got %d", len(all))
	}
}
