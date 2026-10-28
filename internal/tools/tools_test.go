package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ledgerline/ledgerline-ai-gateway/internal/audit"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/db"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/redact"
)

// fakeStore is an in-memory DataStore used to unit test the tool wiring
// (redaction + audit logging + limit enforcement) without a live database.
type fakeStore struct {
	balances map[string]db.AccountBalance
	journals map[string][]redact.JournalRow
	search   map[string][]redact.JournalRow

	searchCalledWithLimit int
}

func (f *fakeStore) GetAccountBalance(ctx context.Context, accountID string) (db.AccountBalance, error) {
	b, ok := f.balances[accountID]
	if !ok {
		return db.AccountBalance{}, db.ErrNotFound
	}
	return b, nil
}

func (f *fakeStore) GetJournalEntriesByDedupID(ctx context.Context, dedupID string) ([]redact.JournalRow, error) {
	return f.journals[dedupID], nil
}

func (f *fakeStore) SearchTransactions(ctx context.Context, accountID string, limit int) ([]redact.JournalRow, error) {
	f.searchCalledWithLimit = limit
	rows := f.search[accountID]
	if limit < len(rows) {
		rows = rows[:limit]
	}
	return rows, nil
}

func newTestService(t *testing.T, store DataStore) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := audit.NewLogger(&buf)
	svc := NewService(store, redact.DefaultPolicy(), logger, 20)
	return svc, &buf
}

func TestGetAccountBalance_Success(t *testing.T) {
	store := &fakeStore{balances: map[string]db.AccountBalance{
		"alice": {AccountID: "alice", Currency: "USD", BalanceMinor: 96850},
	}}
	svc, auditBuf := newTestService(t, store)

	result, err := svc.GetAccountBalance(context.Background(), GetAccountBalanceArgs{AccountID: "alice"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.AccountID != "alice" || result.Currency != "USD" || result.BalanceMinor != 96850 {
		t.Errorf("unexpected result: %+v", result)
	}

	entry := decodeOneAuditEntry(t, auditBuf)
	if entry.Tool != "get_account_balance" {
		t.Errorf("tool = %q", entry.Tool)
	}
	if entry.ResultRowCount != 1 {
		t.Errorf("result_row_count = %d, want 1", entry.ResultRowCount)
	}
	if len(entry.RedactedFields) != 0 {
		t.Errorf("expected no redacted fields for get_account_balance, got %v", entry.RedactedFields)
	}
	if entry.Error != "" {
		t.Errorf("expected no error, got %q", entry.Error)
	}
}

func TestGetAccountBalance_NotFoundStillAudited(t *testing.T) {
	store := &fakeStore{balances: map[string]db.AccountBalance{}}
	svc, auditBuf := newTestService(t, store)

	_, err := svc.GetAccountBalance(context.Background(), GetAccountBalanceArgs{AccountID: "ghost"})
	if err == nil {
		t.Fatalf("expected an error for an unknown account")
	}

	entry := decodeOneAuditEntry(t, auditBuf)
	if entry.ResultRowCount != 0 {
		t.Errorf("result_row_count = %d, want 0", entry.ResultRowCount)
	}
	if entry.Error == "" {
		t.Errorf("expected the audit entry to record the error")
	}
	var args map[string]string
	if err := json.Unmarshal(entry.Args, &args); err != nil {
		t.Fatalf("args is not valid JSON: %v", err)
	}
	if args["account_id"] != "ghost" {
		t.Errorf("audit args did not preserve raw input: %+v", args)
	}
}

func TestGetJournalEntry_RedactsCostCenter(t *testing.T) {
	row1 := redact.JournalRow{TransferDedupID: "txn-2", AccountID: "alice", Direction: "debit", AmountMinor: 2500, Currency: "USD", GLAccount: "4000", CostCenter: "CC-1", PostedAt: time.Now()}
	row2 := redact.JournalRow{TransferDedupID: "txn-2", AccountID: "bob", Direction: "credit", AmountMinor: 2500, Currency: "USD", GLAccount: "4000", CostCenter: "CC-1", PostedAt: time.Now()}
	store := &fakeStore{journals: map[string][]redact.JournalRow{"txn-2": {row1, row2}}}
	svc, auditBuf := newTestService(t, store)

	result, err := svc.GetJournalEntry(context.Background(), GetJournalEntryArgs{TransferDedupID: "txn-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Entries) != 2 {
		t.Fatalf("expected 2 entries (both legs), got %d", len(result.Entries))
	}
	for _, e := range result.Entries {
		if e.CostCenter != nil {
			t.Errorf("expected cost_center to be redacted (nil), got %q", *e.CostCenter)
		}
	}

	// Marshal to JSON and confirm the key is genuinely absent, not just
	// nil in Go -- this is what an MCP client actually sees on the wire.
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(raw), "cost_center") {
		t.Errorf("expected no cost_center key at all in JSON output, got: %s", raw)
	}

	entry := decodeOneAuditEntry(t, auditBuf)
	if entry.ResultRowCount != 2 {
		t.Errorf("result_row_count = %d, want 2", entry.ResultRowCount)
	}
	if len(entry.RedactedFields) != 1 || entry.RedactedFields[0] != "cost_center" {
		t.Errorf("redacted_fields = %v, want [cost_center]", entry.RedactedFields)
	}
}

func TestGetJournalEntry_NoRowsIsAnError(t *testing.T) {
	store := &fakeStore{journals: map[string][]redact.JournalRow{}}
	svc, _ := newTestService(t, store)

	_, err := svc.GetJournalEntry(context.Background(), GetJournalEntryArgs{TransferDedupID: "does-not-exist"})
	if err == nil {
		t.Fatalf("expected an error when no journal rows match")
	}
}

func TestSearchTransactions_RedactsAndMasksAndCapsLimit(t *testing.T) {
	rows := make([]redact.JournalRow, 0, 50)
	for i := 0; i < 50; i++ {
		rows = append(rows, redact.JournalRow{
			TransferDedupID: "txn-abcdefghijklmnop",
			AccountID:       "alice",
			Direction:       "debit",
			AmountMinor:     int64(i),
			Currency:        "USD",
			CostCenter:      "CC-1",
			PostedAt:        time.Now(),
		})
	}
	store := &fakeStore{search: map[string][]redact.JournalRow{"alice": rows}}
	svc, auditBuf := newTestService(t, store)
	svc.MaxSearchLimit = 20

	result, err := svc.SearchTransactions(context.Background(), SearchTransactionsArgs{AccountID: "alice", Limit: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if store.searchCalledWithLimit != 20 {
		t.Errorf("expected the store to be called with the capped limit 20, got %d", store.searchCalledWithLimit)
	}
	if len(result.Entries) != 20 {
		t.Errorf("expected exactly 20 rows (hard cap), got %d", len(result.Entries))
	}
	for _, e := range result.Entries {
		if e.CostCenter != nil {
			t.Errorf("expected cost_center redacted in search_transactions row, got %q", *e.CostCenter)
		}
		if e.TransferDedupID == "txn-abcdefghijklmnop" {
			t.Errorf("expected transfer_dedup_id to be masked, got the full value")
		}
		if !strings.Contains(e.TransferDedupID, "...") {
			t.Errorf("expected masked transfer_dedup_id to contain '...', got %q", e.TransferDedupID)
		}
	}

	entry := decodeOneAuditEntry(t, auditBuf)
	if entry.ResultRowCount != 20 {
		t.Errorf("result_row_count = %d, want 20", entry.ResultRowCount)
	}
	wantFields := map[string]bool{"cost_center": true, "transfer_dedup_id": true}
	if len(entry.RedactedFields) != len(wantFields) {
		t.Errorf("redacted_fields = %v, want both cost_center and transfer_dedup_id", entry.RedactedFields)
	}
	for _, f := range entry.RedactedFields {
		if !wantFields[f] {
			t.Errorf("unexpected redacted field %q", f)
		}
	}

	// The raw caller-requested limit (1000) must still be visible in the
	// audit args, alongside the effective (capped) limit that was used.
	var args map[string]any
	if err := json.Unmarshal(entry.Args, &args); err != nil {
		t.Fatalf("args is not valid JSON: %v", err)
	}
	if int(args["limit"].(float64)) != 1000 {
		t.Errorf("audit args should preserve the raw requested limit 1000, got %v", args["limit"])
	}
	if int(args["effective_limit"].(float64)) != 20 {
		t.Errorf("audit args should record the effective (capped) limit 20, got %v", args["effective_limit"])
	}
}

func TestSearchTransactions_RequiresAccountID(t *testing.T) {
	svc, _ := newTestService(t, &fakeStore{})
	if _, err := svc.SearchTransactions(context.Background(), SearchTransactionsArgs{AccountID: ""}); err == nil {
		t.Errorf("expected an error when account_id is empty")
	}
}

// errStore lets us assert that DB errors still produce an audit entry.
type errStore struct{ fakeStore }

func (e *errStore) GetAccountBalance(ctx context.Context, accountID string) (db.AccountBalance, error) {
	return db.AccountBalance{}, errors.New("connection refused")
}

func TestGetAccountBalance_DBErrorStillAudited(t *testing.T) {
	svc, auditBuf := newTestService(t, &errStore{})
	_, err := svc.GetAccountBalance(context.Background(), GetAccountBalanceArgs{AccountID: "alice"})
	if err == nil {
		t.Fatalf("expected error to propagate")
	}
	entry := decodeOneAuditEntry(t, auditBuf)
	if entry.Error == "" {
		t.Errorf("expected audit entry to capture the DB error")
	}
}

func decodeOneAuditEntry(t *testing.T, buf *bytes.Buffer) audit.Entry {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly one audit log line, got %d: %q", len(lines), buf.String())
	}
	var entry audit.Entry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("audit line is not valid JSON: %v: %s", err, lines[0])
	}
	return entry
}
