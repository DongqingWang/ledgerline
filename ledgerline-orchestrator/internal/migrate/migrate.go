// Package migrate contains the pure, HTTP-free logic for deciding which
// accounts and transfers belong to a shard split, given the ordered event
// history of the old cluster. Keeping this logic free of I/O makes it
// straightforward to unit test with fixed, fake event slices.
package migrate

import "github.com/ledgerline/ledgerline-orchestrator/internal/ledgerclient"

// Plan is the result of filtering an event history down to the accounts
// and transfers that need to move to the new shard.
type Plan struct {
	// Accounts lists the IDs of accounts that fall in the target range,
	// in the order they were first observed in the event stream (either
	// via their account_created event, or - for the defensive edge case
	// of an account with no visible account_created event - the first
	// transfer_applied event that mentions them).
	Accounts []string

	// Transfers lists, in original order, every transfer_applied event
	// where BOTH from_account and to_account fall in the target range.
	// These are safe to replay verbatim (same dedup_id) on the new
	// cluster.
	Transfers []ledgerclient.Event

	// SkippedCrossShard lists transfer_applied events where exactly one
	// side falls in the target range. This demo does not support
	// cross-shard transfers; these are reported so the caller can log a
	// warning, but they are not replayed.
	SkippedCrossShard []ledgerclient.Event
}

// InRangeFunc decides whether an account ID belongs to the range being
// migrated.
type InRangeFunc func(accountID string) bool

// BuildPlan walks events (assumed to be in ascending raft_index / stream
// order, as delivered by GET /v1/events/stream) and produces a Plan of
// everything that belongs to the range selected by inRange.
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
		case ledgerclient.EventKindTransferApplied:
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
		}
	}
	return plan
}
