// Package scheme is a simulated card network (acquiring rail). Real schemes
// (Visa/Mastercard) authorize and capture asynchronously and can decline;
// this mock reproduces that with deterministic failure injection.
package scheme

import "fmt"

// Result is a scheme operation outcome.
type Result struct {
	Ref    string
	OK     bool
	Reason string
}

// Scheme is the card-network interface the acquiring service drives.
type Scheme interface {
	Auth(amount int64, currency string) Result
	Capture(amount int64, currency string) Result
}

// Simulated is a mock scheme. Amounts in FailAmounts are declined.
type Simulated struct {
	FailAmounts map[int64]bool
	seq         int64
}

// NewSimulated builds a mock scheme with the given decline set.
func NewSimulated(failAmounts map[int64]bool) *Simulated {
	if failAmounts == nil {
		failAmounts = make(map[int64]bool)
	}
	return &Simulated{FailAmounts: failAmounts}
}

func (s *Simulated) op(kind string, amount int64, currency string) Result {
	s.seq++
	ref := fmt.Sprintf("scheme-%s-%d", kind, s.seq)
	if s.FailAmounts[amount] {
		return Result{Ref: ref, OK: false, Reason: "simulated scheme decline"}
	}
	return Result{Ref: ref, OK: true}
}

// Auth authorizes (freezes) funds on the customer's card.
func (s *Simulated) Auth(amount int64, currency string) Result {
	return s.op("auth", amount, currency)
}

// Capture initiates the actual debit.
func (s *Simulated) Capture(amount int64, currency string) Result {
	return s.op("capture", amount, currency)
}
