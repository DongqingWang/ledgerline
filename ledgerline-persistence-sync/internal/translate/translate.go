// Package translate contains pure, DB-free logic for turning ledger engine
// events into the rows / mutations that ledgerline-persistence-sync applies
// to Postgres. Keeping this logic free of any database or network calls
// makes it straightforward to unit test.
package translate

import (
	"errors"
	"fmt"
	"sort"
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
// LineIndex positions the leg within its journal (the uniqueness key is
// (transfer_dedup_id, line_index), which supports multi-leg journals where
// the same account can appear on several legs).
type JournalEntryRow struct {
	TransferDedupID string
	LineIndex       int
	AccountID       string
	Direction       Direction
	AmountMinor     int64
	Currency        string
	GLAccount       *string
	CostCenter      *string
	PostedAt        time.Time
	RaftIndex       int64
	ReversalOf      *string
}

// BalanceDelta describes a single account's balance mutation resulting from
// one transfer_applied event, guarded by the event's raft index.
type BalanceDelta struct {
	AccountID  string
	Currency   string
	DeltaMinor int64
	RaftIndex  int64
}

// AccountDelta is the aggregated per-account effect of one event: posted
// balance change and/or two-phase hold change. One event can touch an
// account on several journal legs; the deltas are netted here so the store
// applies exactly one guarded upsert per account.
type AccountDelta struct {
	AccountID    string
	Currency     string
	BalanceMinor int64
	PendingMinor int64
	RaftIndex    int64
}

// AccountBalanceSeed is the zero-balance row to upsert for a newly created
// account.
type AccountBalanceSeed struct {
	AccountID string
	Currency  string
	Type      string
	TenantID  string
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
		Type:      ev.AccountType,
		TenantID:  ev.TenantID,
	}, nil
}

// TransferToJournalRows converts a transfer_applied LedgerEvent into the two
// journal_entries rows (one debit, one credit) it produces. The wire event
// carries the currency; a missing currency is a contract violation and a
// hard error (never a silent empty-string fallback, which would poison
// per-currency reconciliation).
func TransferToJournalRows(ev events.LedgerEvent) (debit, credit JournalEntryRow, err error) {
	if ev.Kind != events.KindTransferApplied {
		return JournalEntryRow{}, JournalEntryRow{}, ErrWrongKind
	}
	if ev.Currency == "" {
		return JournalEntryRow{}, JournalEntryRow{}, fmt.Errorf("%w: transfer_applied dedup_id=%q", ErrMissingCurrency, ev.DedupID)
	}
	gl := nullableString(ev.GLAccount)
	cc := nullableString(ev.CostCenter)

	base := JournalEntryRow{
		TransferDedupID: ev.DedupID,
		AmountMinor:     ev.AmountMinor,
		Currency:        ev.Currency,
		GLAccount:       gl,
		CostCenter:      cc,
		PostedAt:        ev.Timestamp,
		RaftIndex:       ev.RaftIndex,
	}

	debit = base
	debit.AccountID = ev.FromAccount
	debit.Direction = Debit
	debit.LineIndex = 0

	credit = base
	credit.AccountID = ev.ToAccount
	credit.Direction = Credit
	credit.LineIndex = 1

	return debit, credit, nil
}

// JournalToRows converts a journal_applied LedgerEvent into one
// journal_entries row per leg, preserving per-leg GL dimensions and
// currencies, and linking reversals to the original journal id.
func JournalToRows(ev events.LedgerEvent) ([]JournalEntryRow, error) {
	if ev.Kind != events.KindJournalApplied {
		return nil, ErrWrongKind
	}
	if len(ev.Lines) < 2 {
		return nil, fmt.Errorf("translate: journal_applied dedup_id=%q has %d lines, want >= 2", ev.DedupID, len(ev.Lines))
	}
	var reversalOf *string
	if ev.ReversalOf != "" {
		reversalOf = s2ptr(ev.ReversalOf)
	}
	rows := make([]JournalEntryRow, 0, len(ev.Lines))
	for i, ln := range ev.Lines {
		if ln.Currency == "" {
			return nil, fmt.Errorf("%w: journal_applied dedup_id=%q line %d", ErrMissingCurrency, ev.DedupID, i)
		}
		dir := Debit
		if ln.Direction == "credit" {
			dir = Credit
		} else if ln.Direction != "debit" {
			return nil, fmt.Errorf("translate: journal_applied dedup_id=%q line %d: invalid direction %q", ev.DedupID, i, ln.Direction)
		}
		rows = append(rows, JournalEntryRow{
			TransferDedupID: ev.DedupID,
			LineIndex:       i,
			AccountID:       ln.AccountID,
			Direction:       dir,
			AmountMinor:     ln.AmountMinor,
			Currency:        ln.Currency,
			GLAccount:       nullableString(ln.GLAccount),
			CostCenter:      nullableString(ln.CostCenter),
			PostedAt:        ev.Timestamp,
			RaftIndex:       ev.RaftIndex,
			ReversalOf:      reversalOf,
		})
	}
	return rows, nil
}

// EventToAccountDeltas nets every balance/hold effect of one event into a
// single sorted slice of per-account deltas. Expired holds carried on the
// event are released first, then the event body is applied. rejected /
// duplicate / account_created events produce no deltas.
func EventToAccountDeltas(ev events.LedgerEvent) ([]AccountDelta, error) {
	agg := make(map[string]*AccountDelta)
	add := func(accountID, currency string, balance, pending int64) {
		d, ok := agg[accountID]
		if !ok {
			d = &AccountDelta{AccountID: accountID, Currency: currency, RaftIndex: ev.RaftIndex}
			agg[accountID] = d
		}
		d.BalanceMinor += balance
		d.PendingMinor += pending
	}

	for _, xp := range ev.ExpiredPendings {
		add(xp.FromAccount, xp.Currency, 0, -xp.AmountMinor)
	}

	switch ev.Kind {
	case events.KindTransferApplied:
		if ev.Currency == "" {
			return nil, fmt.Errorf("%w: transfer_applied dedup_id=%q", ErrMissingCurrency, ev.DedupID)
		}
		add(ev.FromAccount, ev.Currency, -ev.AmountMinor, 0)
		add(ev.ToAccount, ev.Currency, ev.AmountMinor, 0)
	case events.KindJournalApplied:
		for i, ln := range ev.Lines {
			if ln.Currency == "" {
				return nil, fmt.Errorf("%w: journal_applied dedup_id=%q line %d", ErrMissingCurrency, ev.DedupID, i)
			}
			if ln.Direction == "debit" {
				add(ln.AccountID, ln.Currency, -ln.AmountMinor, 0)
			} else {
				add(ln.AccountID, ln.Currency, ln.AmountMinor, 0)
			}
		}
	case events.KindPendingCreated:
		add(ev.FromAccount, ev.Currency, 0, ev.AmountMinor)
	case events.KindPendingPosted:
		add(ev.FromAccount, ev.Currency, -ev.AmountMinor, -ev.AmountMinor)
		add(ev.ToAccount, ev.Currency, ev.AmountMinor, 0)
	case events.KindPendingVoided:
		add(ev.FromAccount, ev.Currency, 0, -ev.AmountMinor)
	case events.KindAccountCreated, events.KindCommandRejected, events.KindCommandDuplicate:
		// No balance/hold effect.
	default:
		return nil, fmt.Errorf("translate: unknown event kind %q at raft_index=%d", ev.Kind, ev.RaftIndex)
	}

	out := make([]AccountDelta, 0, len(agg))
	for _, d := range agg {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
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
