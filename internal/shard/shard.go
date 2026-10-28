// Package shard implements the key-range hashing and routing-table logic
// used by the orchestrator to decide which ledgerline-raft cluster owns a
// given account.
package shard

import (
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
)

// MaxRange is the exclusive upper bound of the uint32 hash space. Shard
// ranges are half-open [start, end) subsets of [0, MaxRange).
const MaxRange uint64 = 1 << 32

// ShardFor hashes an account ID into the uint32 keyspace using FNV-1a.
// The result is deterministic for a given accountID and is used to decide
// which shard range (and therefore which cluster) owns the account.
func ShardFor(accountID string) uint32 {
	h := fnv.New32a()
	// Hash.Write on a Fowler-Noll-Vo hash never returns an error.
	_, _ = h.Write([]byte(accountID))
	return h.Sum32()
}

// Shard is a single entry in the routing table: a half-open key range and
// the HTTP addresses of the ledgerline-raft cluster nodes that currently
// own it.
type Shard struct {
	RangeStart  uint32   `json:"range_start"`
	RangeEnd    uint64   `json:"range_end"` // uint64 so MaxRange (2^32) is representable
	LedgerNodes []string `json:"ledger_nodes"`
}

// Contains reports whether key falls within [RangeStart, RangeEnd).
func (s Shard) Contains(key uint32) bool {
	return uint64(key) >= uint64(s.RangeStart) && uint64(key) < s.RangeEnd
}

// Table is an in-memory routing table: an ordered, non-overlapping set of
// shards covering the full key space. It is safe for concurrent use; all
// methods take an internal lock.
type Table struct {
	mu     sync.RWMutex
	shards []Shard
}

// NewTable creates a routing table with a single shard covering the full
// range [0, MaxRange) pointed at the given initial cluster nodes.
func NewTable(initialNodes []string) *Table {
	return &Table{
		shards: []Shard{
			{RangeStart: 0, RangeEnd: MaxRange, LedgerNodes: append([]string(nil), initialNodes...)},
		},
	}
}

// Snapshot returns a copy of the current shards, ordered by RangeStart.
func (t *Table) Snapshot() []Shard {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Shard, len(t.shards))
	copy(out, t.shards)
	return out
}

// ShardForKey returns the shard owning the given key, if any.
func (t *Table) ShardForKey(key uint32) (Shard, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, s := range t.shards {
		if s.Contains(key) {
			return s, true
		}
	}
	return Shard{}, false
}

// FindShardForSplitPoint returns the index into the current shard slice of
// the shard that contains splitPoint. This is the shard that a split
// request would divide into two.
func FindShardForSplitPoint(shards []Shard, splitPoint uint32) (int, error) {
	for i, s := range shards {
		if s.Contains(splitPoint) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("no shard contains split point %d", splitPoint)
}

// ApplySplit computes the resulting shard slice after splitting the shard
// that contains splitPoint into [old_start, splitPoint) -> unchanged nodes
// and [splitPoint, old_end) -> newNodes. It does not mutate the input
// slice. It returns an error if splitPoint does not fall strictly inside
// an existing shard's range (i.e. splitting at a shard boundary, or a
// point outside any shard, is rejected).
func ApplySplit(shards []Shard, splitPoint uint32, newNodes []string) ([]Shard, error) {
	idx, err := FindShardForSplitPoint(shards, splitPoint)
	if err != nil {
		return nil, err
	}
	old := shards[idx]
	if uint64(splitPoint) == uint64(old.RangeStart) {
		return nil, fmt.Errorf("split point %d equals the shard's start; nothing to split", splitPoint)
	}

	lower := Shard{RangeStart: old.RangeStart, RangeEnd: uint64(splitPoint), LedgerNodes: old.LedgerNodes}
	upper := Shard{RangeStart: splitPoint, RangeEnd: old.RangeEnd, LedgerNodes: append([]string(nil), newNodes...)}

	out := make([]Shard, 0, len(shards)+1)
	out = append(out, shards[:idx]...)
	out = append(out, lower, upper)
	out = append(out, shards[idx+1:]...)

	sort.Slice(out, func(i, j int) bool { return out[i].RangeStart < out[j].RangeStart })
	return out, nil
}

// Split atomically replaces the shard containing splitPoint with two
// shards as described in ApplySplit. It is the caller's responsibility to
// have already validated/migrated data before calling this - Split itself
// performs no I/O, only the in-memory routing update.
func (t *Table) Split(splitPoint uint32, newNodes []string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	next, err := ApplySplit(t.shards, splitPoint, newNodes)
	if err != nil {
		return err
	}
	t.shards = next
	return nil
}
