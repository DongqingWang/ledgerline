// Package gatewayapi implements ledgerline-gateway's client-facing REST API:
// it composes a double-entry journal via ledgerline-accounting-core and then
// submits it to a ledgerline-raft cluster as ONE atomic multi-leg journal
// (AssertJournal), so a fee-split payout either posts every leg or none.
package gatewayapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-gateway/internal/accountingclient"
	"github.com/ledgerline/ledgerline-gateway/internal/fxclient"
	"github.com/ledgerline/ledgerline-gateway/internal/ledgerclient"
)

// FeeRevenueAccount is the well-known account that fee legs are credited to.
// It must exist as a real account in the ledger cluster (created via the
// normal POST /v1/accounts) for fee transfers to succeed.
const FeeRevenueAccount = "platform-fee-revenue"

// GL classification for FX position legs: a currency-position asset account.
const (
	fxPositionGLAccount = "1030"
	fxCostCenter        = "CC-FX"
)

// LedgerClient is the subset of *ledgerclient.Client used by this package,
// extracted as an interface so tests can supply a stub.
type LedgerClient interface {
	Journal(ctx context.Context, req ledgerclient.JournalRequest) (*ledgerclient.TransferResult, error)
	Reverse(ctx context.Context, req ledgerclient.ReverseRequest) (*ledgerclient.TransferResult, error)
	GetAccount(ctx context.Context, accountID string) (*ledgerclient.Account, error)
	CreateAccount(ctx context.Context, req ledgerclient.CreateAccountRequest) (*ledgerclient.Account, error)
}

// AccountingClient is the subset of *accountingclient.Client used by this
// package, extracted as an interface so tests can supply a stub.
type AccountingClient interface {
	Compose(req accountingclient.ComposeRequest) (*accountingclient.Journal, error)
}

// FXClient is the subset of *fxclient.Client used by this package. A nil
// FXClient means cross-currency transfers are rejected.
type FXClient interface {
	Convert(ctx context.Context, base, quote string, amountMinor int64) (*fxclient.ConvertResult, error)
}

// Server holds the dependencies for the gateway's HTTP handlers.
type Server struct {
	Ledger     LedgerClient
	Accounting AccountingClient
	FX         FXClient
	Auth       *AuthConfig
}

// NewServer constructs a Server and its routing. fx may be nil to disable
// cross-currency transfers.
func NewServer(ledger LedgerClient, accounting AccountingClient, fx FXClient) *Server {
	return &Server{Ledger: ledger, Accounting: accounting, FX: fx}
}

// Routes returns an http.Handler with all gateway routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/transfers", s.handleTransfers)
	mux.HandleFunc("/v1/reversals", s.handleReversals)
	mux.HandleFunc("/v1/accounts/", s.handleGetAccount)
	return s.withAuth(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

// HTTPError carries an explicit HTTP status for business-level rejections
// (as opposed to upstream failures, which surface as 502).
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// TransferRequest is the gateway's own, richer transfer request body.
// ToCurrency, when set and different from Currency, requests an FX
// conversion: the recipient is credited in ToCurrency through the
// platform's fx-position accounts.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor"`
	Currency    string `json:"currency"`
	ToCurrency  string `json:"to_currency,omitempty"`
	Category    string `json:"category"`
	Country     string `json:"country"`
}

// TransferResponse is the gateway's own transfer response body. Journal is
// the journal actually posted to the ledger (for FX transfers this includes
// the position-account legs).
type TransferResponse struct {
	Status        string                         `json:"status"`
	Journal       *accountingclient.Journal      `json:"journal"`
	LedgerResults []*ledgerclient.TransferResult `json:"ledger_results"`
}

func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req TransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.DedupID == "" || req.FromAccount == "" || req.ToAccount == "" || req.AmountMinor <= 0 || req.Currency == "" {
		writeError(w, http.StatusBadRequest, "dedup_id, from_account, to_account, currency are required and amount_minor must be positive")
		return
	}
	if req.FeeMinor < 0 || req.FeeMinor >= req.AmountMinor {
		writeError(w, http.StatusBadRequest, "fee_minor must be >= 0 and less than amount_minor")
		return
	}

	resp, err := ProcessTransfer(r.Context(), s.Accounting, s.Ledger, s.FX, req)
	if err != nil {
		writeProcessError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReversals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req ledgerclient.ReverseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.DedupID == "" || req.OriginalDedupID == "" {
		writeError(w, http.StatusBadRequest, "dedup_id and original_dedup_id are required")
		return
	}

	result, err := s.Ledger.Reverse(engineContext(r.Context()), req)
	if err != nil {
		writeProcessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeProcessError maps business and upstream errors onto HTTP statuses:
// explicit HTTPError and accounting rejections are 4xx; engine errors keep
// the engine's status (409 conflict, 422 insufficient funds, ...); anything
// else is an upstream failure (502).
func writeProcessError(w http.ResponseWriter, err error) {
	var he *HTTPError
	if errors.As(err, &he) {
		writeError(w, he.Status, he.Message)
		return
	}
	var ae *accountingclient.Error
	if errors.As(err, &ae) {
		writeError(w, http.StatusUnprocessableEntity, "accounting error: "+ae.Message)
		return
	}
	var fe *fxclient.Error
	if errors.As(err, &fe) {
		if fe.StatusCode == http.StatusNotFound {
			writeError(w, http.StatusUnprocessableEntity, "fx error: "+fe.Message)
		} else {
			writeError(w, http.StatusBadGateway, "fx error: "+fe.Message)
		}
		return
	}
	var le *ledgerclient.Error
	if errors.As(err, &le) {
		writeError(w, le.StatusCode, le.Message)
		return
	}
	log.Printf("request failed: %v", err)
	writeError(w, http.StatusBadGateway, err.Error())
}

// engineContext bridges the gateway's authenticated tenant onto the
// ledger client's X-Tenant-ID header. The gateway calls the engine with an
// admin service key and acts on behalf of the authenticated tenant; tenant
// identity never comes from a request body.
func engineContext(ctx context.Context) context.Context {
	return ledgerclient.WithTenant(ctx, tenantFromContext(ctx))
}

// ProcessTransfer implements the gateway's core business logic: compose a
// journal via accountingsvc, then post it as ONE atomic multi-leg journal.
// With a fee, the composed journal already contains the net-credit and
// fee-credit legs with their own GL codes — all legs commit together or
// none do, so an insufficient-funds rejection can never leave a partially
// executed payout behind.
//
// For cross-currency requests (ToCurrency != Currency), the payout posts as
// a 4-leg journal through the platform's fx-position accounts:
//
//	debit  from            amount     (Currency)
//	credit fx-position-A   amount     (Currency)
//	debit  fx-position-B   converted  (ToCurrency)
//	credit to              converted  (ToCurrency)
//
// Each currency balances independently, which is exactly the engine's
// per-currency journal invariant.
//
// Exported (and parameterized over interfaces) so it can be unit tested with
// stub clients.
func ProcessTransfer(ctx context.Context, accounting AccountingClient, ledger LedgerClient, fx FXClient, req TransferRequest) (*TransferResponse, error) {
	crossCurrency := req.ToCurrency != "" && req.ToCurrency != req.Currency
	if crossCurrency {
		if fx == nil {
			return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Message: "cross-currency transfers require the fx service (-fx-addr)"}
		}
		if req.FeeMinor > 0 {
			return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Message: "fee_minor is not supported on cross-currency transfers"}
		}
	}

	journal, err := accounting.Compose(accountingclient.ComposeRequest{
		FromAccount: req.FromAccount,
		ToAccount:   req.ToAccount,
		AmountMinor: req.AmountMinor,
		FeeMinor:    req.FeeMinor,
		Currency:    req.Currency,
		Category:    req.Category,
		Country:     req.Country,
	})
	if err != nil {
		return nil, err
	}

	ectx := engineContext(ctx)

	var jreq ledgerclient.JournalRequest
	posted := journal
	if crossCurrency {
		conv, err := fx.Convert(ctx, req.Currency, req.ToCurrency, req.AmountMinor)
		if err != nil {
			return nil, err
		}
		if conv.ConvertedAmountMinor <= 0 {
			return nil, &HTTPError{Status: http.StatusUnprocessableEntity, Message: "converted amount rounds to zero"}
		}
		tenant := tenantFromContext(ctx)
		posA := fxPositionAccount(req.Currency, tenant)
		posB := fxPositionAccount(req.ToCurrency, tenant)
		if err := ensureFXPosition(ectx, ledger, posA, req.Currency); err != nil {
			return nil, err
		}
		if err := ensureFXPosition(ectx, ledger, posB, req.ToCurrency); err != nil {
			return nil, err
		}
		jreq, posted, err = buildFXJournal(req, journal, conv.ConvertedAmountMinor, posA, posB)
		if err != nil {
			return nil, err
		}
	} else {
		jreq, err = BuildLedgerJournal(req, journal)
		if err != nil {
			return nil, err
		}
	}

	result, err := ledger.Journal(ectx, jreq)
	if err != nil {
		return nil, fmt.Errorf("ledger journal %q failed: %w", jreq.DedupID, err)
	}

	return &TransferResponse{
		Status:        result.Status,
		Journal:       posted,
		LedgerResults: []*ledgerclient.TransferResult{result},
	}, nil
}

// BuildLedgerJournal translates a composed accounting Journal into a single
// atomic multi-leg journal request, preserving each leg's own gl_account /
// cost_center. This replaces the old two-transfer net+fee mapping, which
// was not atomic: a fee leg failing after the net leg succeeded left money
// moved with an error returned and no reversal.
func BuildLedgerJournal(req TransferRequest, journal *accountingclient.Journal) (ledgerclient.JournalRequest, error) {
	if len(journal.Lines) < 2 {
		return ledgerclient.JournalRequest{}, errors.New("gatewayapi: composed journal has fewer than 2 lines")
	}
	lines := make([]ledgerclient.JournalLine, 0, len(journal.Lines))
	for _, l := range journal.Lines {
		lines = append(lines, ledgerclient.JournalLine{
			AccountID:   l.AccountID,
			Direction:   l.Direction,
			AmountMinor: l.AmountMinor,
			GLAccount:   l.GLAccount,
			CostCenter:  l.CostCenter,
		})
	}
	return ledgerclient.JournalRequest{DedupID: req.DedupID, Lines: lines}, nil
}

// fxPositionAccount names the platform's currency-position account. With
// tenant isolation enabled each tenant gets its own position accounts so
// positions never commingle across tenants.
func fxPositionAccount(currency, tenant string) string {
	if tenant != "" {
		return tenant + ":fx-position-" + currency
	}
	return "fx-position-" + currency
}

// ensureFXPosition creates the position account if it does not exist yet.
// Position accounts are unlimited asset accounts: they are the platform's
// own currency inventory, so they may go negative in one currency while
// positive in another.
func ensureFXPosition(ctx context.Context, ledger LedgerClient, accountID, currency string) error {
	_, err := ledger.GetAccount(ctx, accountID)
	if err == nil {
		return nil
	}
	var le *ledgerclient.Error
	if !errors.As(err, &le) || le.StatusCode != http.StatusNotFound {
		return err
	}
	_, err = ledger.CreateAccount(ctx, ledgerclient.CreateAccountRequest{
		AccountID: accountID,
		Currency:  currency,
		Unlimited: true,
		Type:      "asset",
	})
	if err != nil {
		var ce *ledgerclient.Error
		if errors.As(err, &ce) && ce.Code == "account_exists" {
			// A concurrent request created it first; that's fine.
			return nil
		}
		return err
	}
	return nil
}

// buildFXJournal builds the 4-leg cross-currency journal and the matching
// response journal. GL classification comes from the composed source-
// currency journal's debit and recipient-credit legs; the two position legs
// use the FX position GL.
func buildFXJournal(req TransferRequest, journal *accountingclient.Journal, convertedMinor int64, posA, posB string) (ledgerclient.JournalRequest, *accountingclient.Journal, error) {
	var debitLine, creditLine *accountingclient.JournalLine
	for i := range journal.Lines {
		l := &journal.Lines[i]
		switch {
		case l.Direction == "debit" && debitLine == nil:
			debitLine = l
		case l.Direction == "credit" && l.AccountID == req.ToAccount:
			creditLine = l
		}
	}
	if debitLine == nil || creditLine == nil {
		return ledgerclient.JournalRequest{}, nil, errors.New("gatewayapi: composed journal missing debit or recipient credit line for FX mapping")
	}

	lines := []ledgerclient.JournalLine{
		{AccountID: req.FromAccount, Direction: "debit", AmountMinor: req.AmountMinor, GLAccount: debitLine.GLAccount, CostCenter: debitLine.CostCenter},
		{AccountID: posA, Direction: "credit", AmountMinor: req.AmountMinor, GLAccount: fxPositionGLAccount, CostCenter: fxCostCenter},
		{AccountID: posB, Direction: "debit", AmountMinor: convertedMinor, GLAccount: fxPositionGLAccount, CostCenter: fxCostCenter},
		{AccountID: req.ToAccount, Direction: "credit", AmountMinor: convertedMinor, GLAccount: creditLine.GLAccount, CostCenter: creditLine.CostCenter},
	}

	posted := &accountingclient.Journal{Lines: make([]accountingclient.JournalLine, 0, len(lines))}
	for _, l := range lines {
		posted.Lines = append(posted.Lines, accountingclient.JournalLine{
			AccountID:   l.AccountID,
			Direction:   l.Direction,
			AmountMinor: l.AmountMinor,
			GLAccount:   l.GLAccount,
			CostCenter:  l.CostCenter,
		})
	}

	return ledgerclient.JournalRequest{DedupID: req.DedupID, Lines: lines}, posted, nil
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	accountID := r.URL.Path[len("/v1/accounts/"):]
	if accountID == "" {
		writeError(w, http.StatusBadRequest, "account id required")
		return
	}

	acct, err := s.Ledger.GetAccount(engineContext(r.Context()), accountID)
	if err != nil {
		writeProcessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
