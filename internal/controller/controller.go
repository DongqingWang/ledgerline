// Package controller implements the demo shard-split workflow: health
// check the new cluster, replay the upper range's history into it, and
// only then atomically swap the routing table. See the package-level
// README for the simplifications this demo makes relative to a real
// live-migration system.
package controller

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ledgerline/ledgerline-orchestrator/internal/ledgerclient"
	"github.com/ledgerline/ledgerline-orchestrator/internal/migrate"
	"github.com/ledgerline/ledgerline-orchestrator/internal/shard"
)

// DefaultStreamIdleTimeout is how long StreamEvents waits for a new event
// before deciding the backlog replay has caught up. See the
// ledgerclient.Client.StreamEvents doc comment for the heuristic this
// implements.
const DefaultStreamIdleTimeout = 750 * time.Millisecond

// SplitRequest mirrors the JSON body of POST /v1/split.
type SplitRequest struct {
	SplitPoint          uint32   `json:"split_point"`
	NewShardLedgerNodes []string `json:"new_shard_ledger_nodes"`
}

// SplitResponse mirrors the JSON body returned by POST /v1/split.
type SplitResponse struct {
	Status            string `json:"status"` // "applied" or "aborted"
	Reason            string `json:"reason,omitempty"`
	MigratedAccounts  int    `json:"migrated_accounts"`
	MigratedTransfers int    `json:"migrated_transfers"`
}

// Orchestrator holds the live routing table and coordinates split
// operations against live ledgerline-raft clusters. Splits are processed
// one at a time, synchronously - see README "Known simplifications".
type Orchestrator struct {
	Table       *shard.Table
	Client      *ledgerclient.Client
	Logger      *log.Logger
	IdleTimeout time.Duration

	mu sync.Mutex
}

// New creates an Orchestrator around an existing routing table.
func New(table *shard.Table, client *ledgerclient.Client, logger *log.Logger) *Orchestrator {
	return &Orchestrator{
		Table:       table,
		Client:      client,
		Logger:      logger,
		IdleTimeout: DefaultStreamIdleTimeout,
	}
}

func (o *Orchestrator) logf(format string, args ...interface{}) {
	if o.Logger != nil {
		o.Logger.Printf(format, args...)
	}
}

func abort(reason string) SplitResponse {
	return SplitResponse{Status: "aborted", Reason: reason}
}

// Split runs the full split workflow described in the package doc. It
// only mutates the routing table on full success; any failure along the
// way leaves the table untouched and returns an "aborted" response - the
// caller can simply discard/ignore the (partially or fully provisioned)
// new cluster.
func (o *Orchestrator) Split(ctx context.Context, req SplitRequest) SplitResponse {
	// Splits are processed one at a time, synchronously - this is a
	// deliberate simplification for this demo (see README).
	o.mu.Lock()
	defer o.mu.Unlock()

	if len(req.NewShardLedgerNodes) == 0 {
		return abort("new_shard_ledger_nodes must not be empty")
	}

	oldShard, ok := o.Table.ShardForKey(req.SplitPoint)
	if !ok {
		return abort(fmt.Sprintf("split_point %d is not covered by any existing shard", req.SplitPoint))
	}
	if uint64(req.SplitPoint) == uint64(oldShard.RangeStart) {
		return abort(fmt.Sprintf("split_point %d equals the owning shard's start (%d); nothing to split", req.SplitPoint, oldShard.RangeStart))
	}

	// Step (a): health-check the new cluster before doing anything else.
	for _, addr := range req.NewShardLedgerNodes {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		health, err := o.Client.Health(hctx, addr)
		cancel()
		if err != nil {
			return abort(fmt.Sprintf("health check failed for new node %s: %v", addr, err))
		}
		switch health.State {
		case "Leader", "Follower", "Candidate":
			// sane raft state, continue
		default:
			return abort(fmt.Sprintf("new node %s reported unexpected state %q", addr, health.State))
		}
	}

	// Step (b): find the old cluster's leader, so we read/replay against
	// an authoritative, up-to-date node.
	oldLeaderAddr, err := o.Client.LeaderAddrAmong(ctx, oldShard.LedgerNodes)
	if err != nil {
		return abort(fmt.Sprintf("could not determine old cluster leader: %v", err))
	}

	events, err := o.Client.StreamEvents(ctx, oldLeaderAddr, 0, o.IdleTimeout)
	if err != nil {
		return abort(fmt.Sprintf("failed to read old cluster event history from %s: %v", oldLeaderAddr, err))
	}

	inRange := func(accountID string) bool {
		return shard.Shard{RangeStart: req.SplitPoint, RangeEnd: oldShard.RangeEnd}.Contains(shard.ShardFor(accountID))
	}
	plan := migrate.BuildPlan(events, inRange)

	for _, skipped := range plan.SkippedCrossShard {
		o.logf("WARN: skipping cross-shard transfer %s (from=%s to=%s): not supported by this demo split", skipped.DedupID, skipped.FromAccount, skipped.ToAccount)
	}

	// Step (b, continued): find the new cluster's leader to write to.
	newLeaderAddr, err := o.Client.LeaderAddrAmong(ctx, req.NewShardLedgerNodes)
	if err != nil {
		return abort(fmt.Sprintf("could not determine new cluster leader: %v", err))
	}

	// Step (c): recreate accounts on the new cluster, preserving the
	// unlimited flag read from the old cluster (the event stream does not
	// carry it). We also remember each account's currency here: the
	// transfer_applied event itself does not carry a currency field (see
	// the wire contract), so we reuse the currency observed on the
	// account when replaying transfers below.
	accountCurrency := make(map[string]string, len(plan.Accounts))
	for _, accountID := range plan.Accounts {
		acct, err := o.Client.GetAccount(ctx, oldLeaderAddr, accountID)
		if err != nil {
			return abort(fmt.Sprintf("failed to read account %s from old cluster: %v", accountID, err))
		}
		accountCurrency[accountID] = acct.Currency
		_, err = o.Client.CreateAccount(ctx, newLeaderAddr, ledgerclient.CreateAccountRequest{
			AccountID: acct.AccountID,
			Currency:  acct.Currency,
			Unlimited: acct.Unlimited,
		})
		if err != nil {
			return abort(fmt.Sprintf("failed to create account %s on new cluster: %v", accountID, err))
		}
	}

	// Step (d): replay same-shard transfers, in original order.
	for _, ev := range plan.Transfers {
		_, err := o.Client.SubmitTransfer(ctx, newLeaderAddr, ledgerclient.TransferRequest{
			DedupID:     ev.DedupID,
			FromAccount: ev.FromAccount,
			ToAccount:   ev.ToAccount,
			AmountMinor: ev.AmountMinor,
			Currency:    accountCurrency[ev.FromAccount],
			GLAccount:   ev.GLAccount,
			CostCenter:  ev.CostCenter,
		})
		if err != nil {
			return abort(fmt.Sprintf("failed to replay transfer %s on new cluster: %v", ev.DedupID, err))
		}
	}

	// Step (f): everything succeeded - atomically swap the routing table.
	if err := o.Table.Split(req.SplitPoint, req.NewShardLedgerNodes); err != nil {
		// Should not happen since we validated the split point above, but
		// guard against races/bugs rather than silently reporting success.
		return abort(fmt.Sprintf("post-migration routing table update failed: %v", err))
	}

	return SplitResponse{
		Status:            "applied",
		MigratedAccounts:  len(plan.Accounts),
		MigratedTransfers: len(plan.Transfers),
	}
}
