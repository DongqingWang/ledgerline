package recon

import "github.com/ledgerline/ledgerline-recon/internal/stream"

// LedgerRecordsFromEvent extracts the ledger-side reconciliation record(s)
// from an event. For a journal it sums the debit legs (the gross movement) and
// keys on the journal's dedup id; for a transfer it uses the amount directly.
// Pure and deterministic so it can be unit tested without a live stream.
func LedgerRecordsFromEvent(ev stream.Event) []Record {
	switch ev.Kind {
	case "journal_applied":
		var amount int64
		currency := ""
		for _, ln := range ev.Lines {
			if ln.Direction == "debit" {
				amount += ln.AmountMinor
			}
			if currency == "" {
				currency = ln.Currency
			}
		}
		if ev.DedupID == "" {
			return nil
		}
		return []Record{{Ref: ev.DedupID, AmountMinor: amount, Currency: currency}}
	case "transfer_applied":
		if ev.DedupID == "" {
			return nil
		}
		return []Record{{Ref: ev.DedupID, AmountMinor: ev.AmountMinor, Currency: ev.Currency}}
	}
	return nil
}

// HandleEvent feeds any ledger records in the event into the service
// (idempotent by ref, so stream replay is safe).
func (s *Service) HandleEvent(ev stream.Event) {
	for _, r := range LedgerRecordsFromEvent(ev) {
		s.AddLedgerRecord(r)
	}
}
