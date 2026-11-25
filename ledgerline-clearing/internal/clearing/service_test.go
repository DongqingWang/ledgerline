package clearing

import (
	"context"
	"net/http"
	"testing"

	"github.com/ledgerline/ledgerline-clearing/internal/engineclient"
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

func TestRunClearing_NetsAndRecordsJournal(t *testing.T) {
	eng := newStubEngine()
	eng.accounts["merchant-payable"] = &engineclient.Account{AccountID: "merchant-payable", Currency: "USD"}
	svc := NewService(eng)
	svc.Ingest(Capture{MerchantID: "m1", Currency: "USD", NetMinor: 9700, Ref: "t1"})
	svc.Ingest(Capture{MerchantID: "m1", Currency: "USD", NetMinor: 19400, Ref: "t2"})
	svc.Ingest(Capture{MerchantID: "m2", Currency: "USD", NetMinor: 4850, Ref: "t3"})

	batch, err := svc.RunClearing(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(batch.Positions) != 2 {
		t.Fatalf("expected 2 net positions, got %d: %+v", len(batch.Positions), batch.Positions)
	}
	if batch.Positions[0].MerchantID != "m1" || batch.Positions[0].NetMinor != 29100 {
		t.Errorf("m1 net = %+v, want 29100", batch.Positions[0])
	}
	if batch.Count != 3 {
		t.Errorf("count = %d, want 3 captures", batch.Count)
	}
	if len(eng.journals) != 1 {
		t.Fatalf("expected 1 clearing journal, got %d", len(eng.journals))
	}
	j := eng.journals[0]
	// Dr payable 33950 / Cr suspense 33950 (29100 + 4850)
	if j.Lines[0].AccountID != "merchant-payable" || j.Lines[0].Direction != "debit" || j.Lines[0].AmountMinor != 33950 {
		t.Errorf("debit leg wrong: %+v", j.Lines[0])
	}
	if j.Lines[1].AccountID != "clearing-suspense" || j.Lines[1].Direction != "credit" || j.Lines[1].AmountMinor != 33950 {
		t.Errorf("credit leg wrong: %+v", j.Lines[1])
	}
}

func TestRunClearing_PerCurrencyJournals(t *testing.T) {
	eng := newStubEngine()
	eng.accounts["merchant-payable"] = &engineclient.Account{AccountID: "merchant-payable", Currency: "USD"}
	svc := NewService(eng)
	svc.Ingest(Capture{MerchantID: "m1", Currency: "USD", NetMinor: 100})
	svc.Ingest(Capture{MerchantID: "m1", Currency: "EUR", NetMinor: 200})
	if _, err := svc.RunClearing(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(eng.journals) != 2 {
		t.Errorf("expected 2 per-currency journals, got %d", len(eng.journals))
	}
}

func TestRunClearing_EmptyNoJournal(t *testing.T) {
	eng := newStubEngine()
	svc := NewService(eng)
	batch, err := svc.RunClearing(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if batch.Count != 0 || len(eng.journals) != 0 {
		t.Errorf("empty run should record nothing: batch=%+v journals=%d", batch, len(eng.journals))
	}
}

func TestRunClearing_ClearsPending(t *testing.T) {
	eng := newStubEngine()
	eng.accounts["merchant-payable"] = &engineclient.Account{AccountID: "merchant-payable", Currency: "USD"}
	svc := NewService(eng)
	svc.Ingest(Capture{MerchantID: "m1", Currency: "USD", NetMinor: 100})
	svc.RunClearing(context.Background())
	if len(svc.Pending()) != 0 {
		t.Errorf("pending should be cleared after run: %+v", svc.Pending())
	}
	// second run with no new captures records no journal
	before := len(eng.journals)
	svc.RunClearing(context.Background())
	if len(eng.journals) != before {
		t.Errorf("empty second run added journals")
	}
}
