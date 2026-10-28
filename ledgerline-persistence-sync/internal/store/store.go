// Package store implements the Postgres-backed durable projection that
// ledgerline-persistence-sync maintains. All SQL lives here; the
// translation of ledger events into row shapes lives in the DB-free
// internal/translate package so that logic can be unit tested without a
// live database.
package store

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
	"github.com/ledgerline/ledgerline-persistence-sync/internal/translate"
)

// Store wraps a pgx connection pool and exposes the operations
// persistence-sync needs against the ledgerline-persistence-sync schema.
type Store struct {
	pool *pgxpool.Pool
}

// New connects to Postgres at connString and verifies connectivity with a
// ping. Callers should call Close when done.
func New(ctx context.Context, connString string) (*Store, error) {
	pool, err := pgxpool.New(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// migrationSQL creates the schema this service owns. It is intentionally a
// simple idempotent "CREATE TABLE IF NOT EXISTS" migration, not a versioned
// migration tool, per the scope of this service. Table and column names
// here are a cross-repo contract: ledgerline-reporting and
// ledgerline-ai-gateway read from these same tables, so do not rename them.
const migrationSQL = `
CREATE TABLE IF NOT EXISTS journal_entries (
  id BIGSERIAL PRIMARY KEY,
  transfer_dedup_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  direction TEXT NOT NULL CHECK (direction IN ('debit','credit')),
  amount_minor BIGINT NOT NULL,
  currency TEXT NOT NULL,
  gl_account TEXT,
  cost_center TEXT,
  posted_at TIMESTAMPTZ NOT NULL,
  raft_index BIGINT NOT NULL,
  UNIQUE (transfer_dedup_id, account_id, direction)
);

CREATE TABLE IF NOT EXISTS account_balances (
  account_id TEXT PRIMARY KEY,
  currency TEXT NOT NULL,
  balance_minor BIGINT NOT NULL DEFAULT 0,
  last_raft_index BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sync_state (
  id INT PRIMARY KEY DEFAULT 1,
  last_raft_index BIGINT NOT NULL DEFAULT 0
);
`

// Migrate creates the schema (if not already present) and ensures the
// singleton sync_state row exists.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, migrationSQL); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	const ensureSyncState = `
		INSERT INTO sync_state (id, last_raft_index) VALUES (1, 0)
		ON CONFLICT (id) DO NOTHING
	`
	if _, err := s.pool.Exec(ctx, ensureSyncState); err != nil {
		return fmt.Errorf("store: ensure sync_state row: %w", err)
	}
	return nil
}

// LastRaftIndex returns the last successfully processed raft index, read
// fresh from the database every call (never cached in memory), so that
// reconnect/resume logic is always correct even across process restarts.
func (s *Store) LastRaftIndex(ctx context.Context) (int64, error) {
	var idx int64
	err := s.pool.QueryRow(ctx, `SELECT last_raft_index FROM sync_state WHERE id = 1`).Scan(&idx)
	if err != nil {
		return 0, fmt.Errorf("store: read last_raft_index: %w", err)
	}
	return idx, nil
}

// ApplyAccountCreated durably records an account_created event: it seeds a
// zero-balance account_balances row (if one doesn't already exist — an
// existing row's balance must never be reset, since the authoritative
// balance comes from replaying transfer_applied events, not from
// account_created) and advances sync_state.last_raft_index, all in one
// transaction.
func (s *Store) ApplyAccountCreated(ctx context.Context, ev events.LedgerEvent) error {
	seed, err := translate.AccountCreatedToSeed(ev)
	if err != nil {
		return fmt.Errorf("store: translate account_created: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if already committed

	const upsertBalance = `
		INSERT INTO account_balances (account_id, currency, balance_minor, last_raft_index, updated_at)
		VALUES ($1, $2, 0, 0, now())
		ON CONFLICT (account_id) DO NOTHING
	`
	if _, err := tx.Exec(ctx, upsertBalance, seed.AccountID, seed.Currency); err != nil {
		return fmt.Errorf("store: seed account_balances: %w", err)
	}

	if err := advanceSyncState(ctx, tx, ev.RaftIndex); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// ApplyTransferApplied durably records a transfer_applied event: two
// journal_entries rows (debit + credit), the resulting balance deltas
// (guarded against replay via last_raft_index), and the sync_state advance,
// all in a single transaction so a crash mid-processing can't half-apply an
// event.
//
// The transfer_applied wire event does not itself carry a currency field
// (currency is a property of the accounts, not the transfer). Per the
// coordinator's resolution, the authoritative currency for the journal rows
// is looked up from this service's own account_balances table within the
// same transaction — that row was seeded by the account's earlier
// account_created event, and the ledger engine's FSM guarantees the
// from/to/request currencies all match, so either account's row carries the
// right value. If neither account has a seeded balance row yet (should not
// happen in practice, since account_created always precedes transfers on an
// account), this falls back to an empty string and logs a warning rather
// than failing the whole sync.
func (s *Store) ApplyTransferApplied(ctx context.Context, ev events.LedgerEvent) error {
	debit, credit, err := translate.TransferToJournalRows(ev)
	if err != nil {
		return fmt.Errorf("store: translate transfer_applied journal rows: %w", err)
	}
	fromDelta, toDelta, err := translate.TransferToBalanceDeltas(ev)
	if err != nil {
		return fmt.Errorf("store: translate transfer_applied balance deltas: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if already committed

	currency, err := lookupTransferCurrency(ctx, tx, ev.FromAccount, ev.ToAccount)
	if err != nil {
		return err
	}
	if currency == "" {
		log.Printf("persistence-sync: WARNING: could not determine currency for transfer dedup_id=%q (from=%q to=%q raft_index=%d); neither account has a seeded account_balances row yet. Falling back to empty string for journal_entries.currency.",
			ev.DedupID, ev.FromAccount, ev.ToAccount, ev.RaftIndex)
	}
	debit.Currency = currency
	credit.Currency = currency

	const insertJournalRow = `
		INSERT INTO journal_entries
			(transfer_dedup_id, account_id, direction, amount_minor, currency, gl_account, cost_center, posted_at, raft_index)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (transfer_dedup_id, account_id, direction) DO NOTHING
	`
	for _, row := range []translate.JournalEntryRow{debit, credit} {
		_, err := tx.Exec(ctx, insertJournalRow,
			row.TransferDedupID, row.AccountID, string(row.Direction), row.AmountMinor,
			row.Currency, row.GLAccount, row.CostCenter, row.PostedAt, row.RaftIndex,
		)
		if err != nil {
			return fmt.Errorf("store: insert journal_entries row (%s/%s): %w", row.AccountID, row.Direction, err)
		}
	}

	// Guarded upsert: if the account_balances row does not exist yet, seed it
	// with this delta already applied. If it does exist, only apply the
	// delta when this event's raft_index is strictly newer than what's
	// already recorded, which makes replay/reconnect idempotent.
	const upsertBalanceDelta = `
		INSERT INTO account_balances (account_id, currency, balance_minor, last_raft_index, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (account_id) DO UPDATE
		SET balance_minor = account_balances.balance_minor + EXCLUDED.balance_minor,
			last_raft_index = EXCLUDED.last_raft_index,
			updated_at = now()
		WHERE account_balances.last_raft_index < EXCLUDED.last_raft_index
	`
	for _, delta := range []translate.BalanceDelta{fromDelta, toDelta} {
		_, err := tx.Exec(ctx, upsertBalanceDelta, delta.AccountID, currency, delta.DeltaMinor, delta.RaftIndex)
		if err != nil {
			return fmt.Errorf("store: upsert account_balances for %s: %w", delta.AccountID, err)
		}
	}

	if err := advanceSyncState(ctx, tx, ev.RaftIndex); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// lookupTransferCurrency finds the currency to stamp on a transfer's
// journal_entries rows by consulting account_balances for the from/to
// accounts involved, preferring from_account and falling back to
// to_account. Returns "" (not an error) if neither account has a row yet;
// callers decide how to handle that.
func lookupTransferCurrency(ctx context.Context, tx pgx.Tx, fromAccount, toAccount string) (string, error) {
	var currency string
	err := tx.QueryRow(ctx, `SELECT currency FROM account_balances WHERE account_id = $1`, fromAccount).Scan(&currency)
	if err == nil && currency != "" {
		return currency, nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("store: lookup currency for from_account %q: %w", fromAccount, err)
	}

	err = tx.QueryRow(ctx, `SELECT currency FROM account_balances WHERE account_id = $1`, toAccount).Scan(&currency)
	if err == nil {
		return currency, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return "", fmt.Errorf("store: lookup currency for to_account %q: %w", toAccount, err)
}

func advanceSyncState(ctx context.Context, tx pgx.Tx, raftIndex int64) error {
	const update = `UPDATE sync_state SET last_raft_index = $1 WHERE id = 1`
	if _, err := tx.Exec(ctx, update, raftIndex); err != nil {
		return fmt.Errorf("store: advance sync_state: %w", err)
	}
	return nil
}
