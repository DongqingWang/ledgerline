package gatewayapi

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ledgerline/ledgerline-gateway/internal/accountingclient"
	"github.com/ledgerline/ledgerline-gateway/internal/fxclient"
	"github.com/ledgerline/ledgerline-gateway/internal/ledgerclient"
)

// stubAccounting is a test double for AccountingClient.
type stubAccounting struct {
	journal *accountingclient.Journal
	err     error
	lastReq accountingclient.ComposeRequest
	calls   int
}

func (s *stubAccounting) Compose(req accountingclient.ComposeRequest) (*accountingclient.Journal, error) {
	s.calls++
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.journal, nil
}

// stubLedger is a test double for LedgerClient that records every journal
// request and the tenant context it was made under.
type stubLedger struct {
	journalCalls []ledgerclient.JournalRequest
	callTenants  []string
	reverseCalls []ledgerclient.ReverseRequest
	created      []ledgerclient.CreateAccountRequest
	accounts     map[string]*ledgerclient.Account
	result       *ledgerclient.TransferResult // defaults to "applied" if nil
	err          error
}

func (s *stubLedger) Journal(ctx context.Context, req ledgerclient.JournalRequest) (*ledgerclient.TransferResult, error) {
	s.journalCalls = append(s.journalCalls, req)
	s.callTenants = append(s.callTenants, tenantFromContext(ctx))
	if s.err != nil {
		return nil, s.err
	}
	if s.result != nil {
		return s.result, nil
	}
	return &ledgerclient.TransferResult{Status: "applied", JournalID: "jrn-x", RaftIndex: int64(len(s.journalCalls))}, nil
}

func (s *stubLedger) Reverse(ctx context.Context, req ledgerclient.ReverseRequest) (*ledgerclient.TransferResult, error) {
	s.reverseCalls = append(s.reverseCalls, req)
	if s.err != nil {
		return nil, s.err
	}
	return &ledgerclient.TransferResult{Status: "applied", JournalID: "jrn-rev", RaftIndex: 1}, nil
}

func (s *stubLedger) GetAccount(ctx context.Context, accountID string) (*ledgerclient.Account, error) {
	if acct, ok := s.accounts[accountID]; ok {
		return acct, nil
	}
	return nil, &ledgerclient.Error{StatusCode: http.StatusNotFound, Code: "account_not_found", Message: "no such account"}
}

func (s *stubLedger) CreateAccount(ctx context.Context, req ledgerclient.CreateAccountRequest) (*ledgerclient.Account, error) {
	s.created = append(s.created, req)
	acct := &ledgerclient.Account{AccountID: req.AccountID, Currency: req.Currency, Unlimited: req.Unlimited, Type: req.Type}
	if s.accounts == nil {
		s.accounts = make(map[string]*ledgerclient.Account)
	}
	s.accounts[req.AccountID] = acct
	return acct, nil
}

// stubFX is a test double for FXClient.
type stubFX struct {
	converted int64
	err       error
	lastBase  string
	lastQuote string
	lastAmt   int64
}

func (s *stubFX) Convert(ctx context.Context, base, quote string, amountMinor int64) (*fxclient.ConvertResult, error) {
	s.lastBase, s.lastQuote, s.lastAmt = base, quote, amountMinor
	if s.err != nil {
		return nil, s.err
	}
	return &fxclient.ConvertResult{Base: base, Quote: quote, AmountMinor: amountMinor, ConvertedAmountMinor: s.converted, Rate: 0.92}, nil
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

func TestBuildLedgerJournal_NoFee(t *testing.T) {
	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 0, Currency: "USD",
	}
	jreq, err := BuildLedgerJournal(req, noFeeJournal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if jreq.DedupID != "txn-1" {
		t.Errorf("dedup_id = %q, want txn-1", jreq.DedupID)
	}
	if len(jreq.Lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %+v", len(jreq.Lines), jreq.Lines)
	}
	if jreq.Lines[0].GLAccount != "2100" || jreq.Lines[1].GLAccount != "4000" {
		t.Errorf("per-leg GL lost: %+v", jreq.Lines)
	}
}

func TestBuildLedgerJournal_FeeSplitKeepsPerLegGL(t *testing.T) {
	req := TransferRequest{
		DedupID: "txn-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD",
	}
	jreq, err := BuildLedgerJournal(req, feeJournal())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One journal, one dedup id — no more -net/-fee split.
	if jreq.DedupID != "txn-2" {
		t.Errorf("dedup_id = %q, want txn-2 (single atomic journal)", jreq.DedupID)
	}
	if len(jreq.Lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %+v", len(jreq.Lines), jreq.Lines)
	}
	wantGL := []string{"2100", "4000", "4900"}
	wantAmt := []int64{10000, 9700, 300}
	for i, l := range jreq.Lines {
		if l.GLAccount != wantGL[i] || l.AmountMinor != wantAmt[i] {
			t.Errorf("line[%d] = %+v, want GL %s amount %d", i, l, wantGL[i], wantAmt[i])
		}
	}
}

func TestProcessTransfer_FeePostsExactlyOneJournal(t *testing.T) {
	acct := &stubAccounting{journal: feeJournal()}
	ledger := &stubLedger{}

	req := TransferRequest{
		DedupID: "txn-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD", Category: "seller_payout", Country: "US",
	}
	resp, err := ProcessTransfer(context.Background(), acct, ledger, nil, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.journalCalls) != 1 {
		t.Fatalf("expected exactly 1 atomic journal call, got %d", len(ledger.journalCalls))
	}
	if len(ledger.journalCalls[0].Lines) != 3 {
		t.Errorf("expected 3 legs in the single journal, got %+v", ledger.journalCalls[0].Lines)
	}
	if resp.Status != "applied" || len(resp.LedgerResults) != 1 {
		t.Errorf("unexpected response: %+v", resp)
	}
}

func TestProcessTransfer_InsufficientFundsSurfacesWithNoPartialFollowup(t *testing.T) {
	// Regression for the old two-transfer bug: when the engine rejects the
	// journal for insufficient funds, the gateway must surface the error
	// and NOT issue any follow-up calls — the engine applied nothing, so
	// there is nothing to compensate.
	acct := &stubAccounting{journal: feeJournal()}
	ledger := &stubLedger{err: &ledgerclient.Error{StatusCode: http.StatusUnprocessableEntity, Code: "insufficient_funds", Message: "not enough balance"}}

	req := TransferRequest{
		DedupID: "txn-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD",
	}
	_, err := ProcessTransfer(context.Background(), acct, ledger, nil, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var le *ledgerclient.Error
	if !errors.As(err, &le) || le.Code != "insufficient_funds" {
		t.Fatalf("expected insufficient_funds ledger error, got %v", err)
	}
	if len(ledger.journalCalls) != 1 {
		t.Errorf("expected exactly 1 (rejected) journal call and no follow-ups, got %d", len(ledger.journalCalls))
	}
}

func TestProcessTransfer_DuplicateStatusPropagates(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{result: &ledgerclient.TransferResult{Status: "duplicate", JournalID: "jrn-1", RaftIndex: 1}}
	req := TransferRequest{
		DedupID: "txn-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD",
	}
	resp, err := ProcessTransfer(context.Background(), acct, ledger, nil, req)
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
	_, err := ProcessTransfer(context.Background(), acct, ledger, nil, req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ae *accountingclient.Error
	if !errors.As(err, &ae) {
		t.Fatalf("expected *accountingclient.Error, got %T: %v", err, err)
	}
	if len(ledger.journalCalls) != 0 {
		t.Errorf("expected no ledger calls when accounting fails, got %d", len(ledger.journalCalls))
	}
}

func TestProcessTransfer_FXPostsFourLegJournal(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{}
	fx := &stubFX{converted: 9200}

	req := TransferRequest{
		DedupID: "fx-1", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD", ToCurrency: "EUR",
	}
	resp, err := ProcessTransfer(context.Background(), acct, ledger, fx, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fx.lastBase != "USD" || fx.lastQuote != "EUR" || fx.lastAmt != 10000 {
		t.Errorf("unexpected convert call: %s->%s %d", fx.lastBase, fx.lastQuote, fx.lastAmt)
	}
	if len(ledger.journalCalls) != 1 {
		t.Fatalf("expected 1 journal call, got %d", len(ledger.journalCalls))
	}
	lines := ledger.journalCalls[0].Lines
	if len(lines) != 4 {
		t.Fatalf("expected 4 legs, got %d: %+v", len(lines), lines)
	}
	want := []ledgerclient.JournalLine{
		{AccountID: "platform", Direction: "debit", AmountMinor: 10000, GLAccount: "2100", CostCenter: "CC-PAYOUT-US"},
		{AccountID: "fx-position-USD", Direction: "credit", AmountMinor: 10000, GLAccount: "1030", CostCenter: "CC-FX"},
		{AccountID: "fx-position-EUR", Direction: "debit", AmountMinor: 9200, GLAccount: "1030", CostCenter: "CC-FX"},
		{AccountID: "seller-1", Direction: "credit", AmountMinor: 9200, GLAccount: "4000", CostCenter: "CC-REVENUE-US"},
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line[%d] = %+v, want %+v", i, lines[i], want[i])
		}
	}
	// Both position accounts auto-created as unlimited asset accounts.
	if len(ledger.created) != 2 {
		t.Fatalf("expected 2 position accounts created, got %+v", ledger.created)
	}
	for _, c := range ledger.created {
		if !c.Unlimited || c.Type != "asset" {
			t.Errorf("position account %s must be unlimited asset, got %+v", c.AccountID, c)
		}
	}
	// Response journal reflects the actually-posted 4 legs.
	if resp.Journal == nil || len(resp.Journal.Lines) != 4 {
		t.Errorf("response journal should show the posted 4-leg journal, got %+v", resp.Journal)
	}
}

func TestProcessTransfer_FXTenantPrefixedPositions(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{}
	fx := &stubFX{converted: 9200}

	ctx := context.WithValue(context.Background(), ctxTenant, "acme")
	req := TransferRequest{
		DedupID: "fx-2", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD", ToCurrency: "EUR",
	}
	if _, err := ProcessTransfer(ctx, acct, ledger, fx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.created) != 2 ||
		ledger.created[0].AccountID != "acme:fx-position-USD" ||
		ledger.created[1].AccountID != "acme:fx-position-EUR" {
		t.Errorf("expected tenant-prefixed position accounts, got %+v", ledger.created)
	}
	if ledger.callTenants[0] != "acme" {
		t.Errorf("journal call tenant = %q, want acme", ledger.callTenants[0])
	}
}

func TestProcessTransfer_FXReusesExistingPositions(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{accounts: map[string]*ledgerclient.Account{
		"fx-position-USD": {AccountID: "fx-position-USD", Currency: "USD", Unlimited: true},
		"fx-position-EUR": {AccountID: "fx-position-EUR", Currency: "EUR", Unlimited: true},
	}}
	fx := &stubFX{converted: 9200}
	req := TransferRequest{
		DedupID: "fx-3", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD", ToCurrency: "EUR",
	}
	if _, err := ProcessTransfer(context.Background(), acct, ledger, fx, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.created) != 0 {
		t.Errorf("expected no account creation for existing positions, got %+v", ledger.created)
	}
}

func TestProcessTransfer_FXFeeRejected(t *testing.T) {
	acct := &stubAccounting{journal: feeJournal()}
	ledger := &stubLedger{}
	fx := &stubFX{converted: 9200}
	req := TransferRequest{
		DedupID: "fx-4", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, FeeMinor: 300, Currency: "USD", ToCurrency: "EUR",
	}
	_, err := ProcessTransfer(context.Background(), acct, ledger, fx, req)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 HTTPError, got %v", err)
	}
	if acct.calls != 0 || len(ledger.journalCalls) != 0 {
		t.Errorf("expected no compose/journal calls, got compose=%d journal=%d", acct.calls, len(ledger.journalCalls))
	}
}

func TestProcessTransfer_FXNotConfigured(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{}
	req := TransferRequest{
		DedupID: "fx-5", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD", ToCurrency: "EUR",
	}
	_, err := ProcessTransfer(context.Background(), acct, ledger, nil, req)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 HTTPError when fx is not configured, got %v", err)
	}
}

func TestProcessTransfer_SameToCurrencyIsNotFX(t *testing.T) {
	acct := &stubAccounting{journal: noFeeJournal()}
	ledger := &stubLedger{}
	req := TransferRequest{
		DedupID: "txn-9", FromAccount: "platform", ToAccount: "seller-1",
		AmountMinor: 10000, Currency: "USD", ToCurrency: "USD",
	}
	if _, err := ProcessTransfer(context.Background(), acct, ledger, nil, req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ledger.journalCalls) != 1 || len(ledger.journalCalls[0].Lines) != 2 {
		t.Errorf("expected plain 2-leg journal, got %+v", ledger.journalCalls)
	}
}
