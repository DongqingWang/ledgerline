package fsm

import (
	"testing"

	"github.com/hashicorp/raft"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

func mustApply(t *testing.T, f *FSM, index uint64, cmd Command) *ApplyResult {
	t.Helper()
	data, err := cmd.Encode()
	if err != nil {
		t.Fatalf("encode command: %v", err)
	}
	res := f.Apply(&raft.Log{Index: index, Data: data})
	ar, ok := res.(*ApplyResult)
	if !ok {
		t.Fatalf("Apply returned unexpected type %T", res)
	}
	return ar
}

func createAccount(t *testing.T, f *FSM, index uint64, id, currency string) *ApplyResult {
	t.Helper()
	return mustApply(t, f, index, Command{
		Type:          CmdCreateAccount,
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: id, Currency: currency},
	})
}

// createUnlimitedAccount creates a clearing/external-funding account that is
// allowed to go negative — the standard double-entry way money enters the
// ledger from outside, rather than a special-cased "mint" bypass.
func createUnlimitedAccount(t *testing.T, f *FSM, index uint64, id, currency string) *ApplyResult {
	t.Helper()
	return mustApply(t, f, index, Command{
		Type:          CmdCreateAccount,
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: id, Currency: currency, Unlimited: true},
	})
}

func fund(t *testing.T, f *FSM, index uint64, unlimitedSource, to string, amount int64) {
	t.Helper()
	res := mustApply(t, f, index, Command{
		Type: CmdAssertTransfer,
		AssertTransfer: &ledgertypes.TransferRequest{
			DedupID: "seed-" + to, FromAccount: unlimitedSource, ToAccount: to,
			AmountMinor: amount, Currency: "USD",
		},
	})
	if res.IsError() {
		t.Fatalf("funding transfer failed: %s: %s", res.ErrCode, res.ErrMsg)
	}
}

func TestCreateAccount(t *testing.T) {
	f := New()

	res := createAccount(t, f, 1, "acct-a", "USD")
	if res.IsError() {
		t.Fatalf("unexpected error: %s: %s", res.ErrCode, res.ErrMsg)
	}
	if res.Account.BalanceMinor != 0 {
		t.Fatalf("new account should start at zero balance, got %d", res.Account.BalanceMinor)
	}

	// Duplicate account IDs are rejected.
	res = createAccount(t, f, 2, "acct-a", "USD")
	if res.ErrCode != ledgertypes.ErrAccountExists {
		t.Fatalf("expected ErrAccountExists, got %q", res.ErrCode)
	}
}

func TestAssertTransfer_HappyPath(t *testing.T) {
	f := New()
	createUnlimitedAccount(t, f, 1, "external-deposit", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external-deposit", "alice", 10_000)

	res := mustApply(t, f, 5, Command{
		Type: CmdAssertTransfer,
		AssertTransfer: &ledgertypes.TransferRequest{
			DedupID: "txn-1", FromAccount: "alice", ToAccount: "bob",
			AmountMinor: 2_500, Currency: "USD", GLAccount: "4000", CostCenter: "CC-1",
		},
	})
	if res.IsError() {
		t.Fatalf("unexpected error: %s: %s", res.ErrCode, res.ErrMsg)
	}
	if res.Transfer.Status != ledgertypes.TransferStatusApplied {
		t.Fatalf("expected applied, got %s", res.Transfer.Status)
	}
	if res.Transfer.JournalID == "" {
		t.Fatalf("expected a journal id")
	}

	alice, _ := f.Account("alice")
	bob, _ := f.Account("bob")
	if alice.BalanceMinor != 7_500 {
		t.Fatalf("alice balance = %d, want 7500", alice.BalanceMinor)
	}
	if bob.BalanceMinor != 2_500 {
		t.Fatalf("bob balance = %d, want 2500", bob.BalanceMinor)
	}

	// Double-entry invariant: the debit leg and credit leg must net to zero —
	// what alice lost, bob gained, exactly.
	if (10_000 - alice.BalanceMinor) != bob.BalanceMinor {
		t.Fatalf("debit/credit legs don't balance: alice lost %d, bob has %d", 10_000-alice.BalanceMinor, bob.BalanceMinor)
	}
}

func TestAssertTransfer_IdempotentOnDedupID(t *testing.T) {
	f := New()
	createUnlimitedAccount(t, f, 1, "external-deposit", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external-deposit", "alice", 1_000)

	req := &ledgertypes.TransferRequest{
		DedupID: "retry-me", FromAccount: "alice", ToAccount: "bob",
		AmountMinor: 500, Currency: "USD",
	}

	first := mustApply(t, f, 5, Command{Type: CmdAssertTransfer, AssertTransfer: req})
	if first.IsError() || first.Transfer.Status != ledgertypes.TransferStatusApplied {
		t.Fatalf("first apply should succeed and be applied: %+v", first)
	}

	// Simulate a client retry (e.g. after a network timeout) resubmitting
	// the exact same command at a later raft index.
	second := mustApply(t, f, 6, Command{Type: CmdAssertTransfer, AssertTransfer: req})
	if second.IsError() {
		t.Fatalf("retry should not error: %+v", second)
	}
	if second.Transfer.Status != ledgertypes.TransferStatusDuplicate {
		t.Fatalf("retry should report duplicate, got %s", second.Transfer.Status)
	}
	if second.Transfer.JournalID != first.Transfer.JournalID {
		t.Fatalf("retry should return the original journal id")
	}

	alice, _ := f.Account("alice")
	bob, _ := f.Account("bob")
	if alice.BalanceMinor != 500 || bob.BalanceMinor != 500 {
		t.Fatalf("balances must reflect exactly one transfer, got alice=%d bob=%d", alice.BalanceMinor, bob.BalanceMinor)
	}
}

func TestAssertTransfer_InsufficientFunds(t *testing.T) {
	f := New()
	createAccount(t, f, 1, "alice", "USD")
	createAccount(t, f, 2, "bob", "USD")

	res := mustApply(t, f, 3, Command{
		Type: CmdAssertTransfer,
		AssertTransfer: &ledgertypes.TransferRequest{
			DedupID: "txn-1", FromAccount: "alice", ToAccount: "bob",
			AmountMinor: 100, Currency: "USD",
		},
	})
	if res.ErrCode != ledgertypes.ErrInsufficientFunds {
		t.Fatalf("expected ErrInsufficientFunds, got %q", res.ErrCode)
	}
}

func TestAssertTransfer_UnlimitedAccountCanGoNegative(t *testing.T) {
	f := New()
	createUnlimitedAccount(t, f, 1, "external-deposit", "USD")
	createAccount(t, f, 2, "alice", "USD")

	res := mustApply(t, f, 3, Command{
		Type: CmdAssertTransfer,
		AssertTransfer: &ledgertypes.TransferRequest{
			DedupID: "txn-1", FromAccount: "external-deposit", ToAccount: "alice",
			AmountMinor: 1_000_000, Currency: "USD",
		},
	})
	if res.IsError() {
		t.Fatalf("unlimited account should be allowed to go negative: %s: %s", res.ErrCode, res.ErrMsg)
	}

	source, _ := f.Account("external-deposit")
	if source.BalanceMinor != -1_000_000 {
		t.Fatalf("external-deposit balance = %d, want -1000000", source.BalanceMinor)
	}
}

func TestAssertTransfer_AccountNotFound(t *testing.T) {
	f := New()
	createAccount(t, f, 1, "alice", "USD")

	res := mustApply(t, f, 2, Command{
		Type: CmdAssertTransfer,
		AssertTransfer: &ledgertypes.TransferRequest{
			DedupID: "txn-1", FromAccount: "alice", ToAccount: "ghost",
			AmountMinor: 100, Currency: "USD",
		},
	})
	if res.ErrCode != ledgertypes.ErrAccountNotFound {
		t.Fatalf("expected ErrAccountNotFound, got %q", res.ErrCode)
	}
}

func TestSnapshotRestore_RoundTrip(t *testing.T) {
	f := New()
	createUnlimitedAccount(t, f, 1, "external-deposit", "USD")
	createAccount(t, f, 2, "alice", "USD")
	fund(t, f, 3, "external-deposit", "alice", 5_000)

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	sink := newMemSnapshotSink()
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("persist: %v", err)
	}

	restored := New()
	if err := restored.Restore(sink.reader()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	alice, ok := restored.Account("alice")
	if !ok {
		t.Fatalf("restored fsm missing alice")
	}
	if alice.BalanceMinor != 5_000 {
		t.Fatalf("restored balance = %d, want 5000", alice.BalanceMinor)
	}
}

func TestEventBus_ReplayAndLive(t *testing.T) {
	f := New()
	createUnlimitedAccount(t, f, 1, "external-deposit", "USD")
	createAccount(t, f, 2, "alice", "USD")
	fund(t, f, 3, "external-deposit", "alice", 1_000)

	// Subscribing from index 0 should replay everything so far.
	ch, backlog, cancel := f.Events().Subscribe(0)
	defer cancel()
	if len(backlog) != 3 {
		t.Fatalf("expected 3 backlog events (2 creates + 1 transfer), got %d", len(backlog))
	}

	createAccount(t, f, 4, "bob", "USD")

	select {
	case ev := <-ch:
		if ev.Kind != ledgertypes.EventAccountCreated || ev.AccountID != "bob" {
			t.Fatalf("unexpected live event: %+v", ev)
		}
	default:
		t.Fatalf("expected a live event for bob's account creation")
	}
}
