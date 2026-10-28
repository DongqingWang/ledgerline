// Package db contains the gateway's read-only data access against the
// shared ledger Postgres database. It is a thin layer over database/sql:
// all redaction and business logic lives in internal/redact, not here, so
// that logic can be unit tested without a live database.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "github.com/lib/pq"

	"github.com/ledgerline/ledgerline-ai-gateway/internal/redact"
)

// ErrNotFound is returned when a lookup matches no rows.
var ErrNotFound = errors.New("db: not found")

// Store is a read-only handle onto the ledger database. The connection
// string it is opened with is expected to authenticate as the
// least-privilege ledgerline_ai_readonly role (see README.md), not an
// admin/owner role.
type Store struct {
	db *sql.DB
}

// Open opens a connection pool against postgresURL. It does not verify
// connectivity; call Ping for that.
func Open(postgresURL string) (*Store, error) {
	sqlDB, err := sql.Open("postgres", postgresURL)
	if err != nil {
		return nil, fmt.Errorf("db: open: %w", err)
	}
	return &Store{db: sqlDB}, nil
}

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

// Close closes the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// AccountBalance is the in-memory representation of one account_balances
// row. There is no sensitive/internal data in this table's schema, so no
// redaction is applied to it (see README "tool-level scoping" discussion).
type AccountBalance struct {
	AccountID    string
	Currency     string
	BalanceMinor int64
}

// GetAccountBalance fetches the balance row for a single account_id. Backs
// the get_account_balance tool.
func (s *Store) GetAccountBalance(ctx context.Context, accountID string) (AccountBalance, error) {
	var ab AccountBalance
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id, currency, balance_minor FROM account_balances WHERE account_id = $1`,
		accountID,
	).Scan(&ab.AccountID, &ab.Currency, &ab.BalanceMinor)
	if errors.Is(err, sql.ErrNoRows) {
		return AccountBalance{}, ErrNotFound
	}
	if err != nil {
		return AccountBalance{}, fmt.Errorf("db: get account balance: %w", err)
	}
	return ab, nil
}

// GetJournalEntriesByDedupID fetches every leg (row) of journal_entries
// sharing a transfer_dedup_id. Backs the get_journal_entry tool. Returns an
// empty (non-nil) slice, not an error, if no rows match.
func (s *Store) GetJournalEntriesByDedupID(ctx context.Context, dedupID string) ([]redact.JournalRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, transfer_dedup_id, account_id, direction, amount_minor, currency,
		        COALESCE(gl_account, ''), COALESCE(cost_center, ''), posted_at, raft_index
		   FROM journal_entries
		  WHERE transfer_dedup_id = $1
		  ORDER BY id ASC`,
		dedupID,
	)
	if err != nil {
		return nil, fmt.Errorf("db: get journal entries by dedup id: %w", err)
	}
	defer rows.Close()
	return scanJournalRows(rows)
}

// SearchTransactions fetches up to limit journal_entries rows for a single
// account_id, most recent first by posted_at. Backs the search_transactions
// tool. The caller is responsible for applying the hard server-side cap to
// limit (see redact.CapLimit) before calling this method.
func (s *Store) SearchTransactions(ctx context.Context, accountID string, limit int) ([]redact.JournalRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, transfer_dedup_id, account_id, direction, amount_minor, currency,
		        COALESCE(gl_account, ''), COALESCE(cost_center, ''), posted_at, raft_index
		   FROM journal_entries
		  WHERE account_id = $1
		  ORDER BY posted_at DESC, id DESC
		  LIMIT $2`,
		accountID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("db: search transactions: %w", err)
	}
	defer rows.Close()
	return scanJournalRows(rows)
}

func scanJournalRows(rows *sql.Rows) ([]redact.JournalRow, error) {
	out := []redact.JournalRow{}
	for rows.Next() {
		var r redact.JournalRow
		if err := rows.Scan(
			&r.ID, &r.TransferDedupID, &r.AccountID, &r.Direction, &r.AmountMinor,
			&r.Currency, &r.GLAccount, &r.CostCenter, &r.PostedAt, &r.RaftIndex,
		); err != nil {
			return nil, fmt.Errorf("db: scan journal row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate journal rows: %w", err)
	}
	return out, nil
}
