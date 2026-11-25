package acquiring

import (
	"context"
	"net/http"
	"testing"

	"github.com/ledgerline/ledgerline-acquiring/internal/engineclient"
	"github.com/ledgerline/ledgerline-acquiring/internal/scheme"
)

type stubEngine struct {
	accounts        map[string]*engineclient.Account
	journalCalls    []engineclient.JournalRequest
	journalByDedup  map[string]bool
	journalAttempts int
	journalFailN    int // fail the first N attempts with a transient 502
	reverseCalls    []string
}

func newStubEngine() *stubEngine {
	return &stubEngine{
		accounts:       make(map[string]*engineclient.Account),
		journalByDedup: make(map[string]bool),
	}
}

func (s *stubEngine) CreateAccount(ctx context.Context, req engineclient.CreateAccountRequest) (*engineclient.Account, error) {
	a := &engineclient.Account{AccountID: req.AccountID, Currency: req.Currency, Unlimited: req.Unlimited, Type: req.Type}
	s.accounts[req.AccountID] = a
	return a, nil
}

func (s *stubEngine) GetAccount(ctx context.Context, id string) (*engineclient.Account, error) {
	if a, ok := s.accounts[id]; ok {
		return a, nil
	}
	return nil, &engineclient.Error{StatusCode: http.StatusNotFound, Code: "account_not_found"}
}

func (s *stubEngine) AssertJournal(ctx context.Context, req engineclient.JournalRequest) (*engineclient.TransferResult, error) {
	s.journalAttempts++
	if s.journalAttempts <= s.journalFailN {
		return nil, &engineclient.Error{StatusCode: http.StatusBadGateway, Code: "internal", Message: "transient"}
	}
	if s.journalByDedup[req.DedupID] {
		return &engineclient.TransferResult{Status: "duplicate"}, nil
	}
	s.journalByDedup[req.DedupID] = true
	s.journalCalls = append(s.journalCalls, req)
	for _, ln := range req.Lines {
		a := s.accounts[ln.AccountID]
		if ln.Direction == "debit" {
			a.BalanceMinor -= ln.AmountMinor
		} else {
			a.BalanceMinor += ln.AmountMinor
		}
	}
	return &engineclient.TransferResult{Status: "applied"}, nil
}

func (s *stubEngine) Reverse(ctx context.Context, req engineclient.ReverseRequest) (*engineclient.TransferResult, error) {
	s.reverseCalls = append(s.reverseCalls, req.OriginalDedupID)
	return &engineclient.TransferResult{Status: "applied"}, nil
}

func newTestService(failAmounts map[int64]bool) (*Service, *stubEngine) {
	eng := newStubEngine()
	svc := NewService(eng, scheme.NewSimulated(failAmounts), NewStore(nil))
	return svc, eng
}

func TestPayment_HappyPath_RecordsBalancedJournal(t *testing.T) {
	svc, eng := newTestService(nil)
	pi, err := svc.CreatePayment(context.Background(), "p1", "merchant-1", 10000, 300, "USD")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if pi.Status != StatusCaptured {
		t.Fatalf("status = %s, want captured", pi.Status)
	}
	if len(eng.journalCalls) != 1 {
		t.Fatalf("expected 1 journal, got %d", len(eng.journalCalls))
	}
	lines := eng.journalCalls[0].Lines
	if len(lines) != 3 {
		t.Fatalf("expected 3 legs, got %d", len(lines))
	}
	// Dr receivable 10000 / Cr payable 9700 / Cr fee 300
	if lines[0].AccountID != "scheme-receivable" || lines[0].Direction != "debit" || lines[0].AmountMinor != 10000 {
		t.Errorf("receivable leg wrong: %+v", lines[0])
	}
	if lines[1].AccountID != "merchant-payable" || lines[1].AmountMinor != 9700 {
		t.Errorf("payable leg wrong: %+v", lines[1])
	}
	if lines[2].AccountID != "acquiring-fee-revenue" || lines[2].AmountMinor != 300 {
		t.Errorf("fee leg wrong: %+v", lines[2])
	}
	if eng.accounts["merchant-payable"].BalanceMinor != 9700 || eng.accounts["acquiring-fee-revenue"].BalanceMinor != 300 {
		t.Errorf("balances wrong: payable=%d fee=%d", eng.accounts["merchant-payable"].BalanceMinor, eng.accounts["acquiring-fee-revenue"].BalanceMinor)
	}
}

func TestPayment_AuthDeclined_FailsNoJournal(t *testing.T) {
	svc, eng := newTestService(map[int64]bool{10000: true})
	_, err := svc.CreatePayment(context.Background(), "p2", "m", 10000, 0, "USD")
	if err == nil {
		t.Fatal("expected auth decline error")
	}
	if len(eng.journalCalls) != 0 {
		t.Errorf("declined auth must not record journal: %d", len(eng.journalCalls))
	}
	pi, _ := svc.Store.Get("pi-1")
	if pi.Status != StatusFailed {
		t.Errorf("status = %s, want failed", pi.Status)
	}
}

func TestPayment_DualWrite_RetrySucceedsNoDouble(t *testing.T) {
	svc, eng := newTestService(nil)
	eng.journalFailN = 2 // first 2 attempts fail transiently
	pi, err := svc.CreatePayment(context.Background(), "p3", "m", 10000, 300, "USD")
	if err != nil {
		t.Fatalf("should retry to success: %v", err)
	}
	if pi.Status != StatusCaptured {
		t.Errorf("status = %s, want captured", pi.Status)
	}
	if len(eng.journalCalls) != 1 {
		t.Errorf("retries must record exactly once, got %d", len(eng.journalCalls))
	}
	if eng.journalAttempts != 3 {
		t.Errorf("expected 3 attempts (2 fail + 1 success), got %d", eng.journalAttempts)
	}
}

func TestPayment_DualWrite_CapturedButUnrecorded_ReDriveRecovers(t *testing.T) {
	svc, eng := newTestService(nil)
	eng.journalFailN = 100 // journal write keeps failing
	pi, err := svc.CreatePayment(context.Background(), "p4", "m", 10000, 0, "USD")
	if err == nil {
		t.Fatal("expected journal write failure")
	}
	// Capture happened, intent durably authorized (money in flight, not lost).
	if pi.Status != StatusAuthorized {
		t.Fatalf("status = %s, want authorized (capture done, journal pending)", pi.Status)
	}
	if len(eng.journalCalls) != 0 {
		t.Errorf("no journal recorded yet: %d", len(eng.journalCalls))
	}

	// Ledger recovers; re-driving the SAME intent resumes and records once.
	eng.journalFailN = 0
	pi2, err := svc.CreatePayment(context.Background(), "p4", "m", 10000, 0, "USD")
	if err != nil {
		t.Fatalf("re-drive should recover: %v", err)
	}
	if pi2.ID != pi.ID || pi2.Status != StatusCaptured {
		t.Errorf("re-drive must resume same intent to captured: %+v", pi2)
	}
	if len(eng.journalCalls) != 1 {
		t.Errorf("recovery must record exactly once, got %d", len(eng.journalCalls))
	}
}

func TestPayment_IdempotentCreate(t *testing.T) {
	svc, eng := newTestService(nil)
	a, _ := svc.CreatePayment(context.Background(), "dup", "m", 10000, 0, "USD")
	b, _ := svc.CreatePayment(context.Background(), "dup", "m", 10000, 0, "USD")
	if a.ID != b.ID {
		t.Errorf("same dedup same intent")
	}
	if len(eng.journalCalls) != 1 {
		t.Errorf("must not double-record: %d", len(eng.journalCalls))
	}
}

func TestPayment_VoidCapturedReverses(t *testing.T) {
	svc, eng := newTestService(nil)
	pi, _ := svc.CreatePayment(context.Background(), "p5", "m", 10000, 300, "USD")
	if _, err := svc.VoidPayment(context.Background(), pi.ID); err != nil {
		t.Fatalf("void: %v", err)
	}
	if len(eng.reverseCalls) != 1 || eng.reverseCalls[0] != "p5-journal" {
		t.Errorf("expected reverse of p5-journal: %v", eng.reverseCalls)
	}
	got, _ := svc.Store.Get(pi.ID)
	if got.Status != StatusVoided {
		t.Errorf("status = %s, want voided", got.Status)
	}
}

func TestPayment_ZeroFee_TwoLegs(t *testing.T) {
	svc, eng := newTestService(nil)
	svc.CreatePayment(context.Background(), "p6", "m", 10000, 0, "USD")
	if len(eng.journalCalls[0].Lines) != 2 {
		t.Errorf("zero fee should be 2 legs, got %d", len(eng.journalCalls[0].Lines))
	}
}
