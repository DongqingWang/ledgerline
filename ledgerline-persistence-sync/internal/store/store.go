// Package store implements the Postgres-backed durable projection that
// ledgerline-persistence-sync maintains. All SQL lives here; the
// translation of ledger events into row shapes lives in the DB-free
// internal/translate package so that logic can be unit tested without a
// live database.
package store

import (
	"context"
	"fmt"

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
// simple idempotent migration, not a versioned migration tool, per the scope
// of this service. Table and column names here are a cross-repo contract:
// ledgerline-reporting and ledgerline-ai-gateway read from these same
// tables, so do not rename them.
//
// v2 changes: journal_entries gains line_index + reversal_of and its
// uniqueness key becomes (transfer_dedup_id, line_index) so multi-leg
// journals can touch the same account twice; account_balances gains
// pending_minor (two-phase holds), account_type, and tenant_id.
const migrationSQL = `
CREATE TABLE IF NOT EXISTS journal_entries (
  id BIGSERIAL PRIMARY KEY,
  transfer_dedup_id TEXT NOT NULL,
  line_index INT NOT NULL DEFAULT 0,
  account_id TEXT NOT NULL,
  direction TEXT NOT NULL CHECK (direction IN ('debit','credit')),
  amount_minor BIGINT NOT NULL,
  currency TEXT NOT NULL,
  gl_account TEXT,
  cost_center TEXT,
  posted_at TIMESTAMPTZ NOT NULL,
  raft_index BIGINT NOT NULL,
  reversal_of TEXT
);

ALTER TABLE journal_entries ADD COLUMN IF NOT EXISTS line_index INT NOT NULL DEFAULT 0;
ALTER TABLE journal_entries ADD COLUMN IF NOT EXISTS reversal_of TEXT;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'journal_entries_transfer_dedup_id_account_id_direction_key'
  ) THEN
    -- Pre-v2 rows are all two-party transfers: the credit leg is line 1.
    UPDATE journal_entries SET line_index = 1 WHERE direction = 'credit';
    ALTER TABLE journal_entries
      DROP CONSTRAINT journal_entries_transfer_dedup_id_account_id_direction_key;
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS journal_entries_dedup_line_idx
  ON journal_entries (transfer_dedup_id, line_index);

CREATE TABLE IF NOT EXISTS account_balances (
  account_id TEXT PRIMARY KEY,
  currency TEXT NOT NULL,
  balance_minor BIGINT NOT NULL DEFAULT 0,
  pending_minor BIGINT NOT NULL DEFAULT 0,
  account_type TEXT,
  tenant_id TEXT,
  last_raft_index BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE account_balances ADD COLUMN IF NOT EXISTS pending_minor BIGINT NOT NULL DEFAULT 0;
ALTER TABLE account_balances ADD COLUMN IF NOT EXISTS account_type TEXT;
ALTER TABLE account_balances ADD COLUMN IF NOT EXISTS tenant_id TEXT;

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
// balance comes from replaying balance-effect events, not from
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
		INSERT INTO account_balances (account_id, currency, balance_minor, account_type, tenant_id, last_raft_index, updated_at)
		VALUES ($1, $2, 0, $3, $4, 0, now())
		ON CONFLICT (account_id) DO NOTHING
	`
	if _, err := tx.Exec(ctx, upsertBalance, seed.AccountID, seed.Currency, nullableText(seed.Type), nullableText(seed.TenantID)); err != nil {
		return fmt.Errorf("store: seed account_balances: %w", err)
	}

	// An account_created entry can still carry expired holds swept during
	// its application.
	if err := applyDeltasAndAdvance(ctx, tx, ev); err != nil {
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
// event. The currency comes from the wire event itself; a missing currency
// is a hard error, never a silent empty-string fallback.
func (s *Store) ApplyTransferApplied(ctx context.Context, ev events.LedgerEvent) error {
	debit, credit, err := translate.TransferToJournalRows(ev)
	if err != nil {
		return fmt.Errorf("store: translate transfer_applied journal rows: %w", err)
	}
	return s.applyEvent(ctx, ev, []translate.JournalEntryRow{debit, credit})
}

// ApplyJournalApplied durably records a multi-leg journal_applied event:
// one journal_entries row per leg (with per-leg GL dimensions, currency,
// and reversal linkage), netted per-account balance deltas, and the
// sync_state advance, all in one transaction.
func (s *Store) ApplyJournalApplied(ctx context.Context, ev events.LedgerEvent) error {
	rows, err := translate.JournalToRows(ev)
	if err != nil {
		return fmt.Errorf("store: translate journal_applied rows: %w", err)
	}
	return s.applyEvent(ctx, ev, rows)
}

// ApplyPendingEvent durably records pending_created / pending_posted /
// pending_voided: hold reservations/releases and (for posts) the posted
// balance movement plus the post's two journal legs, so journal_entries
// remains the complete audit trail.
func (s *Store) ApplyPendingEvent(ctx context.Context, ev events.LedgerEvent) error {
	var rows []translate.JournalEntryRow
	if ev.Kind == events.KindPendingPosted {
		if ev.Currency == "" {
			return fmt.Errorf("store: pending_posted dedup_id=%q missing currency", ev.DedupID)
		}
		base := translate.JournalEntryRow{
			TransferDedupID: ev.DedupID,
			AmountMinor:     ev.AmountMinor,
			Currency:        ev.Currency,
			PostedAt:        ev.Timestamp,
			RaftIndex:       ev.RaftIndex,
		}
		debit, credit := base, base
		debit.AccountID, debit.Direction, debit.LineIndex = ev.FromAccount, translate.Debit, 0
		credit.AccountID, credit.Direction, credit.LineIndex = ev.ToAccount, translate.Credit, 1
		rows = []translate.JournalEntryRow{debit, credit}
	}
	return s.applyEvent(ctx, ev, rows)
}

// ApplyNoOp advances sync_state for events with no balance effect
// (command_rejected / command_duplicate), still applying any expired holds
// that rode along on the entry. Advancing the watermark for these is what
// keeps the durable position 1:1 with raft indices.
func (s *Store) ApplyNoOp(ctx context.Context, ev events.LedgerEvent) error {
	return s.applyEvent(ctx, ev, nil)
}

func (s *Store) applyEvent(ctx context.Context, ev events.LedgerEvent, rows []translate.JournalEntryRow) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op if already committed

	const insertJournalRow = `
		INSERT INTO journal_entries
			(transfer_dedup_id, line_index, account_id, direction, amount_minor, currency, gl_account, cost_center, posted_at, raft_index, reversal_of)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (transfer_dedup_id, line_index) DO NOTHING
	`
	for _, row := range rows {
		_, err := tx.Exec(ctx, insertJournalRow,
			row.TransferDedupID, row.LineIndex, row.AccountID, string(row.Direction), row.AmountMinor,
			row.Currency, row.GLAccount, row.CostCenter, row.PostedAt, row.RaftIndex, row.ReversalOf,
		)
		if err != nil {
			return fmt.Errorf("store: insert journal_entries row (%s line %d): %w", row.AccountID, row.LineIndex, err)
		}
	}

	if err := applyDeltasAndAdvance(ctx, tx, ev); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// applyDeltasAndAdvance applies the event's netted per-account deltas with
// the replay guard, then advances sync_state — inside the caller's tx.
func applyDeltasAndAdvance(ctx context.Context, tx pgx.Tx, ev events.LedgerEvent) error {
	deltas, err := translate.EventToAccountDeltas(ev)
	if err != nil {
		return fmt.Errorf("store: translate deltas: %w", err)
	}

	// Guarded upsert: if the account_balances row does not exist yet, seed it
	// with this delta already applied. If it does exist, only apply the
	// delta when this event's raft_index is strictly newer than what's
	// already recorded, which makes replay/reconnect idempotent.
	const upsertBalanceDelta = `
		INSERT INTO account_balances (account_id, currency, balance_minor, pending_minor, last_raft_index, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (account_id) DO UPDATE
		SET balance_minor = account_balances.balance_minor + EXCLUDED.balance_minor,
			pending_minor = account_balances.pending_minor + EXCLUDED.pending_minor,
			last_raft_index = EXCLUDED.last_raft_index,
			updated_at = now()
		WHERE account_balances.last_raft_index < EXCLUDED.last_raft_index
	`
	for _, d := range deltas {
		_, err := tx.Exec(ctx, upsertBalanceDelta, d.AccountID, d.Currency, d.BalanceMinor, d.PendingMinor, d.RaftIndex)
		if err != nil {
			return fmt.Errorf("store: upsert account_balances for %s: %w", d.AccountID, err)
		}
	}

	return advanceSyncState(ctx, tx, ev.RaftIndex)
}

func nullableText(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func advanceSyncState(ctx context.Context, tx pgx.Tx, raftIndex int64) error {
	const update = `UPDATE sync_state SET last_raft_index = $1 WHERE id = 1`
	if _, err := tx.Exec(ctx, update, raftIndex); err != nil {
		return fmt.Errorf("store: advance sync_state: %w", err)
	}
	return nil
}
