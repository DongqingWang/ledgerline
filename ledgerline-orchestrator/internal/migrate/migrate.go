// Package migrate contains the pure, HTTP-free logic for deciding which
// accounts and balance movements belong to a shard split, given the
// ordered event history of the old cluster. Keeping this logic free of
// I/O makes it straightforward to unit test with fixed, fake event slices.
package migrate

import "github.com/ledgerline/ledgerline-orchestrator/internal/ledgerclient"

// Plan is the result of filtering an event history down to the accounts
// and balance movements that need to move to the new shard.
type Plan struct {
	// Accounts lists the IDs of accounts that fall in the target range,
	// in the order they were first observed in the event stream (either
	// via their account_created event, or - for the defensive edge case
	// of an account with no visible account_created event - the first
	// balance-moving event that mentions them).
	Accounts []string

	// Transfers lists, in original order, every transfer_applied or
	// pending_posted event where BOTH from_account and to_account fall
	// in the target range. These are safe to replay verbatim (same
	// dedup_id) on the new cluster; a posted hold is replayed as an
	// ordinary transfer since its balance effect is identical.
	Transfers []ledgerclient.Event

	// Journals lists, in original order, every journal_applied event
	// (including reversal journals) where ALL legs' accounts fall in the
	// target range. Replayed verbatim as multi-leg journals.
	Journals []ledgerclient.Event

	// SkippedCrossShard lists balance-moving events where only SOME of
	// the participating accounts fall in the target range. This demo
	// does not support cross-shard movements; these are reported so the
	// caller can log a warning, but they are not replayed.
	SkippedCrossShard []ledgerclient.Event
}

// InRangeFunc decides whether an account ID belongs to the range being
// migrated.
type InRangeFunc func(accountID string) bool

// BuildPlan walks events (assumed to be in ascending raft_index / stream
// order, as delivered by GET /v1/events/stream) and produces a Plan of
// everything that belongs to the range selected by inRange.
//
// pending_created / pending_voided events are deliberately ignored:
// in-flight holds are not migrated by this demo split (a hold is a
// reservation, not a balance movement, and recreating it on the new
// cluster would need its own expiry clock). Only posted holds — which
// did move balances — are replayed, as transfers. command_rejected and
// command_duplicate events carry no balance effect and are skipped.
func BuildPlan(events []ledgerclient.Event, inRange InRangeFunc) Plan {
	var plan Plan
	seen := make(map[string]bool)

	observe := func(accountID string) {
		if accountID == "" || seen[accountID] || !inRange(accountID) {
			return
		}
		seen[accountID] = true
		plan.Accounts = append(plan.Accounts, accountID)
	}

	for _, ev := range events {
		switch ev.Kind {
		case ledgerclient.EventKindAccountCreated:
			observe(ev.AccountID)
		case ledgerclient.EventKindTransferApplied, ledgerclient.EventKindPendingPosted:
			// Observe both sides first (in from/to order) so an account
			// that only ever appears as a transfer participant still gets
			// migrated, per spec.
			observe(ev.FromAccount)
			observe(ev.ToAccount)

			fromIn := inRange(ev.FromAccount)
			toIn := inRange(ev.ToAccount)
			switch {
			case fromIn && toIn:
				plan.Transfers = append(plan.Transfers, ev)
			case fromIn || toIn:
				plan.SkippedCrossShard = append(plan.SkippedCrossShard, ev)
			}
		case ledgerclient.EventKindJournalApplied:
			allIn, anyIn := true, false
			for _, line := range ev.Lines {
				observe(line.AccountID)
				if inRange(line.AccountID) {
					anyIn = true
				} else {
					allIn = false
				}
			}
			switch {
			case allIn && len(ev.Lines) > 0:
				plan.Journals = append(plan.Journals, ev)
			case anyIn:
				plan.SkippedCrossShard = append(plan.SkippedCrossShard, ev)
			}
		}
	}
	return plan
}
