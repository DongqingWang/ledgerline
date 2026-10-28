package shard

import (
	"reflect"
	"testing"
)

func TestShardForDeterministic(t *testing.T) {
	ids := []string{"alice", "bob", "external-deposit", "", "a-very-long-account-id-string-1234567890"}
	for _, id := range ids {
		a := ShardFor(id)
		b := ShardFor(id)
		if a != b {
			t.Errorf("ShardFor(%q) not deterministic: %d != %d", id, a, b)
		}
	}
}

func TestShardForDistribution(t *testing.T) {
	// Sanity check: hashing 1000 distinct IDs should not collapse them all
	// into one bucket, and should spread reasonably across the lower and
	// upper halves of the keyspace.
	const n = 1000
	below, above := 0, 0
	seen := make(map[uint32]bool, n)
	for i := 0; i < n; i++ {
		id := randomishID(i)
		h := ShardFor(id)
		seen[h] = true
		if uint64(h) < MaxRange/2 {
			below++
		} else {
			above++
		}
	}
	if len(seen) < n*9/10 {
		t.Errorf("expected near-unique hashes, got %d unique out of %d", len(seen), n)
	}
	if below == 0 || above == 0 {
		t.Errorf("expected hashes on both sides of the midpoint, got below=%d above=%d", below, above)
	}
}

func randomishID(i int) string {
	// Deterministic pseudo-random-looking IDs without needing math/rand,
	// so the test itself stays fully deterministic.
	return "account-" + string(rune('a'+(i%26))) + string(rune('A'+((i*7)%26))) + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

func TestNewTableSingleShard(t *testing.T) {
	tbl := NewTable([]string{"127.0.0.1:8401", "127.0.0.1:8402"})
	snap := tbl.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 shard, got %d", len(snap))
	}
	if snap[0].RangeStart != 0 || snap[0].RangeEnd != MaxRange {
		t.Errorf("expected full range [0, %d), got [%d, %d)", MaxRange, snap[0].RangeStart, snap[0].RangeEnd)
	}
	if !reflect.DeepEqual(snap[0].LedgerNodes, []string{"127.0.0.1:8401", "127.0.0.1:8402"}) {
		t.Errorf("unexpected nodes: %v", snap[0].LedgerNodes)
	}
}

func TestShardForKey(t *testing.T) {
	tbl := NewTable([]string{"n1"})
	if err := tbl.Split(100, []string{"n2"}); err != nil {
		t.Fatalf("split failed: %v", err)
	}
	lower, ok := tbl.ShardForKey(50)
	if !ok || !reflect.DeepEqual(lower.LedgerNodes, []string{"n1"}) {
		t.Errorf("key 50 should route to n1, got %+v ok=%v", lower, ok)
	}
	upper, ok := tbl.ShardForKey(100)
	if !ok || !reflect.DeepEqual(upper.LedgerNodes, []string{"n2"}) {
		t.Errorf("key 100 (boundary, inclusive on upper side) should route to n2, got %+v ok=%v", upper, ok)
	}
	below, ok := tbl.ShardForKey(99)
	if !ok || !reflect.DeepEqual(below.LedgerNodes, []string{"n1"}) {
		t.Errorf("key 99 should still route to n1, got %+v ok=%v", below, ok)
	}
}

func TestApplySplitBasic(t *testing.T) {
	shards := []Shard{{RangeStart: 0, RangeEnd: MaxRange, LedgerNodes: []string{"old1", "old2"}}}
	out, err := ApplySplit(shards, 2147483648, []string{"new1", "new2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Shard{
		{RangeStart: 0, RangeEnd: 2147483648, LedgerNodes: []string{"old1", "old2"}},
		{RangeStart: 2147483648, RangeEnd: MaxRange, LedgerNodes: []string{"new1", "new2"}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %+v, want %+v", out, want)
	}
}

func TestApplySplitOnAlreadySplitTable(t *testing.T) {
	shards := []Shard{
		{RangeStart: 0, RangeEnd: 1000, LedgerNodes: []string{"a"}},
		{RangeStart: 1000, RangeEnd: 2000, LedgerNodes: []string{"b"}},
	}
	out, err := ApplySplit(shards, 1500, []string{"c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Shard{
		{RangeStart: 0, RangeEnd: 1000, LedgerNodes: []string{"a"}},
		{RangeStart: 1000, RangeEnd: 1500, LedgerNodes: []string{"b"}},
		{RangeStart: 1500, RangeEnd: 2000, LedgerNodes: []string{"c"}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("got %+v, want %+v", out, want)
	}
}

func TestApplySplitRejectsPointOutsideAnyShard(t *testing.T) {
	shards := []Shard{{RangeStart: 0, RangeEnd: 1000, LedgerNodes: []string{"a"}}}
	if _, err := ApplySplit(shards, 5000, []string{"c"}); err == nil {
		t.Errorf("expected error for split point outside any shard")
	}
}

func TestApplySplitRejectsBoundaryPoint(t *testing.T) {
	shards := []Shard{{RangeStart: 1000, RangeEnd: 2000, LedgerNodes: []string{"a"}}}
	if _, err := ApplySplit(shards, 1000, []string{"c"}); err == nil {
		t.Errorf("expected error when split point equals shard start")
	}
}

func TestApplySplitDoesNotMutateInput(t *testing.T) {
	shards := []Shard{{RangeStart: 0, RangeEnd: MaxRange, LedgerNodes: []string{"old1"}}}
	orig := make([]Shard, len(shards))
	copy(orig, shards)

	if _, err := ApplySplit(shards, 100, []string{"new1"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(shards, orig) {
		t.Errorf("ApplySplit mutated its input slice: got %+v, want %+v", shards, orig)
	}
}
