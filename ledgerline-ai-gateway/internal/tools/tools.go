// Package tools wires the three MCP tools exposed by the gateway
// (get_account_balance, get_journal_entry, search_transactions) to the
// database and the redaction policy, and guarantees that every call -
// success or failure - produces exactly one audit log entry.
//
// The wiring here is deliberately thin and protocol-agnostic: it knows
// nothing about MCP or JSON-RPC. cmd/ledgerline-ai-gateway adapts these
// methods to mcp-go's tool handler signature.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ledgerline/ledgerline-ai-gateway/internal/audit"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/db"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/redact"
)

// DefaultMaxSearchLimit is the hard server-side cap on search_transactions'
// "limit" argument, enforced regardless of what the caller requests.
const DefaultMaxSearchLimit = 20

// DataStore is the subset of *db.Store the tool handlers need. It is an
// interface so tests can exercise the audit/redaction wiring with a fake
// in-memory store instead of a live Postgres connection.
type DataStore interface {
	GetAccountBalance(ctx context.Context, accountID string) (db.AccountBalance, error)
	GetJournalEntriesByDedupID(ctx context.Context, dedupID string) ([]redact.JournalRow, error)
	SearchTransactions(ctx context.Context, accountID string, limit int) ([]redact.JournalRow, error)
}

// Service holds the dependencies shared by all three tool handlers.
type Service struct {
	Store          DataStore
	Policy         redact.Policy
	Audit          *audit.Logger
	MaxSearchLimit int
}

// NewService builds a Service, defaulting MaxSearchLimit if left at zero.
func NewService(store DataStore, policy redact.Policy, logger *audit.Logger, maxSearchLimit int) *Service {
	if maxSearchLimit <= 0 {
		maxSearchLimit = DefaultMaxSearchLimit
	}
	return &Service{Store: store, Policy: policy, Audit: logger, MaxSearchLimit: maxSearchLimit}
}

// --- get_account_balance -----------------------------------------------

// GetAccountBalanceArgs is the input schema for get_account_balance.
type GetAccountBalanceArgs struct {
	AccountID string `json:"account_id"`
}

// GetAccountBalanceResult is the output schema for get_account_balance. No
// redaction is applied: the tool's declared contract is already a narrow,
// scoped lookup for the exact account the caller named, so there is
// nothing further to redact (see README "tool-level scoping").
type GetAccountBalanceResult struct {
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
}

// GetAccountBalance looks up a single account's balance.
func (s *Service) GetAccountBalance(ctx context.Context, args GetAccountBalanceArgs) (*GetAccountBalanceResult, error) {
	rowCount := 0
	var resultErr error
	defer func() { s.logAudit("get_account_balance", args, rowCount, nil, resultErr) }()

	if args.AccountID == "" {
		resultErr = errors.New("account_id is required")
		return nil, resultErr
	}

	bal, err := s.Store.GetAccountBalance(ctx, args.AccountID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			resultErr = fmt.Errorf("no account_balances row for account_id %q", args.AccountID)
			return nil, resultErr
		}
		resultErr = err
		return nil, resultErr
	}

	rowCount = 1
	return &GetAccountBalanceResult{
		AccountID:    bal.AccountID,
		Currency:     bal.Currency,
		BalanceMinor: bal.BalanceMinor,
	}, nil
}

// --- get_journal_entry ---------------------------------------------------

// GetJournalEntryArgs is the input schema for get_journal_entry.
type GetJournalEntryArgs struct {
	TransferDedupID string `json:"transfer_dedup_id"`
}

// GetJournalEntryResult is the output schema for get_journal_entry.
type GetJournalEntryResult struct {
	Entries []redact.RedactedJournalRow `json:"entries"`
}

// GetJournalEntry looks up every leg of a transfer by its dedup id,
// returning each row with cost_center redacted per policy.
func (s *Service) GetJournalEntry(ctx context.Context, args GetJournalEntryArgs) (*GetJournalEntryResult, error) {
	rowCount := 0
	var redactedFields []string
	var resultErr error
	defer func() { s.logAudit("get_journal_entry", args, rowCount, redactedFields, resultErr) }()

	if args.TransferDedupID == "" {
		resultErr = errors.New("transfer_dedup_id is required")
		return nil, resultErr
	}

	rows, err := s.Store.GetJournalEntriesByDedupID(ctx, args.TransferDedupID)
	if err != nil {
		resultErr = err
		return nil, resultErr
	}
	if len(rows) == 0 {
		resultErr = fmt.Errorf("no journal_entries rows for transfer_dedup_id %q", args.TransferDedupID)
		return nil, resultErr
	}

	redactedSet := map[string]struct{}{}
	out := make([]redact.RedactedJournalRow, 0, len(rows))
	for _, row := range rows {
		redactedRow, fields := redact.RedactJournalRow(row, s.Policy)
		out = append(out, redactedRow)
		for _, f := range fields {
			redactedSet[f] = struct{}{}
		}
	}
	redactedFields = setToSortedSlice(redactedSet)
	rowCount = len(out)

	return &GetJournalEntryResult{Entries: out}, nil
}

// --- search_transactions --------------------------------------------------

// SearchTransactionsArgs is the input schema for search_transactions.
type SearchTransactionsArgs struct {
	AccountID string `json:"account_id"`
	Limit     int    `json:"limit"`
}

// SearchTransactionsResult is the output schema for search_transactions.
type SearchTransactionsResult struct {
	Entries []redact.SearchRow `json:"entries"`
}

// SearchTransactions looks up the most recent journal_entries rows for an
// account, capping the result count server-side regardless of the
// caller-requested limit, and applying both cost_center redaction and
// transfer_dedup_id masking to every row.
func (s *Service) SearchTransactions(ctx context.Context, args SearchTransactionsArgs) (*SearchTransactionsResult, error) {
	rowCount := 0
	var redactedFields []string
	var resultErr error
	// Log the effective (capped) limit that was actually used, not just
	// the raw caller-requested value, so the audit trail reflects reality.
	effectiveLimit := redact.CapLimit(args.Limit, s.MaxSearchLimit)
	loggedArgs := SearchTransactionsArgs{AccountID: args.AccountID, Limit: args.Limit}
	defer func() {
		s.logAuditWithExtra("search_transactions", loggedArgs, rowCount, redactedFields, resultErr, effectiveLimit)
	}()

	if args.AccountID == "" {
		resultErr = errors.New("account_id is required")
		return nil, resultErr
	}

	rows, err := s.Store.SearchTransactions(ctx, args.AccountID, effectiveLimit)
	if err != nil {
		resultErr = err
		return nil, resultErr
	}

	redactedSet := map[string]struct{}{}
	out := make([]redact.SearchRow, 0, len(rows))
	for _, row := range rows {
		searchRow, fields := redact.RedactSearchRow(row, s.Policy)
		out = append(out, searchRow)
		for _, f := range fields {
			redactedSet[f] = struct{}{}
		}
	}
	redactedFields = setToSortedSlice(redactedSet)
	rowCount = len(out)

	return &SearchTransactionsResult{Entries: out}, nil
}

// --- audit helpers ---------------------------------------------------------

func (s *Service) logAudit(tool string, args any, rowCount int, redactedFields []string, callErr error) {
	s.logAuditWithExtra(tool, args, rowCount, redactedFields, callErr, -1)
}

// logAuditWithExtra emits the audit entry. effectiveLimit, when >= 0, is
// folded into the logged args under "effective_limit" so the audit trail
// shows the server-enforced value actually used, alongside the raw
// caller-requested one.
func (s *Service) logAuditWithExtra(tool string, args any, rowCount int, redactedFields []string, callErr error, effectiveLimit int) {
	if s.Audit == nil {
		return
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		argsJSON = json.RawMessage(`{"error":"failed to marshal args"}`)
	}
	if effectiveLimit >= 0 {
		var m map[string]any
		if json.Unmarshal(argsJSON, &m) == nil {
			m["effective_limit"] = effectiveLimit
			if reJSON, err := json.Marshal(m); err == nil {
				argsJSON = reJSON
			}
		}
	}

	entry := audit.Entry{
		Timestamp:      time.Now(),
		Tool:           tool,
		Args:           argsJSON,
		ResultRowCount: rowCount,
		RedactedFields: redactedFields,
	}
	if callErr != nil {
		entry.Error = callErr.Error()
	}
	// Audit logging is best-effort: a logging failure must not mask or
	// replace the tool call's own result/error.
	_ = s.Audit.Log(entry)
}

func setToSortedSlice(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Small, fixed universe of field names (cost_center,
	// transfer_dedup_id); simple insertion sort keeps this dependency-free.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
