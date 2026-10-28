// Package translate contains pure, DB-free logic for turning ledger engine
// events into the rows / mutations that ledgerline-persistence-sync applies
// to Postgres. Keeping this logic free of any database or network calls
// makes it straightforward to unit test.
package translate

import (
	"errors"
	"time"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
)

// Direction mirrors the journal_entries.direction CHECK constraint.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// JournalEntryRow mirrors one row to be inserted into journal_entries.
type JournalEntryRow struct {
	TransferDedupID string
	AccountID       string
	Direction       Direction
	AmountMinor     int64
	Currency        string
	GLAccount       *string
	CostCenter      *string
	PostedAt        time.Time
	RaftIndex       int64
}

// BalanceDelta describes a single account's balance mutation resulting from
// one transfer_applied event, guarded by the event's raft index.
type BalanceDelta struct {
	AccountID  string
	Currency   string
	DeltaMinor int64
	RaftIndex  int64
}

// AccountBalanceSeed is the zero-balance row to upsert for a newly created
// account.
type AccountBalanceSeed struct {
	AccountID string
	Currency  string
}

var (
	// ErrWrongKind is returned when a translation function is called with an
	// event of the kind it does not handle.
	ErrWrongKind = errors.New("translate: event has unexpected kind")
	// ErrMissingCurrency is returned when an account_created event lacks a
	// currency, which should never happen per the documented wire contract.
	ErrMissingCurrency = errors.New("translate: account_created event missing currency")
)

// nullableString turns an empty string into a nil pointer, since the
// journal_entries.gl_account / cost_center columns are nullable and the
// wire contract uses omitempty (zero value == "not present") for them.
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return s2ptr(s)
}

func s2ptr(s string) *string {
	v := s
	return &v
}

// AccountCreatedToSeed converts an account_created LedgerEvent into the
// zero-balance seed row for account_balances.
func AccountCreatedToSeed(ev events.LedgerEvent) (AccountBalanceSeed, error) {
	if ev.Kind != events.KindAccountCreated {
		return AccountBalanceSeed{}, ErrWrongKind
	}
	if ev.Currency == "" {
		return AccountBalanceSeed{}, ErrMissingCurrency
	}
	return AccountBalanceSeed{
		AccountID: ev.AccountID,
		Currency:  ev.Currency,
	}, nil
}

// TransferToJournalRows converts a transfer_applied LedgerEvent into the two
// journal_entries rows (one debit, one credit) it produces.
func TransferToJournalRows(ev events.LedgerEvent) (debit, credit JournalEntryRow, err error) {
	if ev.Kind != events.KindTransferApplied {
		return JournalEntryRow{}, JournalEntryRow{}, ErrWrongKind
	}
	gl := nullableString(ev.GLAccount)
	cc := nullableString(ev.CostCenter)

	base := JournalEntryRow{
		TransferDedupID: ev.DedupID,
		AmountMinor:     ev.AmountMinor,
		Currency:        currencyForTransfer(ev),
		GLAccount:       gl,
		CostCenter:      cc,
		PostedAt:        ev.Timestamp,
		RaftIndex:       ev.RaftIndex,
	}

	debit = base
	debit.AccountID = ev.FromAccount
	debit.Direction = Debit

	credit = base
	credit.AccountID = ev.ToAccount
	credit.Direction = Credit

	return debit, credit, nil
}

// currencyForTransfer isolates the (currently trivial) decision of which
// currency to stamp on journal_entries rows for a transfer. The
// transfer_applied event itself does not carry a currency field in the
// documented contract (currency is a property of the accounts involved, not
// the transfer), so callers that need the authoritative currency should look
// it up from account_balances. Here we default to empty string, leaving it
// to the caller (store layer) to fill in from the accounts table if a
// stricter guarantee is needed. This keeps the pure translation function
// honest about what the event itself actually carries.
func currencyForTransfer(ev events.LedgerEvent) string {
	return ""
}

// TransferToBalanceDeltas converts a transfer_applied LedgerEvent into the
// two balance deltas (debit side negative, credit side positive) it implies.
func TransferToBalanceDeltas(ev events.LedgerEvent) (from, to BalanceDelta, err error) {
	if ev.Kind != events.KindTransferApplied {
		return BalanceDelta{}, BalanceDelta{}, ErrWrongKind
	}
	from = BalanceDelta{
		AccountID:  ev.FromAccount,
		DeltaMinor: -ev.AmountMinor,
		RaftIndex:  ev.RaftIndex,
	}
	to = BalanceDelta{
		AccountID:  ev.ToAccount,
		DeltaMinor: ev.AmountMinor,
		RaftIndex:  ev.RaftIndex,
	}
	return from, to, nil
}

// ShouldApplyBalanceUpdate implements the replay/idempotency guard: a
// balance mutation for a given raft index should only be applied if that
// index is strictly greater than the last-applied index recorded on the
// account_balances row. This prevents double-applying a transfer when the
// stream is replayed (e.g. after a restart or reconnect).
func ShouldApplyBalanceUpdate(eventRaftIndex, lastAppliedRaftIndex int64) bool {
	return eventRaftIndex > lastAppliedRaftIndex
}
