package migrate

import (
	"reflect"
	"testing"

	"github.com/ledgerline/ledgerline-orchestrator/internal/ledgerclient"
)

// inSet builds an InRangeFunc from a fixed set of account IDs, standing in
// for a real shardFor-based range check in these pure-logic tests.
func inSet(ids ...string) InRangeFunc {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return func(accountID string) bool { return m[accountID] }
}

func TestBuildPlanBasicAccountsAndTransfers(t *testing.T) {
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "alice", Currency: "USD"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindAccountCreated, AccountID: "bob", Currency: "USD"},
		{RaftIndex: 3, Kind: ledgerclient.EventKindAccountCreated, AccountID: "carol", Currency: "USD"},
		{RaftIndex: 4, Kind: ledgerclient.EventKindTransferApplied, DedupID: "t1", FromAccount: "alice", ToAccount: "bob", AmountMinor: 100},
	}
	// alice and bob are in the migrated range; carol is not.
	plan := BuildPlan(events, inSet("alice", "bob"))

	if !reflect.DeepEqual(plan.Accounts, []string{"alice", "bob"}) {
		t.Errorf("Accounts = %v, want [alice bob]", plan.Accounts)
	}
	if len(plan.Transfers) != 1 || plan.Transfers[0].DedupID != "t1" {
		t.Errorf("Transfers = %+v, want one transfer t1", plan.Transfers)
	}
	if len(plan.SkippedCrossShard) != 0 {
		t.Errorf("SkippedCrossShard = %+v, want none", plan.SkippedCrossShard)
	}
}

func TestBuildPlanCrossShardTransferIsSkippedNotMigrated(t *testing.T) {
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "alice"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindAccountCreated, AccountID: "dave"},
		{RaftIndex: 3, Kind: ledgerclient.EventKindTransferApplied, DedupID: "cross1", FromAccount: "alice", ToAccount: "dave", AmountMinor: 50},
	}
	// Only alice is in the migrated range; dave stays on the old cluster.
	plan := BuildPlan(events, inSet("alice"))

	if !reflect.DeepEqual(plan.Accounts, []string{"alice"}) {
		t.Errorf("Accounts = %v, want [alice]", plan.Accounts)
	}
	if len(plan.Transfers) != 0 {
		t.Errorf("Transfers = %+v, want none (cross-shard must not be replayed)", plan.Transfers)
	}
	if len(plan.SkippedCrossShard) != 1 || plan.SkippedCrossShard[0].DedupID != "cross1" {
		t.Errorf("SkippedCrossShard = %+v, want one entry cross1", plan.SkippedCrossShard)
	}
}

func TestBuildPlanNoAccountsInRangeYieldsEmptyPlan(t *testing.T) {
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "alice"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindAccountCreated, AccountID: "bob"},
		{RaftIndex: 3, Kind: ledgerclient.EventKindTransferApplied, DedupID: "t1", FromAccount: "alice", ToAccount: "bob"},
	}
	plan := BuildPlan(events, inSet("nobody"))
	if len(plan.Accounts) != 0 || len(plan.Transfers) != 0 || len(plan.SkippedCrossShard) != 0 {
		t.Errorf("expected empty plan, got %+v", plan)
	}
}

func TestBuildPlanAccountWithNoCreatedEventIsStillObservedViaTransfer(t *testing.T) {
	// Defensive edge case: an account_id that only ever appears as a
	// transfer participant (no account_created event seen in this
	// range, e.g. "external-deposit"-style accounts) must still be
	// collected into the Accounts list per the spec.
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "bob"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindTransferApplied, DedupID: "t1", FromAccount: "external-deposit", ToAccount: "bob", AmountMinor: 100},
	}
	plan := BuildPlan(events, inSet("external-deposit", "bob"))
	if !reflect.DeepEqual(plan.Accounts, []string{"bob", "external-deposit"}) {
		t.Errorf("Accounts = %v, want [bob external-deposit]", plan.Accounts)
	}
	if len(plan.Transfers) != 1 {
		t.Errorf("Transfers = %+v, want 1", plan.Transfers)
	}
}

func TestBuildPlanDuplicateAccountCreatedEventsAreDeduped(t *testing.T) {
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "alice"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindAccountCreated, AccountID: "alice"}, // shouldn't happen in practice, but be robust
	}
	plan := BuildPlan(events, inSet("alice"))
	if !reflect.DeepEqual(plan.Accounts, []string{"alice"}) {
		t.Errorf("Accounts = %v, want [alice] (deduped)", plan.Accounts)
	}
}

func TestBuildPlanPreservesTransferOrder(t *testing.T) {
	events := []ledgerclient.Event{
		{RaftIndex: 1, Kind: ledgerclient.EventKindAccountCreated, AccountID: "a"},
		{RaftIndex: 2, Kind: ledgerclient.EventKindAccountCreated, AccountID: "b"},
		{RaftIndex: 3, Kind: ledgerclient.EventKindTransferApplied, DedupID: "second", FromAccount: "a", ToAccount: "b"},
		{RaftIndex: 4, Kind: ledgerclient.EventKindTransferApplied, DedupID: "third", FromAccount: "b", ToAccount: "a"},
	}
	plan := BuildPlan(events, inSet("a", "b"))
	gotOrder := []string{plan.Transfers[0].DedupID, plan.Transfers[1].DedupID}
	if !reflect.DeepEqual(gotOrder, []string{"second", "third"}) {
		t.Errorf("transfer order = %v, want [second third]", gotOrder)
	}
}
