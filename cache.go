package main

import (
	"sync"
)

// Balance is the cached, read-optimized view of a single account's balance.
type Balance struct {
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
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

// Apply applies a single LedgerEvent to the cache, mutating balances as
// appropriate. It is idempotent/ordering-safe with respect to raft_index:
//
//   - Events are expected to arrive in strictly increasing raft_index order
//     (that's what the stream guarantees). As a defensive measure against
//     duplicate delivery or out-of-order replay (e.g. a reconnect that
//     accidentally replays an already-seen index), any event whose
//     RaftIndex is less than or equal to the last-processed index is
//     ignored rather than re-applied. This prevents double-counting a
//     transfer if the same index is ever seen twice.
//   - account_created events insert a zero-balance entry for the account if
//     one doesn't already exist. If the account somehow already exists
//     (e.g. a duplicate account_created, which shouldn't happen in
//     practice), the existing balance is left untouched -- we never reset
//     a non-zero balance back to zero.
//   - transfer_applied events debit from_account and credit to_account by
//     amount_minor. Accounts are created on the fly with an empty currency
//     if they are not already present in the cache, since this service is
//     defensive about starting mid-stream or missing an account_created
//     event; in normal operation every account is seen via
//     account_created first because streaming starts at from_index=0.
func (c *BalanceCache) Apply(ev LedgerEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.haveProcessed && ev.RaftIndex <= c.lastProcessedIndex {
		// Stale, duplicate, or out-of-order event relative to what we've
		// already applied. Ignore it to avoid double-applying a transfer.
		return
	}

	switch ev.Kind {
	case eventKindAccountCreated:
		if _, exists := c.balances[ev.AccountID]; !exists {
			c.balances[ev.AccountID] = &Balance{
				AccountID:    ev.AccountID,
				Currency:     ev.Currency,
				BalanceMinor: 0,
			}
		}
	case eventKindTransferApplied:
		from := c.getOrCreateLocked(ev.FromAccount)
		to := c.getOrCreateLocked(ev.ToAccount)
		from.BalanceMinor -= ev.AmountMinor
		to.BalanceMinor += ev.AmountMinor
	default:
		// Unknown event kind: ignore. Forward-compatible with future event
		// kinds this service doesn't yet understand.
	}

	c.lastProcessedIndex = ev.RaftIndex
	c.haveProcessed = true
}

// getOrCreateLocked returns the Balance for accountID, creating a
// zero-balance entry with an empty currency if it doesn't already exist.
// Callers must hold c.mu for writing.
func (c *BalanceCache) getOrCreateLocked(accountID string) *Balance {
	b, exists := c.balances[accountID]
	if !exists {
		b = &Balance{AccountID: accountID}
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
	return *b, true
}

// All returns a snapshot copy of every cached balance.
func (c *BalanceCache) All() []Balance {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Balance, 0, len(c.balances))
	for _, b := range c.balances {
		out = append(out, *b)
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
