// Package clearing computes net settlement obligations from gross captured
// transactions. Clearing is the information-flow layer between acquiring
// (gross per-transaction) and settlement (net fund movement): it nets many
// transactions per party into one obligation, reducing rail traffic and cost.
//
// The netting engine here is pure and deterministic (sorted output) so it can
// be exhaustively unit tested and produces identical batches on every replica.
package clearing

import "sort"

// Capture is one gross captured transaction (or refund) flowing into clearing.
// NetMinor is the net amount owed to the merchant for this item: positive for
// a capture (amount - fee), negative for a refund.
type Capture struct {
	MerchantID string
	Currency   string
	NetMinor   int64
	Ref        string
}

// NetPosition is the netted obligation for one (merchant, currency) over a
// clearing cycle.
type NetPosition struct {
	MerchantID string `json:"merchant_id"`
	Currency   string `json:"currency"`
	NetMinor   int64  `json:"net_minor"`
	Count      int    `json:"count"`
}

// Net groups captures by (merchant, currency), sums their net amounts, and
// returns positions sorted by (merchant, currency) for determinism. Positions
// that net to exactly zero are still returned (a fully-refunded merchant) so
// the clearing journal can zero out the gross payable.
func Net(captures []Capture) []NetPosition {
	type key struct{ merchant, currency string }
	agg := make(map[key]*NetPosition)
	for _, c := range captures {
		k := key{c.MerchantID, c.Currency}
		p, ok := agg[k]
		if !ok {
			p = &NetPosition{MerchantID: c.MerchantID, Currency: c.Currency}
			agg[k] = p
		}
		p.NetMinor += c.NetMinor
		p.Count++
	}
	out := make([]NetPosition, 0, len(agg))
	for _, p := range agg {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MerchantID != out[j].MerchantID {
			return out[i].MerchantID < out[j].MerchantID
		}
		return out[i].Currency < out[j].Currency
	})
	return out
}

// Batch is the output of a clearing run: the net positions to settle plus a
// batch identifier.
type Batch struct {
	BatchID   string        `json:"batch_id"`
	Positions []NetPosition `json:"positions"`
	Count     int           `json:"count"`
}
