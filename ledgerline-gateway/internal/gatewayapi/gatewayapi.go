// Package gatewayapi implements ledgerline-gateway's client-facing REST API:
// it composes a double-entry journal via ledgerline-accounting-core and then
// submits the resulting transfer(s) to a ledgerline-raft cluster.
package gatewayapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-gateway/internal/accountingclient"
	"github.com/ledgerline/ledgerline-gateway/internal/ledgerclient"
)

// FeeRevenueAccount is the well-known account that fee legs are credited to.
// It must exist as a real account in the ledger cluster (created via the
// normal POST /v1/accounts) for fee transfers to succeed.
const FeeRevenueAccount = "platform-fee-revenue"

// LedgerClient is the subset of *ledgerclient.Client used by this package,
// extracted as an interface so tests can supply a stub.
type LedgerClient interface {
	Transfer(req ledgerclient.TransferRequest) (*ledgerclient.TransferResult, error)
	GetAccount(accountID string) (*ledgerclient.Account, error)
}

// AccountingClient is the subset of *accountingclient.Client used by this
// package, extracted as an interface so tests can supply a stub.
type AccountingClient interface {
	Compose(req accountingclient.ComposeRequest) (*accountingclient.Journal, error)
}

// Server holds the dependencies for the gateway's HTTP handlers.
type Server struct {
	Ledger     LedgerClient
	Accounting AccountingClient
}

// NewServer constructs a Server and its routing.
func NewServer(ledger LedgerClient, accounting AccountingClient) *Server {
	return &Server{Ledger: ledger, Accounting: accounting}
}

// Routes returns an http.Handler with all gateway routes registered.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/v1/transfers", s.handleTransfers)
	mux.HandleFunc("/v1/accounts/", s.handleGetAccount)
	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"state": "ok"})
}

// TransferRequest is the gateway's own, richer transfer request body.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor"`
	Currency    string `json:"currency"`
	Category    string `json:"category"`
	Country     string `json:"country"`
}

// TransferResponse is the gateway's own transfer response body.
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

	resp, err := ProcessTransfer(s.Accounting, s.Ledger, req)
	if err != nil {
		var ae *accountingclient.Error
		if errors.As(err, &ae) {
			writeError(w, http.StatusUnprocessableEntity, "accounting error: "+ae.Message)
			return
		}
		log.Printf("transfer failed: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// ProcessTransfer implements the gateway's core business logic: compose a
// journal via accountingsvc, map its lines onto one or two ledger-engine
// transfers, submit them, and report back the composed journal plus the
// ledger results.
//
// Exported (and parameterized over interfaces) so it can be unit tested with
// httptest-backed stub clients.
func ProcessTransfer(accounting AccountingClient, ledger LedgerClient, req TransferRequest) (*TransferResponse, error) {
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

	transfers, err := BuildLedgerTransfers(req, journal)
	if err != nil {
		return nil, err
	}

	results := make([]*ledgerclient.TransferResult, 0, len(transfers))
	status := "applied"
	for _, tr := range transfers {
		result, err := ledger.Transfer(tr)
		if err != nil {
			return nil, fmt.Errorf("ledger transfer %q failed: %w", tr.DedupID, err)
		}
		if result.Status == "duplicate" {
			status = "duplicate"
		}
		results = append(results, result)
	}

	return &TransferResponse{
		Status:        status,
		Journal:       journal,
		LedgerResults: results,
	}, nil
}

// BuildLedgerTransfers translates a composed accounting Journal into one or
// two ledgerline-raft transfer requests, per the gateway's documented
// mapping rules:
//
//   - No fee (fee_minor == 0): exactly one ledger transfer, using the
//     primary debit line's gl_account/cost_center.
//   - Fee (fee_minor > 0): two ledger transfers — a "net" transfer for
//     amount_minor-fee_minor using the net-credit line's gl_account/
//     cost_center, and a "fee" transfer to FeeRevenueAccount for fee_minor
//     using the fee line's gl_account/cost_center.
func BuildLedgerTransfers(req TransferRequest, journal *accountingclient.Journal) ([]ledgerclient.TransferRequest, error) {
	var debitLine *accountingclient.JournalLine
	var netCreditLine *accountingclient.JournalLine
	var feeCreditLine *accountingclient.JournalLine

	for i := range journal.Lines {
		line := &journal.Lines[i]
		switch {
		case line.Direction == "debit" && debitLine == nil:
			debitLine = line
		case line.Direction == "credit" && line.AccountID == req.ToAccount:
			netCreditLine = line
		case line.Direction == "credit" && line.AccountID == FeeRevenueAccount:
			feeCreditLine = line
		}
	}

	if debitLine == nil {
		return nil, errors.New("gatewayapi: composed journal has no debit line")
	}

	if req.FeeMinor == 0 {
		return []ledgerclient.TransferRequest{
			{
				DedupID:     req.DedupID,
				FromAccount: req.FromAccount,
				ToAccount:   req.ToAccount,
				AmountMinor: req.AmountMinor,
				Currency:    req.Currency,
				GLAccount:   debitLine.GLAccount,
				CostCenter:  debitLine.CostCenter,
			},
		}, nil
	}

	if netCreditLine == nil {
		return nil, fmt.Errorf("gatewayapi: composed journal has no credit line for %q", req.ToAccount)
	}
	if feeCreditLine == nil {
		return nil, fmt.Errorf("gatewayapi: composed journal has no fee credit line for %q", FeeRevenueAccount)
	}

	return []ledgerclient.TransferRequest{
		{
			DedupID:     req.DedupID + "-net",
			FromAccount: req.FromAccount,
			ToAccount:   req.ToAccount,
			AmountMinor: req.AmountMinor - req.FeeMinor,
			Currency:    req.Currency,
			GLAccount:   netCreditLine.GLAccount,
			CostCenter:  netCreditLine.CostCenter,
		},
		{
			DedupID:     req.DedupID + "-fee",
			FromAccount: req.FromAccount,
			ToAccount:   FeeRevenueAccount,
			AmountMinor: req.FeeMinor,
			Currency:    req.Currency,
			GLAccount:   feeCreditLine.GLAccount,
			CostCenter:  feeCreditLine.CostCenter,
		},
	}, nil
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

	acct, err := s.Ledger.GetAccount(accountID)
	if err != nil {
		var le *ledgerclient.Error
		if errors.As(err, &le) {
			writeError(w, le.StatusCode, le.Message)
			return
		}
		log.Printf("get account failed: %v", err)
		writeError(w, http.StatusBadGateway, err.Error())
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
