package redact

import (
	"sort"
	"strings"
	"time"
)

// RedactedPlaceholder is substituted for a field's value when the policy
// action is ActionRedact but the field must still be structurally present
// (not used by the two tools in this gateway today, which omit redacted
// fields instead, but kept available for other policies/tools).
const RedactedPlaceholder = "[REDACTED]"

// JournalRow is the in-memory representation of one row of journal_entries,
// exactly as fetched from the database, before any redaction is applied.
type JournalRow struct {
	ID              int64
	TransferDedupID string
	AccountID       string
	Direction       string
	AmountMinor     int64
	Currency        string
	GLAccount       string
	CostCenter      string
	PostedAt        time.Time
	RaftIndex       int64
}

// RedactedJournalRow is the shape returned by get_journal_entry: both legs
// of a transfer, with internal_sensitive fields (cost_center) redacted.
type RedactedJournalRow struct {
	AccountID   string    `json:"account_id"`
	Direction   string    `json:"direction"`
	AmountMinor int64     `json:"amount_minor"`
	Currency    string    `json:"currency"`
	GLAccount   string    `json:"gl_account,omitempty"`
	CostCenter  *string   `json:"cost_center,omitempty"`
	PostedAt    time.Time `json:"posted_at"`
}

// SearchRow is the shape returned by search_transactions: like
// RedactedJournalRow, but also carries a masked transfer_dedup_id, since a
// broad search gets stricter redaction than a narrow, single-record lookup.
type SearchRow struct {
	TransferDedupID string    `json:"transfer_dedup_id"`
	AccountID       string    `json:"account_id"`
	Direction       string    `json:"direction"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	GLAccount       string    `json:"gl_account,omitempty"`
	CostCenter      *string   `json:"cost_center,omitempty"`
	PostedAt        time.Time `json:"posted_at"`
}

// RedactJournalRow converts a raw JournalRow into the shape returned by
// get_journal_entry, applying the policy's action for cost_center. It
// returns the redacted row plus the sorted list of field names that were
// actually redacted/masked in this row (for audit logging).
func RedactJournalRow(row JournalRow, policy Policy) (RedactedJournalRow, []string) {
	out := RedactedJournalRow{
		AccountID:   row.AccountID,
		Direction:   row.Direction,
		AmountMinor: row.AmountMinor,
		Currency:    row.Currency,
		GLAccount:   row.GLAccount,
		PostedAt:    row.PostedAt,
	}

	var redacted []string
	switch policy.ActionFor("cost_center") {
	case ActionRedact:
		// Omit entirely: CostCenter stays nil so `omitempty` drops it.
		redacted = append(redacted, "cost_center")
	case ActionMask:
		masked := MaskValue(row.CostCenter, 2, 2)
		out.CostCenter = &masked
		redacted = append(redacted, "cost_center")
	default:
		if row.CostCenter != "" {
			out.CostCenter = &row.CostCenter
		}
	}

	return out, redacted
}

// RedactSearchRow converts a raw JournalRow into the shape returned by
// search_transactions: cost_center redacted the same way as
// RedactJournalRow, plus transfer_dedup_id masked (or redacted) per policy.
// It returns the redacted row plus the sorted list of field names that were
// actually redacted/masked in this row (for audit logging).
func RedactSearchRow(row JournalRow, policy Policy) (SearchRow, []string) {
	out := SearchRow{
		AccountID:   row.AccountID,
		Direction:   row.Direction,
		AmountMinor: row.AmountMinor,
		Currency:    row.Currency,
		GLAccount:   row.GLAccount,
		PostedAt:    row.PostedAt,
	}

	var redacted []string

	switch policy.ActionFor("cost_center") {
	case ActionRedact:
		redacted = append(redacted, "cost_center")
	case ActionMask:
		masked := MaskValue(row.CostCenter, 2, 2)
		out.CostCenter = &masked
		redacted = append(redacted, "cost_center")
	default:
		if row.CostCenter != "" {
			out.CostCenter = &row.CostCenter
		}
	}

	switch policy.ActionFor("transfer_dedup_id") {
	case ActionMask:
		out.TransferDedupID = MaskValue(row.TransferDedupID, 4, 4)
		redacted = append(redacted, "transfer_dedup_id")
	case ActionRedact:
		out.TransferDedupID = RedactedPlaceholder
		redacted = append(redacted, "transfer_dedup_id")
	default:
		out.TransferDedupID = row.TransferDedupID
	}

	sort.Strings(redacted)
	return out, redacted
}

// MaskValue partially obscures a string, keeping up to prefixLen leading
// characters and suffixLen trailing characters visible, joined by "...".
// If the string is too short for the mask to meaningfully hide anything
// (len(s) <= prefixLen+suffixLen), the whole value is replaced with
// asterisks of the same length instead of being returned unmasked.
func MaskValue(s string, prefixLen, suffixLen int) string {
	if len(s) <= prefixLen+suffixLen {
		return strings.Repeat("*", len(s))
	}
	return s[:prefixLen] + "..." + s[len(s)-suffixLen:]
}

// CapLimit enforces a hard server-side ceiling on a caller-supplied result
// limit, independent of per-field redaction. It returns the smaller of the
// requested limit and max. Non-positive requested values fall back to max,
// so a missing/zero/negative "limit" argument doesn't silently return zero
// rows.
func CapLimit(requested, max int) int {
	if requested <= 0 {
		return max
	}
	if requested > max {
		return max
	}
	return requested
}
