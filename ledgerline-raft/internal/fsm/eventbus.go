package fsm

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"sync"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// EventBus is the CQRS fan-out point: every applied command publishes one
// LedgerEvent here, and any number of subscribers (the HTTP streaming
// endpoint, which balance-reader and persistence-sync tail) can replay from
// a given raft index and then keep receiving new events live.
//
// When constructed with a file path, history is durable: every event is
// appended to an ndjson file and replayed into memory on startup, so a
// restarting node can still serve backlog to consumers even after raft log
// compaction has discarded the original entries. The file needs no fsync —
// the raft log is the source of truth and rebuilds any lost tail via Apply
// (Publish skips events already present, keyed by raft index).
//
// Keeping the full history in memory is a deliberate demo-scale
// simplification — a production system would window/compact this (or read
// replay history from the durable store instead of RAM).
type EventBus struct {
	mu          sync.Mutex
	history     []ledgertypes.LedgerEvent
	lastIndex   uint64
	subscribers map[int]chan ledgertypes.LedgerEvent
	nextSubID   int

	path string
	file *os.File
}

// NewEventBus builds a bus. path == "" keeps history in memory only (unit
// tests); otherwise history is loaded from / appended to that ndjson file.
func NewEventBus(path string) *EventBus {
	b := &EventBus{
		subscribers: make(map[int]chan ledgertypes.LedgerEvent),
		path:        path,
	}
	if path == "" {
		return b
	}
	b.loadHistory()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("eventbus: open event log %s: %v (continuing in-memory)", path, err)
		return b
	}
	b.file = f
	return b
}

// loadHistory reads the ndjson file into memory. A partially-written
// trailing line (crash mid-append) is truncated away; raft replay will
// re-Apply that entry and re-publish the event.
func (b *EventBus) loadHistory() {
	data, err := os.ReadFile(b.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("eventbus: read event log %s: %v", b.path, err)
		}
		return
	}
	var goodBytes int64
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			goodBytes += int64(len(line)) + 1
			continue
		}
		var ev ledgertypes.LedgerEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			// Partial/corrupt tail: stop here and truncate the rest.
			break
		}
		b.history = append(b.history, ev)
		if ev.RaftIndex > b.lastIndex {
			b.lastIndex = ev.RaftIndex
		}
		goodBytes += int64(len(line)) + 1
	}
	if goodBytes < int64(len(data)) {
		if err := os.Truncate(b.path, goodBytes); err != nil {
			log.Printf("eventbus: truncate partial tail of %s: %v", b.path, err)
		}
	}
}

// Publish appends an event to history and fans it out to live subscribers.
// Called only from inside FSM.Apply, so it is already serialized with
// respect to other state mutations.
func (b *EventBus) Publish(ev ledgertypes.LedgerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Raft replays log entries after a restart/snapshot restore; events we
	// already durably have must not be duplicated in history or the file.
	if len(b.history) > 0 && ev.RaftIndex <= b.lastIndex {
		return
	}
	b.history = append(b.history, ev)
	if ev.RaftIndex > b.lastIndex {
		b.lastIndex = ev.RaftIndex
	}
	if b.file != nil {
		line, err := json.Marshal(ev)
		if err == nil {
			if _, err := b.file.Write(append(line, '\n')); err != nil {
				log.Printf("eventbus: append event log: %v", err)
			}
		}
	}

	for id, ch := range b.subscribers {
		// Non-blocking: a slow subscriber must never stall the FSM's apply
		// path. Instead of silently dropping events (which would corrupt
		// projections), kick the subscriber: closing its channel forces the
		// HTTP stream to end, and the consumer reconnects from its last
		// processed index, replaying the backlog it missed. Kafka-style
		// "you fell behind, re-fetch from your offset".
		select {
		case ch <- ev:
		default:
			delete(b.subscribers, id)
			close(ch)
		}
	}
}

// Subscribe replays every event with RaftIndex >= fromIndex, then streams
// new events as they're published, until cancel() is called or the bus
// kicks the subscriber for falling behind (channel closed).
func (b *EventBus) Subscribe(fromIndex uint64) (events <-chan ledgertypes.LedgerEvent, backlog []ledgertypes.LedgerEvent, cancel func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for _, ev := range b.history {
		if ev.RaftIndex >= fromIndex {
			backlog = append(backlog, ev)
		}
	}

	id := b.nextSubID
	b.nextSubID++
	ch := make(chan ledgertypes.LedgerEvent, 256)
	b.subscribers[id] = ch

	cancel = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		// The bus may already have kicked (and closed) this subscriber.
		if _, ok := b.subscribers[id]; ok {
			delete(b.subscribers, id)
			close(ch)
		}
	}
	return ch, backlog, cancel
}

// Close closes the durable event log file, if any.
func (b *EventBus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		err := b.file.Close()
		b.file = nil
		return err
	}
	return nil
}
