package recon

import (
	"context"
	"net/http"
	"testing"

	"github.com/ledgerline/ledgerline-recon/internal/engineclient"
)

type stubEngine struct {
	accounts map[string]*engineclient.Account
	journals []engineclient.JournalRequest
}

func newStubEngine() *stubEngine {
	return &stubEngine{accounts: make(map[string]*engineclient.Account)}
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
	s.journals = append(s.journals, req)
	return &engineclient.TransferResult{Status: "applied"}, nil
}

func TestService_RunProducesBreaks(t *testing.T) {
	svc := NewService(newStubEngine())
	svc.IngestLedger([]Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}})
	svc.IngestExternal([]Record{{Ref: "t1", AmountMinor: 100, Currency: "USD"}, {Ref: "t2", AmountMinor: 200, Currency: "USD"}})
	breaks := svc.Run()
	if len(breaks) != 1 || breaks[0].Type != BreakMissingInLedger || breaks[0].Ref != "t2" {
		t.Errorf("expected missing_in_ledger t2, got %+v", breaks)
	}
	if len(svc.LastBreaks()) != 1 {
		t.Errorf("LastBreaks should retain run result")
	}
}

func TestService_AdjustPostsSuspenseJournal(t *testing.T) {
	eng := newStubEngine()
	svc := NewService(eng)
	b := Break{Type: BreakMissingInLedger, Ref: "t2", ExternalAmount: 200, Currency: "USD"}
	if err := svc.Adjust(context.Background(), b); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if len(eng.journals) != 1 {
		t.Fatalf("expected 1 adjustment journal, got %d", len(eng.journals))
	}
	j := eng.journals[0]
	if j.DedupID != "recon-adj-t2" {
		t.Errorf("dedup = %q, want recon-adj-t2", j.DedupID)
	}
	if j.Lines[0].AccountID != "recon-suspense" || j.Lines[0].AmountMinor != 200 || j.Lines[1].AccountID != "recon-clearing" {
		t.Errorf("adjustment legs wrong: %+v", j.Lines)
	}
}

func TestService_AdjustRejectsNonMissingType(t *testing.T) {
	svc := NewService(newStubEngine())
	err := svc.Adjust(context.Background(), Break{Type: BreakAmountMismatch, Ref: "x"})
	if err == nil {
		t.Fatal("expected error adjusting a non-missing break")
	}
}
