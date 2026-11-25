package clearing

import "github.com/ledgerline/ledgerline-clearing/internal/stream"

// CapturesFromEvent extracts acquiring captures from a journal_applied event.
// The acquiring journal's credit leg to the merchant-payable account carries
// the merchant in CostCenter; a debit leg (a reversal/refund) nets negative.
// Pure and deterministic so it can be unit tested without a live stream.
func CapturesFromEvent(ev stream.Event, payableAccount string) []Capture {
	if ev.Kind != "journal_applied" {
		return nil
	}
	var out []Capture
	for _, ln := range ev.Lines {
		if ln.AccountID != payableAccount {
			continue
		}
		net := ln.AmountMinor
		if ln.Direction == "debit" {
			net = -net // reversal / refund
		}
		out = append(out, Capture{
			MerchantID: ln.CostCenter,
			Currency:   ln.Currency,
			NetMinor:   net,
			Ref:        ev.DedupID,
		})
	}
	return out
}

// HandleEvent feeds any captures in the event into the service (idempotent).
func (s *Service) HandleEvent(ev stream.Event) {
	for _, c := range CapturesFromEvent(ev, s.PayableAccount) {
		s.Ingest(c)
	}
}
