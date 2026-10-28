package fsm

import (
	"sync"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

// EventBus is the CQRS fan-out point: every applied command publishes one
// LedgerEvent here, and any number of subscribers (the HTTP streaming
// endpoint, which balance-reader and persistence-sync tail) can replay from
// a given raft index and then keep receiving new events live.
//
// Keeping the full history in memory is a deliberate demo-scale
// simplification — a production system would window/compact this (or read
// replay history from the durable store instead of RAM). Documented in the
// repo README.
type EventBus struct {
	mu          sync.Mutex
	history     []ledgertypes.LedgerEvent
	subscribers map[int]chan ledgertypes.LedgerEvent
	nextSubID   int
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[int]chan ledgertypes.LedgerEvent),
	}
}

// Publish appends an event to history and fans it out to live subscribers.
// Called only from inside FSM.Apply, so it is already serialized with
// respect to other state mutations.
func (b *EventBus) Publish(ev ledgertypes.LedgerEvent) {
	b.mu.Lock()
	b.history = append(b.history, ev)
	subs := make([]chan ledgertypes.LedgerEvent, 0, len(b.subscribers))
	for _, ch := range b.subscribers {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking: a slow subscriber must never stall the FSM's apply
		// path. Subscribers that fall behind should reconnect with
		// Subscribe(fromIndex) rather than relying on an unbounded buffer.
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe replays every event with RaftIndex >= fromIndex, then streams
// new events as they're published, until cancel() is called.
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
		delete(b.subscribers, id)
		close(ch)
	}
	return ch, backlog, cancel
}
