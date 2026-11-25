package fsm

import (
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

func setupSettlementAccounts(t *testing.T, f *FSM, userBalance int64) {
	t.Helper()
	createUnlimitedAccount(t, f, 1, "bank-clearing", "USD")
	createAccount(t, f, 2, "user", "USD")
	if userBalance > 0 {
		fund(t, f, 3, "bank-clearing", "user", userBalance)
	}
}

func createSettlement(t *testing.T, f *FSM, index uint64, dedupID string, kind ledgertypes.SettlementKind, amount, timeout int64) *ApplyResult {
	t.Helper()
	return mustApply(t, f, index, Command{
		Type: CmdCreateSettlement,
		CreateSettlement: &ledgertypes.CreateSettlementRequest{
			DedupID: dedupID, Kind: kind, AccountID: "user", FundingAccount: "bank-clearing",
			AmountMinor: amount, Currency: "USD", TimeoutSeconds: timeout,
		},
	})
}

func markProcessing(t *testing.T, f *FSM, index uint64, dedupID, settlementID string) *ApplyResult {
	t.Helper()
	return mustApply(t, f, index, Command{
		Type: CmdMarkSettlementProcessing, SettlementID: settlementID,
		SettlementAction: &ledgertypes.SettlementActionRequest{DedupID: dedupID},
	})
}

func settleOutcome(t *testing.T, f *FSM, index uint64, dedupID, settlementID, outcome string) *ApplyResult {
	t.Helper()
	return mustApply(t, f, index, Command{
		Type: CmdSettleOutcome, SettlementID: settlementID,
		SettlementAction: &ledgertypes.SettlementActionRequest{DedupID: dedupID, Outcome: outcome},
	})
}

func TestSettlement_WithdrawalLifecycle(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)

	res := createSettlement(t, f, 4, "w1", ledgertypes.SettlementWithdrawal, 300, 3600)
	if res.IsError() {
		t.Fatalf("create: %s", res.ErrMsg)
	}
	sid := res.Settlement.SettlementID
	if res.Settlement.Status != ledgertypes.SettlementPending {
		t.Errorf("status = %s, want pending", res.Settlement.Status)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 1000 || u.PendingDebitsMinor != 0 {
		t.Fatalf("optimistic create should not move balance: %+v", u)
	}

	// mark-processing posts optimistically.
	markProcessing(t, f, 5, "w1-proc", sid)
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementProcessing {
		t.Errorf("status = %s, want processing", s.Status)
	}
	u, _ = f.Account("user")
	if u.BalanceMinor != 700 || u.PendingDebitsMinor != 0 {
		t.Fatalf("mark-processing should post (700/0): %+v", u)
	}

	settleOutcome(t, f, 6, "w1-settle", sid, "settled")
	s, _ = f.Settlement(sid)
	if s.Status != ledgertypes.SettlementPosted {
		t.Errorf("status = %s, want posted", s.Status)
	}
	u, _ = f.Account("user")
	bc, _ := f.Account("bank-clearing")
	if u.BalanceMinor != 700 {
		t.Errorf("user after settle = %d, want 700", u.BalanceMinor)
	}
	if bc.BalanceMinor != -700 {
		t.Errorf("bank-clearing = %d, want -700", bc.BalanceMinor)
	}
}

func TestSettlement_WithdrawalFailedReversesPost(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w2", ledgertypes.SettlementWithdrawal, 300, 3600)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "w2-proc", sid) // posts: user 700
	settleOutcome(t, f, 6, "w2-settle", sid, "failed")

	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementVoided {
		t.Errorf("status = %s, want voided", s.Status)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 1000 || u.PendingDebitsMinor != 0 {
		t.Errorf("failed withdrawal must reverse to 1000: %+v", u)
	}
}

func TestSettlement_DepositLifecycle(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 0)
	res := createSettlement(t, f, 4, "d1", ledgertypes.SettlementDeposit, 300, 3600)
	sid := res.Settlement.SettlementID

	// pending deposit records intent but does NOT change balance.
	u, _ := f.Account("user")
	if u.BalanceMinor != 0 {
		t.Fatalf("pending deposit must not credit: %+v", u)
	}
	markProcessing(t, f, 5, "d1-proc", sid)
	u, _ = f.Account("user")
	if u.BalanceMinor != 0 {
		t.Fatalf("processing deposit still not credited: %+v", u)
	}

	settleOutcome(t, f, 6, "d1-settle", sid, "settled")
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementPosted {
		t.Errorf("status = %s, want posted", s.Status)
	}
	u, _ = f.Account("user")
	if u.BalanceMinor != 300 {
		t.Errorf("user after deposit settle = %d, want 300", u.BalanceMinor)
	}
}

func TestSettlement_DepositFailedNoEntry(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 0)
	res := createSettlement(t, f, 4, "d2", ledgertypes.SettlementDeposit, 300, 3600)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "d2-proc", sid)
	settleOutcome(t, f, 6, "d2-settle", sid, "failed")

	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementVoided {
		t.Errorf("status = %s, want voided", s.Status)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 0 {
		t.Errorf("failed deposit must leave balance 0: %+v", u)
	}
}

func TestSettlement_WithdrawalInsufficientFunds(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 100)
	res := createSettlement(t, f, 4, "w3", ledgertypes.SettlementWithdrawal, 300, 3600)
	if res.ErrCode != ledgertypes.ErrInsufficientFunds {
		t.Fatalf("expected insufficient_funds, got %q", res.ErrCode)
	}
	u, _ := f.Account("user")
	if u.PendingDebitsMinor != 0 {
		t.Errorf("rejected create must not reserve: %+v", u)
	}
}

func TestSettlement_PendingWithdrawalExpiry(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	// created at index 4 (base+4s), timeout 10s -> ExpiresAt = base+14s.
	// Never submitted (stays pending), so it expires with no balance effect.
	res := createSettlement(t, f, 4, "w4", ledgertypes.SettlementWithdrawal, 300, 10)
	sid := res.Settlement.SettlementID

	mustApply(t, f, 5, Command{
		Type: CmdCreateAccount, Timestamp: testBase.Add(20 * time.Second),
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: "other", Currency: "USD"},
	})
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementExpired {
		t.Errorf("status = %s, want expired", s.Status)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 1000 || u.PendingDebitsMinor != 0 {
		t.Errorf("pending expiry has no balance effect: %+v", u)
	}
}

func TestSettlement_ProcessingWithdrawalNotSwept(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w4b", ledgertypes.SettlementWithdrawal, 300, 10)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "w4b-proc", sid) // optimistically posted: user 700

	// Far-future command must NOT expire an optimistically-posted withdrawal.
	mustApply(t, f, 6, Command{
		Type: CmdCreateAccount, Timestamp: testBase.Add(10000 * time.Second),
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: "other", Currency: "USD"},
	})
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementProcessing {
		t.Errorf("optimistically-posted withdrawal must not be swept: %s", s.Status)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 700 {
		t.Errorf("posted balance must stay debited: %+v", u)
	}
}

func TestSettlement_DepositLateSettledAfterExpiry(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 0)
	res := createSettlement(t, f, 4, "d-late", ledgertypes.SettlementDeposit, 300, 10)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "d-late-proc", sid)

	// Expire the pending deposit (no balance effect).
	mustApply(t, f, 6, Command{
		Type: CmdCreateAccount, Timestamp: testBase.Add(20 * time.Second),
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: "other", Currency: "USD"},
	})
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementExpired {
		t.Fatalf("status = %s, want expired", s.Status)
	}
	if u, _ := f.Account("user"); u.BalanceMinor != 0 {
		t.Fatalf("expired deposit not credited: %+v", u)
	}

	// Late rail confirmation: money really arrived, so credit even though expired.
	late := settleOutcome(t, f, 7, "d-late-settle", sid, "settled")
	if late.IsError() {
		t.Fatalf("late settled should be tolerated: %s", late.ErrMsg)
	}
	s, _ = f.Settlement(sid)
	if s.Status != ledgertypes.SettlementPosted {
		t.Errorf("status = %s, want posted after late settle", s.Status)
	}
	if u, _ := f.Account("user"); u.BalanceMinor != 300 {
		t.Errorf("late settle must credit 300: %+v", u)
	}
}

func TestSettlement_PostedNotSwept(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w5", ledgertypes.SettlementWithdrawal, 300, 10)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "w5-proc", sid)
	settleOutcome(t, f, 6, "w5-settle", sid, "settled")

	// Far-future command must NOT expire a posted settlement.
	mustApply(t, f, 7, Command{
		Type: CmdCreateAccount, Timestamp: testBase.Add(10000 * time.Second),
		CreateAccount: &ledgertypes.CreateAccountRequest{AccountID: "other", Currency: "USD"},
	})
	s, _ := f.Settlement(sid)
	if s.Status != ledgertypes.SettlementPosted {
		t.Errorf("posted settlement must not be swept: %s", s.Status)
	}
}

func TestSettlement_IdempotentCreate(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	a := createSettlement(t, f, 4, "dup", ledgertypes.SettlementWithdrawal, 300, 3600)
	b := createSettlement(t, f, 5, "dup", ledgertypes.SettlementWithdrawal, 300, 3600)
	if a.Settlement.SettlementID != b.Settlement.SettlementID {
		t.Errorf("same dedup must return same settlement")
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 1000 || u.PendingDebitsMinor != 0 {
		t.Errorf("duplicate create must not post twice: %+v", u)
	}
}

func TestSettlement_ReversalOfPostedWithdrawal(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w6", ledgertypes.SettlementWithdrawal, 300, 3600)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "w6-proc", sid)
	settleOutcome(t, f, 6, "w6-settle", sid, "settled")
	s, _ := f.Settlement(sid)

	// Delayed return: reverse the posted entry via the ordinary primitive.
	rev := mustApply(t, f, 7, Command{
		Type: CmdReverse, Reverse: &ledgertypes.ReverseRequest{DedupID: "w6-rev", OriginalDedupID: s.JournalDedupID},
	})
	if rev.IsError() {
		t.Fatalf("reversal failed: %s", rev.ErrMsg)
	}
	u, _ := f.Account("user")
	if u.BalanceMinor != 1000 {
		t.Errorf("reversal must restore user to 1000: %+v", u)
	}
}

func TestSettlement_IllegalTransition(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w7", ledgertypes.SettlementWithdrawal, 300, 3600)
	sid := res.Settlement.SettlementID
	// settle-outcome on a pending (not processing) settlement is illegal.
	bad := settleOutcome(t, f, 5, "w7-settle", sid, "settled")
	if bad.ErrCode != ledgertypes.ErrInvalidRequest {
		t.Errorf("expected invalid_request, got %q", bad.ErrCode)
	}
}

func TestSettlement_DurableAcrossSnapshot(t *testing.T) {
	f := New("")
	setupSettlementAccounts(t, f, 1000)
	res := createSettlement(t, f, 4, "w8", ledgertypes.SettlementWithdrawal, 300, 3600)
	sid := res.Settlement.SettlementID
	markProcessing(t, f, 5, "w8-proc", sid)

	snap, err := f.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	sink := newMemSnapshotSink()
	if err := snap.Persist(sink); err != nil {
		t.Fatalf("persist: %v", err)
	}
	restored := New("")
	if err := restored.Restore(sink.reader()); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// In-flight settlement survives — the durability win of fat-ledger.
	s, ok := restored.Settlement(sid)
	if !ok || s.Status != ledgertypes.SettlementProcessing {
		t.Fatalf("in-flight settlement lost on restore: %+v ok=%v", s, ok)
	}
	u, _ := restored.Account("user")
	if u.BalanceMinor != 700 {
		t.Errorf("optimistically-posted balance lost on restore: %+v", u)
	}
}
