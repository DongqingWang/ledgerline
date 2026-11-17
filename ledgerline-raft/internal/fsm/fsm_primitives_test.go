package fsm

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

func journalCmd(dedupID string, lines ...ledgertypes.JournalLine) Command {
	return Command{Type: CmdAssertJournal, AssertJournal: &ledgertypes.JournalRequest{DedupID: dedupID, Lines: lines}}
}

func line(account string, dir ledgertypes.Direction, amount int64, gl string) ledgertypes.JournalLine {
	return ledgertypes.JournalLine{AccountID: account, Direction: dir, AmountMinor: amount, GLAccount: gl}
}

// TestAssertJournal_FeeSplitAtomic is the regression test for the original
// gateway bug: a fee-split payout used to be two sequential AssertTransfer
// calls, so the net leg could commit while the fee leg failed. As one
// journal, all three legs commit together with per-leg GL dimensions.
func TestAssertJournal_FeeSplitAtomic(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "platform", "USD")
	createAccount(t, f, 2, "seller-1", "USD")
	createAccount(t, f, 3, "platform-fee-revenue", "USD")

	res := mustApply(t, f, 4, journalCmd("payout-1",
		line("platform", ledgertypes.Debit, 10_000, "2100"),
		line("seller-1", ledgertypes.Credit, 9_700, "4000"),
		line("platform-fee-revenue", ledgertypes.Credit, 300, "4900"),
	))
	if res.IsError() {
		t.Fatalf("fee-split journal failed: %s: %s", res.ErrCode, res.ErrMsg)
	}

	seller, _ := f.Account("seller-1")
	fee, _ := f.Account("platform-fee-revenue")
	if seller.BalanceMinor != 9_700 || fee.BalanceMinor != 300 {
		t.Fatalf("balances wrong: seller=%d fee=%d", seller.BalanceMinor, fee.BalanceMinor)
	}

	_, backlog, cancel := f.Events().Subscribe(4)
	defer cancel()
	if len(backlog) != 1 || backlog[0].Kind != ledgertypes.EventJournalApplied {
		t.Fatalf("expected one journal_applied event, got %+v", backlog)
	}
	ev := backlog[0]
	if len(ev.Lines) != 3 {
		t.Fatalf("expected 3 event lines, got %d", len(ev.Lines))
	}
	wantGL := map[string]string{"platform": "2100", "seller-1": "4000", "platform-fee-revenue": "4900"}
	for _, ln := range ev.Lines {
		if ln.GLAccount != wantGL[ln.AccountID] {
			t.Fatalf("leg %s: GL = %q, want %q", ln.AccountID, ln.GLAccount, wantGL[ln.AccountID])
		}
		if ln.Currency != "USD" {
			t.Fatalf("leg %s: event line missing currency", ln.AccountID)
		}
	}
}

// TestAssertJournal_InsufficientFundsAllOrNothing reproduces the exact
// counterexample from the review: balance 90, amount 100, fee 20. The old
// two-transfer split committed the 80 net leg and then failed the fee leg;
// the journal must reject everything.
func TestAssertJournal_InsufficientFundsAllOrNothing(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "payer", "USD")
	createAccount(t, f, 3, "payee", "USD")
	createAccount(t, f, 4, "fees", "USD")
	fund(t, f, 5, "external", "payer", 90)

	res := mustApply(t, f, 6, journalCmd("split-1",
		line("payer", ledgertypes.Debit, 100, ""),
		line("payee", ledgertypes.Credit, 80, ""),
		line("fees", ledgertypes.Credit, 20, ""),
	))
	if res.ErrCode != ledgertypes.ErrInsufficientFunds {
		t.Fatalf("expected ErrInsufficientFunds, got %q (%s)", res.ErrCode, res.ErrMsg)
	}
	payer, _ := f.Account("payer")
	payee, _ := f.Account("payee")
	fees, _ := f.Account("fees")
	if payer.BalanceMinor != 90 || payee.BalanceMinor != 0 || fees.BalanceMinor != 0 {
		t.Fatalf("partial application leaked: payer=%d payee=%d fees=%d", payer.BalanceMinor, payee.BalanceMinor, fees.BalanceMinor)
	}
}

func TestAssertJournal_UnbalancedRejected(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "a", "USD")
	createAccount(t, f, 2, "b", "USD")

	res := mustApply(t, f, 3, journalCmd("bad-1",
		line("a", ledgertypes.Debit, 100, ""),
		line("b", ledgertypes.Credit, 90, ""),
	))
	if res.ErrCode != ledgertypes.ErrUnbalancedJournal {
		t.Fatalf("expected ErrUnbalancedJournal, got %q", res.ErrCode)
	}
}

// TestAssertJournal_MultiCurrency: each currency must balance independently;
// this is how FX conversions post through position accounts in one journal.
func TestAssertJournal_MultiCurrency(t *testing.T) {
	f := New("")
	createAccount(t, f, 1, "cust-usd", "USD")
	createAccount(t, f, 2, "cust-eur", "EUR")
	createUnlimitedAccount(t, f, 3, "fx-pos-usd", "USD")
	createUnlimitedAccount(t, f, 4, "fx-pos-eur", "EUR")
	fund(t, f, 5, "fx-pos-usd", "cust-usd", 10_000)

	res := mustApply(t, f, 6, journalCmd("fx-1",
		line("cust-usd", ledgertypes.Debit, 10_000, ""),
		line("fx-pos-usd", ledgertypes.Credit, 10_000, ""),
		line("fx-pos-eur", ledgertypes.Debit, 9_200, ""),
		line("cust-eur", ledgertypes.Credit, 9_200, ""),
	))
	if res.IsError() {
		t.Fatalf("cross-currency journal failed: %s: %s", res.ErrCode, res.ErrMsg)
	}

	// Same shape but EUR legs don't balance -> rejected even though USD does.
	res = mustApply(t, f, 7, journalCmd("fx-bad",
		line("cust-usd", ledgertypes.Debit, 100, ""),
		line("fx-pos-usd", ledgertypes.Credit, 100, ""),
		line("fx-pos-eur", ledgertypes.Debit, 92, ""),
		line("cust-eur", ledgertypes.Credit, 90, ""),
	))
	if res.ErrCode != ledgertypes.ErrUnbalancedJournal {
		t.Fatalf("expected ErrUnbalancedJournal, got %q", res.ErrCode)
	}
}

func TestDedupFingerprint_ConflictOnDifferentParams(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external", "alice", 1_000)

	first := mustApply(t, f, 5, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "k1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD",
	}})
	if first.IsError() {
		t.Fatalf("first apply failed: %s", first.ErrMsg)
	}

	// Same key, different amount: must be a conflict, not a silent replay.
	conflict := mustApply(t, f, 6, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "k1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 200, Currency: "USD",
	}})
	if conflict.ErrCode != ledgertypes.ErrConflict {
		t.Fatalf("expected ErrConflict, got %q", conflict.ErrCode)
	}

	// Same key, same params: duplicate replay of the original result.
	dup := mustApply(t, f, 7, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "k1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD",
	}})
	if dup.IsError() || dup.Transfer.Status != ledgertypes.TransferStatusDuplicate {
		t.Fatalf("expected duplicate, got %+v", dup)
	}
	alice, _ := f.Account("alice")
	if alice.BalanceMinor != 900 {
		t.Fatalf("alice = %d, want 900 (exactly one transfer)", alice.BalanceMinor)
	}
}

func TestReverse_RestoresBalancesAndLinks(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external", "alice", 1_000)

	orig := mustApply(t, f, 5, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "t1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 400, Currency: "USD",
	}})
	if orig.IsError() {
		t.Fatalf("transfer failed: %s", orig.ErrMsg)
	}

	rev := mustApply(t, f, 6, Command{Type: CmdReverse, Reverse: &ledgertypes.ReverseRequest{
		DedupID: "r1", OriginalDedupID: "t1",
	}})
	if rev.IsError() {
		t.Fatalf("reversal failed: %s: %s", rev.ErrCode, rev.ErrMsg)
	}
	alice, _ := f.Account("alice")
	bob, _ := f.Account("bob")
	if alice.BalanceMinor != 1_000 || bob.BalanceMinor != 0 {
		t.Fatalf("reversal didn't restore balances: alice=%d bob=%d", alice.BalanceMinor, bob.BalanceMinor)
	}

	_, backlog, cancel := f.Events().Subscribe(6)
	defer cancel()
	if len(backlog) != 1 || backlog[0].ReversalOf != "t1" {
		t.Fatalf("reversal event should link to original entry's dedup id: %+v", backlog)
	}

	again := mustApply(t, f, 7, Command{Type: CmdReverse, Reverse: &ledgertypes.ReverseRequest{
		DedupID: "r2", OriginalDedupID: "t1",
	}})
	if again.ErrCode != ledgertypes.ErrAlreadyReversed {
		t.Fatalf("expected ErrAlreadyReversed, got %q", again.ErrCode)
	}
}

func TestPending_CreatePostVoid(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external", "alice", 1_000)

	hold := mustApply(t, f, 5, Command{Type: CmdCreatePending, CreatePending: &ledgertypes.PendingTransferRequest{
		DedupID: "h1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD", TimeoutSeconds: 3600,
	}})
	if hold.IsError() {
		t.Fatalf("hold creation failed: %s: %s", hold.ErrCode, hold.ErrMsg)
	}
	alice, _ := f.Account("alice")
	if alice.BalanceMinor != 1_000 || alice.PendingDebitsMinor != 600 || alice.AvailableMinor != 400 {
		t.Fatalf("hold should reserve without posting: %+v", alice)
	}

	// A second debit may only draw against available (400), not posted (1000).
	res := mustApply(t, f, 6, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "over", FromAccount: "alice", ToAccount: "bob", AmountMinor: 500, Currency: "USD",
	}})
	if res.ErrCode != ledgertypes.ErrInsufficientFunds {
		t.Fatalf("expected insufficient available funds, got %q", res.ErrCode)
	}

	post := mustApply(t, f, 7, Command{Type: CmdPostPending, PendingID: hold.Pending.PendingID,
		PendingAction: &ledgertypes.PendingActionRequest{DedupID: "p1"}})
	if post.IsError() {
		t.Fatalf("post failed: %s: %s", post.ErrCode, post.ErrMsg)
	}
	alice, _ = f.Account("alice")
	bob, _ := f.Account("bob")
	if alice.BalanceMinor != 400 || alice.PendingDebitsMinor != 0 || bob.BalanceMinor != 600 {
		t.Fatalf("post wrong: alice=%+v bob=%d", alice, bob.BalanceMinor)
	}

	// Retrying the post is a duplicate, not a double-post.
	dup := mustApply(t, f, 8, Command{Type: CmdPostPending, PendingID: hold.Pending.PendingID,
		PendingAction: &ledgertypes.PendingActionRequest{DedupID: "p1"}})
	if dup.IsError() || dup.Transfer.Status != ledgertypes.TransferStatusDuplicate {
		t.Fatalf("post retry should be duplicate, got %+v", dup)
	}

	// Void path on a fresh hold.
	hold2 := mustApply(t, f, 9, Command{Type: CmdCreatePending, CreatePending: &ledgertypes.PendingTransferRequest{
		DedupID: "h2", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100, Currency: "USD", TimeoutSeconds: 3600,
	}})
	if hold2.IsError() {
		t.Fatalf("hold2 failed: %s", hold2.ErrMsg)
	}
	void := mustApply(t, f, 10, Command{Type: CmdVoidPending, PendingID: hold2.Pending.PendingID,
		PendingAction: &ledgertypes.PendingActionRequest{DedupID: "v1"}})
	if void.IsError() {
		t.Fatalf("void failed: %s", void.ErrMsg)
	}
	alice, _ = f.Account("alice")
	if alice.PendingDebitsMinor != 0 || alice.AvailableMinor != 400 {
		t.Fatalf("void should release the hold: %+v", alice)
	}

	// Posting a voided hold fails.
	res = mustApply(t, f, 11, Command{Type: CmdPostPending, PendingID: hold2.Pending.PendingID,
		PendingAction: &ledgertypes.PendingActionRequest{DedupID: "p2"}})
	if res.ErrCode != ledgertypes.ErrPendingNotFound {
		t.Fatalf("expected ErrPendingNotFound, got %q", res.ErrCode)
	}
}

// TestPending_DeterministicExpiry: expiry is evaluated against the
// log-carried clock during Apply, and released holds ride on the triggering
// entry's event.
func TestPending_DeterministicExpiry(t *testing.T) {
	f := New("")
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "alice", "USD")
	createAccount(t, f, 3, "bob", "USD")
	fund(t, f, 4, "external", "alice", 1_000)

	// Index 5 -> clock base+5s; timeout 10s -> expires at base+15s.
	hold := mustApply(t, f, 5, Command{Type: CmdCreatePending, CreatePending: &ledgertypes.PendingTransferRequest{
		DedupID: "h-exp", FromAccount: "alice", ToAccount: "bob", AmountMinor: 600, Currency: "USD", TimeoutSeconds: 10,
	}})
	if hold.IsError() {
		t.Fatalf("hold failed: %s", hold.ErrMsg)
	}

	// Index 6 at base+20s: the sweep must release the hold before dispatch.
	res := mustApply(t, f, 6, Command{Type: CmdCreateAccount, Timestamp: testBase.Add(20 * time.Second),
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: "carol", Currency: "USD"}})
	if res.IsError() {
		t.Fatalf("unrelated command failed: %s", res.ErrMsg)
	}
	alice, _ := f.Account("alice")
	if alice.PendingDebitsMinor != 0 || alice.AvailableMinor != 1_000 {
		t.Fatalf("expired hold not released: %+v", alice)
	}

	_, backlog, cancel := f.Events().Subscribe(6)
	defer cancel()
	if len(backlog) != 1 || len(backlog[0].ExpiredPendings) != 1 {
		t.Fatalf("expiry should ride on index 6's event: %+v", backlog)
	}
	if backlog[0].ExpiredPendings[0].PendingID != hold.Pending.PendingID {
		t.Fatalf("wrong expired pending: %+v", backlog[0].ExpiredPendings[0])
	}

	// Posting the expired hold fails.
	res = mustApply(t, f, 7, Command{Type: CmdPostPending, PendingID: hold.Pending.PendingID,
		PendingAction: &ledgertypes.PendingActionRequest{DedupID: "late"}})
	if res.ErrCode != ledgertypes.ErrPendingNotFound {
		t.Fatalf("expected ErrPendingNotFound, got %q", res.ErrCode)
	}
}

// TestEventStream_OneEventPerLogEntry: rejected and duplicate commands also
// emit events, so raft indices are contiguous on the stream — the property
// consumer gap-detection relies on.
func TestEventStream_OneEventPerLogEntry(t *testing.T) {
	f := New("")
	createAccount(t, f, 1, "alice", "USD") // ok
	createAccount(t, f, 2, "alice", "USD") // rejected: exists
	mustApply(t, f, 3, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "x", FromAccount: "alice", ToAccount: "ghost", AmountMinor: 1, Currency: "USD",
	}}) // rejected: not found
	mustApply(t, f, 4, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "x", FromAccount: "alice", ToAccount: "ghost", AmountMinor: 1, Currency: "USD",
	}}) // rejected again (errors are not deduped)

	_, backlog, cancel := f.Events().Subscribe(0)
	defer cancel()
	if len(backlog) != 4 {
		t.Fatalf("expected 4 events for 4 log entries, got %d", len(backlog))
	}
	for i, ev := range backlog {
		if ev.RaftIndex != uint64(i+1) {
			t.Fatalf("event %d has index %d, want %d (contiguity broken)", i, ev.RaftIndex, i+1)
		}
		if ev.Timestamp.IsZero() {
			t.Fatalf("event %d missing replicated timestamp", i)
		}
	}
	if backlog[1].Kind != ledgertypes.EventCommandRejected || backlog[1].ErrCode != ledgertypes.ErrAccountExists {
		t.Fatalf("expected command_rejected/account_exists at index 2: %+v", backlog[1])
	}
}

func TestEventBus_DurableReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.ndjson")
	f := New(path)
	createUnlimitedAccount(t, f, 1, "external", "USD")
	createAccount(t, f, 2, "alice", "USD")
	fund(t, f, 3, "external", "alice", 500)
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a restart: history must come back from disk.
	f2 := New(path)
	defer f2.Close()
	_, backlog, cancel := f2.Events().Subscribe(0)
	defer cancel()
	if len(backlog) != 3 {
		t.Fatalf("expected 3 replayed events after restart, got %d", len(backlog))
	}

	// Raft replays the last entry after restart; it must not duplicate on
	// the bus. (f2 has no account state restored here, so the command itself
	// is rejected — the bus dedup is keyed on raft index regardless.)
	mustApply(t, f2, 3, Command{Type: CmdAssertTransfer, AssertTransfer: &ledgertypes.TransferRequest{
		DedupID: "seed-alice", FromAccount: "external", ToAccount: "alice", AmountMinor: 500, Currency: "USD",
	}})
	_, backlog2, cancel2 := f2.Events().Subscribe(0)
	defer cancel2()
	if len(backlog2) != 3 {
		t.Fatalf("replayed entry duplicated on the bus: %d events", len(backlog2))
	}

	// New entries continue the sequence.
	createAccount(t, f2, 4, "bob", "USD")
	_, backlog3, cancel3 := f2.Events().Subscribe(4)
	defer cancel3()
	if len(backlog3) != 1 || backlog3[0].AccountID != "bob" {
		t.Fatalf("post-restart publish broken: %+v", backlog3)
	}
}

func TestEventBus_KicksSlowSubscriber(t *testing.T) {
	f := New("")
	ch, _, cancel := f.Events().Subscribe(0)
	defer cancel()

	// Never read from ch: after the 256-slot buffer fills, the bus must
	// close the channel (forcing reconnect-from-watermark) instead of
	// silently dropping events.
	for i := uint64(1); i <= 300; i++ {
		createAccount(t, f, i, fmt.Sprintf("acct-%d", i), "USD")
	}
	select {
	case _, open := <-ch:
		if open {
			// Drain until closed; the kick must have happened by now.
			for range ch {
			}
		}
	default:
		t.Fatalf("expected the slow subscriber's channel to be closed")
	}
}
