package redact

import (
	"testing"
	"time"
)

func sampleRow() JournalRow {
	return JournalRow{
		ID:              1,
		TransferDedupID: "txn-abcdef1234567890",
		AccountID:       "alice",
		Direction:       "debit",
		AmountMinor:     2500,
		Currency:        "USD",
		GLAccount:       "4000",
		CostCenter:      "CC-1",
		PostedAt:        time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		RaftIndex:       11,
	}
}

func TestRedactJournalRow_RedactsCostCenter(t *testing.T) {
	row := sampleRow()
	policy := DefaultPolicy()

	out, redacted := RedactJournalRow(row, policy)

	if out.CostCenter != nil {
		t.Errorf("expected cost_center to be omitted (nil), got %q", *out.CostCenter)
	}
	if len(redacted) != 1 || redacted[0] != "cost_center" {
		t.Errorf("expected redacted fields [cost_center], got %v", redacted)
	}
	// Everything else must pass through unchanged.
	if out.AccountID != row.AccountID || out.Direction != row.Direction ||
		out.AmountMinor != row.AmountMinor || out.Currency != row.Currency ||
		out.GLAccount != row.GLAccount || !out.PostedAt.Equal(row.PostedAt) {
		t.Errorf("non-sensitive fields must pass through unchanged, got %+v", out)
	}
}

func TestRedactJournalRow_PassThroughWhenPolicyAllows(t *testing.T) {
	row := sampleRow()
	policy := Policy{Fields: map[string]FieldPolicy{
		"cost_center": {Classification: ClassificationPublic, Action: ActionNone},
	}}

	out, redacted := RedactJournalRow(row, policy)

	if out.CostCenter == nil || *out.CostCenter != "CC-1" {
		t.Errorf("expected cost_center to pass through as CC-1, got %v", out.CostCenter)
	}
	if len(redacted) != 0 {
		t.Errorf("expected no redacted fields, got %v", redacted)
	}
}

func TestRedactJournalRow_UnknownFieldDefaultsToNone(t *testing.T) {
	row := sampleRow()
	policy := Policy{Fields: map[string]FieldPolicy{}} // cost_center not mentioned at all

	out, redacted := RedactJournalRow(row, policy)

	if out.CostCenter == nil || *out.CostCenter != "CC-1" {
		t.Errorf("expected cost_center to default to pass-through, got %v", out.CostCenter)
	}
	if len(redacted) != 0 {
		t.Errorf("expected no redacted fields, got %v", redacted)
	}
}

func TestRedactSearchRow_RedactsCostCenterAndMasksDedupID(t *testing.T) {
	row := sampleRow()
	policy := DefaultPolicy()

	out, redacted := RedactSearchRow(row, policy)

	if out.CostCenter != nil {
		t.Errorf("expected cost_center to be omitted, got %q", *out.CostCenter)
	}
	wantMasked := "txn-...7890"
	if out.TransferDedupID != wantMasked {
		t.Errorf("expected masked transfer_dedup_id %q, got %q", wantMasked, out.TransferDedupID)
	}
	if len(redacted) != 2 || redacted[0] != "cost_center" || redacted[1] != "transfer_dedup_id" {
		t.Errorf("expected redacted fields [cost_center transfer_dedup_id], got %v", redacted)
	}
}

func TestRedactSearchRow_StricterThanJournalLookup(t *testing.T) {
	// search_transactions is a broader query than get_journal_entry, so it
	// must redact/mask strictly more (or equally many) fields.
	row := sampleRow()
	policy := DefaultPolicy()

	_, journalRedacted := RedactJournalRow(row, policy)
	_, searchRedacted := RedactSearchRow(row, policy)

	if len(searchRedacted) < len(journalRedacted) {
		t.Errorf("search_transactions redacted fewer fields (%v) than get_journal_entry (%v)", searchRedacted, journalRedacted)
	}
	found := false
	for _, f := range searchRedacted {
		if f == "transfer_dedup_id" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected transfer_dedup_id to be masked in search_transactions output, redacted=%v", searchRedacted)
	}
}

func TestMaskValue(t *testing.T) {
	cases := []struct {
		name               string
		in                 string
		prefixLen, suffLen int
		want               string
	}{
		{"typical dedup id", "txn-abcdef1234567890", 4, 4, "txn-...7890"},
		{"exactly at boundary is fully masked", "abcd1234", 4, 4, "********"},
		{"shorter than boundary is fully masked", "ab", 4, 4, "**"},
		{"empty string", "", 4, 4, ""},
		{"longer string by one char", "abcdefghi", 4, 4, "abcd...fghi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MaskValue(c.in, c.prefixLen, c.suffLen)
			if got != c.want {
				t.Errorf("MaskValue(%q, %d, %d) = %q, want %q", c.in, c.prefixLen, c.suffLen, got, c.want)
			}
			if len(c.in) > 0 && got == c.in {
				t.Errorf("MaskValue(%q, ...) returned the input unmasked", c.in)
			}
		})
	}
}

func TestCapLimit(t *testing.T) {
	cases := []struct {
		name      string
		requested int
		max       int
		want      int
	}{
		{"within cap", 5, 20, 5},
		{"exactly at cap", 20, 20, 20},
		{"exceeds cap, must clamp", 1000, 20, 20},
		{"zero falls back to max", 0, 20, 20},
		{"negative falls back to max", -1, 20, 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CapLimit(c.requested, c.max)
			if got != c.want {
				t.Errorf("CapLimit(%d, %d) = %d, want %d", c.requested, c.max, got, c.want)
			}
			if got > c.max {
				t.Errorf("CapLimit(%d, %d) = %d exceeds hard cap %d", c.requested, c.max, got, c.max)
			}
		})
	}
}
