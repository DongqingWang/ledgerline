// Package store contains the only I/O boundary in this program: reading
// journal_entries rows out of the durable Postgres projection written by
// ledgerline-persistence-sync. It deliberately knows nothing about trial
// balance aggregation — it just fetches rows and hands them back as plain
// structs for internal/trialbalance to process.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// JournalRow mirrors a single journal_entries record, with gl_account
// already normalized (NULL -> "").
type JournalRow struct {
	GLAccount   string
	Direction   string
	AmountMinor int64
}

// Open establishes a connection pool to Postgres and verifies connectivity.
func Open(ctx context.Context, postgresURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", postgresURL)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	return db, nil
}

// FetchJournalRows reads gl_account, direction, and amount_minor from
// journal_entries, optionally filtered to posted_at within [from, to]
// (either bound may be the zero time.Time to mean "unbounded").
func FetchJournalRows(ctx context.Context, db *sql.DB, from, to time.Time) ([]JournalRow, error) {
	query := `SELECT gl_account, direction, amount_minor FROM journal_entries WHERE 1=1`
	args := make([]any, 0, 2)

	if !from.IsZero() {
		args = append(args, from)
		query += fmt.Sprintf(" AND posted_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		query += fmt.Sprintf(" AND posted_at <= $%d", len(args))
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying journal_entries: %w", err)
	}
	defer rows.Close()

	var out []JournalRow
	for rows.Next() {
		var (
			glAccount sql.NullString
			direction string
			amount    int64
		)
		if err := rows.Scan(&glAccount, &direction, &amount); err != nil {
			return nil, fmt.Errorf("scanning journal_entries row: %w", err)
		}
		out = append(out, JournalRow{
			GLAccount:   glAccount.String, // "" when NULL, which callers treat as UNSPECIFIED
			Direction:   direction,
			AmountMinor: amount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating journal_entries rows: %w", err)
	}

	return out, nil
}
