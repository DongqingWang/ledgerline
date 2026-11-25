package settlement

import (
	"context"
	"net/http"
	"testing"

	"github.com/ledgerline/ledgerline-settlement/internal/engineclient"
)

// stubEngine models the FSM settlement state machine minimally so driver
// tests can assert command sequence, idempotency, and balance effects.
type stubEngine struct {
	calls       []string
	accounts    map[string]*engineclient.Account
	settlements map[string]*engineclient.Settlement
	byDedup     map[string]*engineclient.Settlement
	seq         int
}

func newStubEngine() *stubEngine {
	return &stubEngine{
		accounts:    make(map[string]*engineclient.Account),
		settlements: make(map[string]*engineclient.Settlement),
		byDedup:     make(map[string]*engineclient.Settlement),
	}
}

func (s *stubEngine) CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error) {
	s.calls = append(s.calls, "CreateAccount:"+req.AccountID)
	a := &engineclient.Account{AccountID: req.AccountID, Currency: req.Currency, Unlimited: req.Unlimited}
	s.accounts[req.AccountID] = a
	return a, nil
}

func (s *stubEngine) GetAccount(ctx context.Context, id string) (*engineclient.Account, error) {
	if a, ok := s.accounts[id]; ok {
		return a, nil
	}
	return nil, &engineclient.Error{StatusCode: http.StatusNotFound, Code: "account_not_found"}
}

func (s *stubEngine) CreateSettlement(ctx context.Context, req engineclient.CreateSettlementRequest) (*engineclient.Settlement, error) {
	s.calls = append(s.calls, "CreateSettlement:"+req.DedupID)
	if existing, ok := s.byDedup[req.DedupID]; ok {
		return existing, nil
	}
	u := s.accounts[req.AccountID]
	if req.Kind == engineclient.SettlementWithdrawal {
		if !u.Unlimited && u.AvailableMinor < req.AmountMinor {
			return nil, &engineclient.Error{StatusCode: 422, Code: "insufficient_funds"}
		}
		// optimistic post: no reservation at create
	}
	s.seq++
	stl := &engineclient.Settlement{
		SettlementID: "stl-test", DedupID: req.DedupID, Kind: req.Kind,
		AccountID: req.AccountID, FundingAccount: req.FundingAccount,
		AmountMinor: req.AmountMinor, Currency: req.Currency, Status: "pending",
	}
	// unique id per settlement
	stl.SettlementID = "stl-" + req.DedupID
	s.settlements[stl.SettlementID] = stl
	s.byDedup[req.DedupID] = stl
	return stl, nil
}

func (s *stubEngine) MarkSettlementProcessing(ctx context.Context, id string, req engineclient.SettlementActionRequest) (*engineclient.Settlement, error) {
	s.calls = append(s.calls, "MarkProcessing:"+id)
	stl := s.settlements[id]
	if stl.Kind == engineclient.SettlementWithdrawal {
		u, f := s.accounts[stl.AccountID], s.accounts[stl.FundingAccount]
		u.BalanceMinor -= stl.AmountMinor
		f.BalanceMinor += stl.AmountMinor
		u.AvailableMinor = u.BalanceMinor - u.PendingDebitsMinor
		stl.JournalDedupID = stl.DedupID + "-posted"
	}
	stl.Status = "processing"
	return stl, nil
}

func (s *stubEngine) SettleOutcome(ctx context.Context, id string, req engineclient.SettlementActionRequest) (*engineclient.Settlement, error) {
	s.calls = append(s.calls, "SettleOutcome:"+id+":"+req.Outcome)
	stl := s.settlements[id]
	u, f := s.accounts[stl.AccountID], s.accounts[stl.FundingAccount]
	if stl.Kind == engineclient.SettlementWithdrawal {
		// already posted at processing; settled=no-op, failed=reverse
		if req.Outcome == "failed" {
			u.BalanceMinor += stl.AmountMinor
			f.BalanceMinor -= stl.AmountMinor
			u.AvailableMinor = u.BalanceMinor - u.PendingDebitsMinor
			stl.Status = "voided"
		} else {
			stl.Status = "posted"
		}
		return stl, nil
	}
	if req.Outcome == "settled" {
		f.BalanceMinor -= stl.AmountMinor
		u.BalanceMinor += stl.AmountMinor
		u.AvailableMinor = u.BalanceMinor - u.PendingDebitsMinor
		stl.Status = "posted"
		stl.JournalDedupID = stl.DedupID + "-posted"
	} else {
		stl.Status = "voided"
	}
	return stl, nil
}

func (s *stubEngine) GetSettlement(ctx context.Context, id string) (*engineclient.Settlement, error) {
	if stl, ok := s.settlements[id]; ok {
		return stl, nil
	}
	return nil, &engineclient.Error{StatusCode: 404, Code: "settlement_not_found"}
}

func (s *stubEngine) Reverse(ctx context.Context, req engineclient.ReverseRequest) (*engineclient.TransferResult, error) {
	s.calls = append(s.calls, "Reverse:"+req.OriginalDedupID)
	return &engineclient.TransferResult{Status: "applied"}, nil
}

func (s *stubEngine) hasCall(prefix string) bool {
	for _, c := range s.calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

type stubRail struct{ submitted []string }

func (r *stubRail) Submit(id string, amount int64) { r.submitted = append(r.submitted, id) }

func newTestOrch() (*Orchestrator, *stubEngine, *stubRail) {
	eng := newStubEngine()
	r := &stubRail{}
	o := NewOrchestrator(eng, r)
	return o, eng, r
}

func fundUser(eng *stubEngine, id string, balance int64) {
	eng.accounts[id] = &engineclient.Account{AccountID: id, Currency: "USD", BalanceMinor: balance, AvailableMinor: balance}
}

func TestDriver_WithdrawalSubmittedThenSettled(t *testing.T) {
	o, eng, r := newTestOrch()
	fundUser(eng, "user", 1000)
	ctx := context.Background()

	stl, err := o.CreateWithdrawal(ctx, "w1", "user", 300, "USD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if stl.Status != "processing" || len(r.submitted) != 1 {
		t.Fatalf("expected processing + 1 rail submit, got %s / %d", stl.Status, len(r.submitted))
	}
	if !eng.hasCall("CreateSettlement:w1") || !eng.hasCall("MarkProcessing:") {
		t.Errorf("expected create+mark commands: %v", eng.calls)
	}
	if eng.accounts["user"].BalanceMinor != 700 || eng.accounts["user"].PendingDebitsMinor != 0 {
		t.Errorf("withdrawal optimistically posted at submission (700/0): %+v", eng.accounts["user"])
	}

	if err := o.OnRailResult(ctx, stl.SettlementID, "settled", ""); err != nil {
		t.Fatalf("settled: %v", err)
	}
	if eng.accounts["user"].BalanceMinor != 700 || eng.accounts["user"].PendingDebitsMinor != 0 {
		t.Errorf("after settle user stays 700/0: %+v", eng.accounts["user"])
	}
}

func TestDriver_WithdrawalFailedReleases(t *testing.T) {
	o, eng, _ := newTestOrch()
	fundUser(eng, "user", 1000)
	ctx := context.Background()
	stl, _ := o.CreateWithdrawal(ctx, "w2", "user", 300, "USD")
	o.OnRailResult(ctx, stl.SettlementID, "failed", "rejected")
	if eng.accounts["user"].BalanceMinor != 1000 || eng.accounts["user"].PendingDebitsMinor != 0 {
		t.Errorf("failed withdrawal intact: %+v", eng.accounts["user"])
	}
}

func TestDriver_DepositCreditedOnlyOnSettle(t *testing.T) {
	o, eng, _ := newTestOrch()
	fundUser(eng, "user", 0)
	ctx := context.Background()
	stl, _ := o.CreateDeposit(ctx, "d1", "user", 300, "USD")
	if eng.accounts["user"].BalanceMinor != 0 {
		t.Fatalf("pending deposit not credited: %+v", eng.accounts["user"])
	}
	o.OnRailResult(ctx, stl.SettlementID, "settled", "")
	if eng.accounts["user"].BalanceMinor != 300 {
		t.Errorf("after settle user 300: %+v", eng.accounts["user"])
	}
}

func TestDriver_DepositFailedNoCredit(t *testing.T) {
	o, eng, _ := newTestOrch()
	fundUser(eng, "user", 0)
	ctx := context.Background()
	stl, _ := o.CreateDeposit(ctx, "d2", "user", 300, "USD")
	o.OnRailResult(ctx, stl.SettlementID, "failed", "ACH R01")
	if eng.accounts["user"].BalanceMinor != 0 {
		t.Errorf("failed deposit no credit: %+v", eng.accounts["user"])
	}
}

func TestDriver_IdempotentCreateNoDoubleSubmit(t *testing.T) {
	o, eng, r := newTestOrch()
	fundUser(eng, "user", 1000)
	ctx := context.Background()
	a, _ := o.CreateWithdrawal(ctx, "dup", "user", 300, "USD")
	b, _ := o.CreateWithdrawal(ctx, "dup", "user", 300, "USD")
	if a.SettlementID != b.SettlementID {
		t.Errorf("same dedup same settlement")
	}
	if len(r.submitted) != 1 {
		t.Errorf("must submit once, got %d", len(r.submitted))
	}
	if eng.accounts["user"].BalanceMinor != 700 || eng.accounts["user"].PendingDebitsMinor != 0 {
		t.Errorf("no double post: %+v", eng.accounts["user"])
	}
}

func TestDriver_ReturnReversesPosted(t *testing.T) {
	o, eng, _ := newTestOrch()
	fundUser(eng, "user", 1000)
	ctx := context.Background()
	stl, _ := o.CreateWithdrawal(ctx, "w3", "user", 300, "USD")
	o.OnRailResult(ctx, stl.SettlementID, "settled", "")
	if _, err := o.ReturnSettlement(ctx, stl.SettlementID, "ACH R01"); err != nil {
		t.Fatalf("return: %v", err)
	}
	if !eng.hasCall("Reverse:w3-posted") {
		t.Errorf("expected reverse of posted: %v", eng.calls)
	}
}

func TestDriver_ReturnNonPostedRejected(t *testing.T) {
	o, eng, _ := newTestOrch()
	fundUser(eng, "user", 1000)
	ctx := context.Background()
	stl, _ := o.CreateWithdrawal(ctx, "w4", "user", 300, "USD")
	if _, err := o.ReturnSettlement(ctx, stl.SettlementID, "x"); err == nil {
		t.Fatal("expected error returning a non-posted settlement")
	}
}
