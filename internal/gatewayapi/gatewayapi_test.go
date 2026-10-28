package gatewayapi

import (
	"errors"
	"testing"

	"github.com/ledgerline/ledgerline-gateway/internal/accountingclient"
	"github.com/ledgerline/ledgerline-gateway/internal/ledgerclient"
)

// stubAccounting is a test double for AccountingClient.
type stubAccounting struct {
	journal *accountingclient.Journal
	err     error
	lastReq accountingclient.ComposeRequest
}

func (s *stubAccounting) Compose(req accountingclient.ComposeRequest) (*accountingclient.Journal, error) {
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.journal, nil
}

// stubLedger is a test double for LedgerClient that records every transfer
// request it receives.
type stubLedger struct {
	calls   []ledgerclient.TransferRequest
	results []*ledgerclient.TransferResult // one per call, in order; defaults to "applied" if nil
	err     error
}

func (s *stubLedger) Transfer(req ledgerclient.TransferRequest) (*ledgerclient.TransferResult, error) {
	s.calls = append(s.calls, req)
	if s.err != nil {
		return nil, s.err
	}
	if len(s.results) >= len(s.calls) {
		return s.results[len(s.calls)-1], nil
	}
	return &ledgerclient.TransferResult{Status: "applied", JournalID: "jrn-x", RaftIndex: int64(len(s.calls))}, nil
}

func (s *stubLedger) GetAccount(accountID string) (*ledgerclient.Account, error) {
	return &ledgerclient.Account{AccountID: accountID}, nil
}

func noFeeJournal() *accountingclient.Journal {
	return &accountingclient.Journal{
		Lines: []accountingclient.JournalLine{
			{AccountID: "platform", Direction: "debit", AmountMinor: 10000, GLAccount: "2100", CostCenter: "CC-PAYOUT-US"},
			{AccountID: "seller-1", Direction: "credit", AmountMinor: 10000, GLAccount: "4000", CostCenter: "CC-REVENUE-US"},
		},
	}
}

func feeJournal() *accountingclient.Journal {
	return &accountingclient.Journal{
		Lines: []accountingclient.JournalLine{
			{AccountID: "platform", Direction: "debit", AmountMinor: 10000, GLAccount: "2100", CostCenter: "CC-PAYOUT-US"},
			{AccountID: "seller-1", Direction: "credit", AmountMinor: 9700, GLAccount: "4000", CostCenter: "CC-REVENUE-US"},
			{AccountID: FeeRevenueAccount, Direction: "credit", AmountMinor: 300, GLAccount: "4900", CostCenter: "CC-FEES-US"},
		},
	}
}

func TestBuildLedgerTransfers_NoFee(t *testing.T) {
	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 0, Currency: "USD",
	}
	transfers, err := BuildLedgerTransfers(req, noFeeJournal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transfers) != 1 {
		t.Fatalf("expected 1 transfer, got %d: %+v", len(transfers), transfers)
	}
	tr := transfers[0]
	if tr.DedupID != "txn-1" {
		t.Errorf("dedup_id = %q, want %q", tr.DedupID, "txn-1")
	}
	if tr.AmountMinor != 10000 {
		t.Errorf("amount_minor = %d, want 10000", tr.AmountMinor)
	}
	if tr.GLAccount != "2100" || tr.CostCenter != "CC-PAYOUT-US" {
		t.Errorf("gl/cc = %q/%q, want 2100/CC-PAYOUT-US", tr.GLAccount, tr.CostCenter)
	}
}

func TestBuildLedgerTransfers_WithFee(t *testing.T) {
	req := TransferRequest{
		DedupID: "txn-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD",
	}
	transfers, err := BuildLedgerTransfers(req, feeJournal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(transfers) != 2 {
		t.Fatalf("expected 2 transfers, got %d: %+v", len(transfers), transfers)
	}

	net := transfers[0]
	if net.DedupID != "txn-2-net" {
		t.Errorf("net dedup_id = %q, want txn-2-net", net.DedupID)
	}
	if net.AmountMinor != 9700 {
		t.Errorf("net amount_minor = %d, want 9700", net.AmountMinor)
	}
	if net.ToAccount != "seller-1" {
		t.Errorf("net to_account = %q, want seller-1", net.ToAccount)
	}
	if net.GLAccount != "4000" || net.CostCenter != "CC-REVENUE-US" {
		t.Errorf("net gl/cc = %q/%q, want 4000/CC-REVENUE-US", net.GLAccount, net.CostCenter)
	}

	fee := transfers[1]
	if fee.DedupID != "txn-2-fee" {
		t.Errorf("fee dedup_id = %q, want txn-2-fee", fee.DedupID)
	}
	if fee.AmountMinor != 300 {
		t.Errorf("fee amount_minor = %d, want 300", fee.AmountMinor)
	}
	if fee.ToAccount != FeeRevenueAccount {
		t.Errorf("fee to_account = %q, want %q", fee.ToAccount, FeeRevenueAccount)
	}
	if fee.GLAccount != "4900" || fee.CostCenter != "CC-FEES-US" {
		t.Errorf("fee gl/cc = %q/%q, want 4900/CC-FEES-US", fee.GLAccount, fee.CostCenter)
	}
}

func TestBuildLedgerTransfers_MissingFeeLine(t *testing.T) {
	req := TransferRequest{
		DedupID: "txn-3", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD",
	}
	// A journal with a fee requested but no fee-revenue credit line present.
	_, err := BuildLedgerTransfers(req, noFeeJournal())
	if err == nil {
		t.Fatal("expected error for missing fee credit line, got nil")
	}
}

func TestProcessTransfer_NoFee_OneLedgerCall(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{}

	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 0, Currency: "USD", Category: "seller_payout", Country: "US",
	}
	resp, err := ProcessTransfer(acct, ledger, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.calls) != 1 {
		t.Fatalf("expected 1 ledger call, got %d", len(ledger.calls))
	}
	if resp.Status != "applied" {
		t.Errorf("status = %q, want applied", resp.Status)
	}
	if len(resp.LedgerResults) != 1 {
		t.Errorf("expected 1 ledger result, got %d", len(resp.LedgerResults))
	}
	if acct.lastReq.AmountMinor != 10000 || acct.lastReq.FeeMinor != 0 {
		t.Errorf("unexpected compose request: %+v", acct.lastReq)
	}
}

func TestProcessTransfer_WithFee_TwoLedgerCalls(t *testing.T) {
	acct := &stubAccounting{journal: feeJournal()}
	ledger := &stubLedger{}

	req := TransferRequest{
		DedupID: "txn-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD", Category: "seller_payout", Country: "US",
	}
	resp, err := ProcessTransfer(acct, ledger, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.calls) != 2 {
		t.Fatalf("expected 2 ledger calls, got %d", len(ledger.calls))
	}
	if ledger.calls[0].DedupID != "txn-2-net" || ledger.calls[0].AmountMinor != 9700 {
		t.Errorf("unexpected net call: %+v", ledger.calls[0])
	}
	if ledger.calls[1].DedupID != "txn-2-fee" || ledger.calls[1].AmountMinor != 300 {
		t.Errorf("unexpected fee call: %+v", ledger.calls[1])
	}
	if len(resp.LedgerResults) != 2 {
		t.Errorf("expected 2 ledger results, got %d", len(resp.LedgerResults))
	}
}

func TestProcessTransfer_DuplicateStatusPropagates(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{
		results: []*ledgerclient.TransferResult{
			{Status: "duplicate", JournalID: "jrn-1", RaftIndex: 1},
		},
	}
	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 0, Currency: "USD",
	}
	resp, err := ProcessTransfer(acct, ledger, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Status != "duplicate" {
		t.Errorf("status = %q, want duplicate", resp.Status)
	}
}

func TestProcessTransfer_AccountingErrorPropagates(t *testing.T) {
	acct := &stubAccounting{err: &accountingclient.Error{StatusCode: 400, Message: "bad category"}}
	ledger := &stubLedger{}
	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD",
	}
	_, err := ProcessTransfer(acct, ledger, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *accountingclient.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *accountingclient.Error, got %T: %v", err, err)
	}
	if len(ledger.calls) != 0 {
		t.Errorf("expected no ledger calls when accounting fails, got %d", len(ledger.calls))
	}
}
