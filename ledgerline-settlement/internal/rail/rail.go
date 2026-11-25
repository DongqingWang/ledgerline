// Package rail is a simulated async bank rail. Real ACH/card rails take
// days and can fail or return; this mock reproduces that shape with a
// configurable delay and deterministic failure injection so the settlement
// loop is testable without external infrastructure.
package rail

import (
	"sync"
	"time"
)

// Outcome is the rail result delivered to the orchestrator.
type Outcome string

const (
	Settled Outcome = "settled"
	Failed  Outcome = "failed"
)

// Rule decides an instruction's outcome from its id and amount.
type Rule func(id string, amount int64) (Outcome, string)

// SettledRule always settles (default happy path).
func SettledRule(string, int64) (Outcome, string) { return Settled, "" }

// FailAmounts fails any instruction whose amount is in the set.
func FailAmounts(amounts map[int64]bool) Rule {
	return func(_ string, amount int64) (Outcome, string) {
		if amounts[amount] {
			return Failed, "simulated rail rejection"
		}
		return Settled, ""
	}
}

// Rail delivers outcomes asynchronously after a delay.
type Rail struct {
	delay   time.Duration
	rule    Rule
	deliver func(id string, o Outcome, reason string)
	wg      sync.WaitGroup
}

// New builds a rail. deliver is called (after delay) with each outcome.
func New(delay time.Duration, rule Rule, deliver func(string, Outcome, string)) *Rail {
	if rule == nil {
		rule = SettledRule
	}
	return &Rail{delay: delay, rule: rule, deliver: deliver}
}

// Submit schedules async processing of a settlement (by its ledger id).
func (r *Rail) Submit(id string, amount int64) {
	r.wg.Add(1)
	time.AfterFunc(r.delay, func() {
		defer r.wg.Done()
		o, reason := r.rule(id, amount)
		r.deliver(id, o, reason)
	})
}

// Wait blocks until all scheduled deliveries have run.
func (r *Rail) Wait() { r.wg.Wait() }
