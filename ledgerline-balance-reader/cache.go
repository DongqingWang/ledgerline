package main

import (
	"fmt"
	"sync"
)

// Balance is the cached, read-optimized view of a single account's balance.
// BalanceMinor is the posted balance; PendingMinor is the sum of active
// two-phase holds; AvailableMinor = posted - pending is what new debits
// may draw against (mirrors the engine's own semantics).
type Balance struct {
	AccountID      string `json:"account_id"`
	Currency       string `json:"currency"`
	BalanceMinor   int64  `json:"balance_minor"`
	PendingMinor   int64  `json:"pending_minor"`
	AvailableMinor int64  `json:"available_minor"`
}

// BalanceCache is an in-memory, mutex-guarded read model built by replaying
// LedgerEvents from the ledgerline-raft committed event stream. This is the
// entire point of the service: once an event has been applied here, reading
// a balance is a plain in-process map lookup -- no network hop to the raft
// cluster, no consensus round, no per-request work against the ledger
// engine's own read path.
type BalanceCache struct {
	mu                 sync.RWMutex
	balances           map[string]*Balance
	lastProcessedIndex uint64
	haveProcessed      bool
}

// NewBalanceCache creates an empty cache.
func NewBalanceCache() *BalanceCache {
	return &BalanceCache{
		balances: make(map[string]*Balance),
	}
}

// Apply applies a single LedgerEvent to the cache. It returns an error when
// the event reveals a delivery GAP: the engine emits exactly one event per
// raft log entry, so after the first event every subsequent index must be
// exactly last+1. A gap means events were lost (e.g. an engine restarted
// with an in-memory bus, or a bug) — applying later events on top of a
// missing one would silently corrupt balances, so we refuse and let the
// streamer reconnect / full-resync instead.
//
// Duplicate or stale indices (<= last processed) are ignored, which makes
// reconnect-from-watermark replays safe.
func (c *BalanceCache) Apply(ev LedgerEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveProcessed {
		if ev.RaftIndex <= c.lastProcessedIndex {
			return nil // stale/duplicate replay
		}
		if ev.RaftIndex != c.lastProcessedIndex+1 {
			return fmt.Errorf("event stream gap: last processed index %d, got %d", c.lastProcessedIndex, ev.RaftIndex)
		}
	}

	// Expiries ride on the triggering entry; release them before the body.
	for _, xp := range ev.ExpiredPendings {
		c.getOrCreateLocked(xp.FromAccount, xp.Currency).PendingMinor -= xp.AmountMinor
	}
	// Settlement expiries carry no balance effect: a withdrawal is optimistically
	// posted (not reserved) and a deposit was never credited, so nothing to release.

	switch ev.Kind {
	case eventKindAccountCreated:
		if _, exists := c.balances[ev.AccountID]; !exists {
			c.balances[ev.AccountID] = &Balance{AccountID: ev.AccountID, Currency: ev.Currency}
		}
	case eventKindTransferApplied:
		from := c.getOrCreateLocked(ev.FromAccount, ev.Currency)
		to := c.getOrCreateLocked(ev.ToAccount, ev.Currency)
		from.BalanceMinor -= ev.AmountMinor
		to.BalanceMinor += ev.AmountMinor
	case eventKindJournalApplied:
		for _, ln := range ev.Lines {
			b := c.getOrCreateLocked(ln.AccountID, ln.Currency)
			if ln.Direction == "debit" {
				b.BalanceMinor -= ln.AmountMinor
			} else {
				b.BalanceMinor += ln.AmountMinor
			}
		}
	case eventKindPendingCreated:
		c.getOrCreateLocked(ev.FromAccount, ev.Currency).PendingMinor += ev.AmountMinor
	case eventKindPendingPosted:
		from := c.getOrCreateLocked(ev.FromAccount, ev.Currency)
		to := c.getOrCreateLocked(ev.ToAccount, ev.Currency)
		from.PendingMinor -= ev.AmountMinor
		from.BalanceMinor -= ev.AmountMinor
		to.BalanceMinor += ev.AmountMinor
	case eventKindPendingVoided:
		c.getOrCreateLocked(ev.FromAccount, ev.Currency).PendingMinor -= ev.AmountMinor
	case eventKindSettlementCreated:
		// Intent recorded; no balance effect (withdrawal posts at processing).
	case eventKindSettlementProcessing:
		// Withdrawal is optimistically posted at submission; deposit still nothing.
		if ev.SettlementKind == "withdrawal" {
			c.getOrCreateLocked(ev.AccountID, ev.Currency).BalanceMinor -= ev.AmountMinor
			c.getOrCreateLocked(ev.FundingAccount, ev.Currency).BalanceMinor += ev.AmountMinor
		}
	case eventKindSettlementPosted:
		// Withdrawal already posted at processing (no-op); deposit credits now.
		if ev.SettlementKind != "withdrawal" {
			c.getOrCreateLocked(ev.FundingAccount, ev.Currency).BalanceMinor -= ev.AmountMinor
			c.getOrCreateLocked(ev.AccountID, ev.Currency).BalanceMinor += ev.AmountMinor
		}
	case eventKindSettlementVoided:
		// Withdrawal failure reverses the optimistic post; deposit had no effect.
		if ev.SettlementKind == "withdrawal" {
			c.getOrCreateLocked(ev.AccountID, ev.Currency).BalanceMinor += ev.AmountMinor
			c.getOrCreateLocked(ev.FundingAccount, ev.Currency).BalanceMinor -= ev.AmountMinor
		}
	case eventKindCommandRejected, eventKindCommandDuplicate:
		// No balance effect; still advances the watermark so contiguity
		// checking stays valid.
	default:
		return fmt.Errorf("unknown event kind %q at index %d", ev.Kind, ev.RaftIndex)
	}

	c.lastProcessedIndex = ev.RaftIndex
	c.haveProcessed = true
	return nil
}

// Reset wipes the cache and watermark so the streamer can full-resync from
// index 0 after an unrecoverable gap.
func (c *BalanceCache) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.balances = make(map[string]*Balance)
	c.lastProcessedIndex = 0
	c.haveProcessed = false
}

// getOrCreateLocked returns the Balance for accountID, creating a
// zero-balance entry if it doesn't already exist. Callers must hold c.mu
// for writing.
func (c *BalanceCache) getOrCreateLocked(accountID, currency string) *Balance {
	b, exists := c.balances[accountID]
	if !exists {
		b = &Balance{AccountID: accountID, Currency: currency}
		c.balances[accountID] = b
	}
	return b
}

// Get returns a copy of the cached balance for accountID, and whether it
// was found.
func (c *BalanceCache) Get(accountID string) (Balance, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	b, ok := c.balances[accountID]
	if !ok {
		return Balance{}, false
	}
	out := *b
	out.AvailableMinor = out.BalanceMinor - out.PendingMinor
	return out, true
}

// All returns a snapshot copy of every cached balance.
func (c *BalanceCache) All() []Balance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Balance, 0, len(c.balances))
	for _, b := range c.balances {
		cp := *b
		cp.AvailableMinor = cp.BalanceMinor - cp.PendingMinor
		out = append(out, cp)
	}
	return out
}

// LastProcessedIndex returns the raft_index of the last event applied, and
// whether any event has been applied yet.
func (c *BalanceCache) LastProcessedIndex() (uint64, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastProcessedIndex, c.haveProcessed
}

// Count returns the number of cached accounts.
func (c *BalanceCache) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.balances)
}
