package acquiring

import (
	"fmt"
	"sync"
	"time"
)

// Store is an in-memory PaymentIntent store (demo-grade; production persists
// it so a crash never loses the durable intent). Idempotent create by DedupID
// and legal status transitions are enforced here.
type Store struct {
	mu      sync.Mutex
	byID    map[string]*PaymentIntent
	byDedup map[string]*PaymentIntent
	seq     int64
	now     func() time.Time
}

// NewStore creates an empty store.
func NewStore(now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{byID: make(map[string]*PaymentIntent), byDedup: make(map[string]*PaymentIntent), now: now}
}

// Create inserts a new intent, idempotent on DedupID.
func (s *Store) Create(dedupID, merchantID string, amount, fee int64, currency string) (*PaymentIntent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.byDedup[dedupID]; ok {
		return e, false, nil
	}
	if amount <= 0 || fee < 0 || fee >= amount {
		return nil, false, fmt.Errorf("amount must be positive and fee in [0, amount)")
	}
	s.seq++
	now := s.now()
	pi := &PaymentIntent{
		ID: fmt.Sprintf("pi-%d", s.seq), DedupID: dedupID, MerchantID: merchantID,
		AmountMinor: amount, FeeMinor: fee, Currency: currency,
		Status: StatusPending, CreatedAt: now, UpdatedAt: now,
	}
	s.byID[pi.ID] = pi
	s.byDedup[dedupID] = pi
	return pi, true, nil
}

// Get returns a copy by ID.
func (s *Store) Get(id string) (*PaymentIntent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	cp := *pi
	return &cp, true
}

// Transition moves status with lifecycle validation; mutate adjusts fields.
func (s *Store) Transition(id string, to Status, mutate func(*PaymentIntent)) (*PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("intent %q not found", id)
	}
	if !CanTransition(pi.Status, to) {
		return nil, fmt.Errorf("illegal transition %s -> %s for %s", pi.Status, to, id)
	}
	if mutate != nil {
		mutate(pi)
	}
	pi.Status = to
	pi.UpdatedAt = s.now()
	cp := *pi
	return &cp, nil
}

// Update mutates fields without changing status.
func (s *Store) Update(id string, mutate func(*PaymentIntent)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pi, ok := s.byID[id]
	if !ok {
		return fmt.Errorf("intent %q not found", id)
	}
	mutate(pi)
	pi.UpdatedAt = s.now()
	return nil
}

// List returns copies of all intents.
func (s *Store) List() []PaymentIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PaymentIntent, 0, len(s.byID))
	for _, pi := range s.byID {
		out = append(out, *pi)
	}
	return out
}
