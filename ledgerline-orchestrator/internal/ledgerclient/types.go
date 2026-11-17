// Package ledgerclient is a minimal, dependency-free client for the
// ledgerline-raft HTTP contract. It intentionally does NOT import
// ledgerline-raft as a Go module; it defines its own small structs that
// mirror the wire JSON so this repo can be built and tested independently
// of the ledgerline-raft checkout.
package ledgerclient

// Account mirrors the JSON body of GET/POST /v1/accounts{,/{id}}.
type Account struct {
	AccountID          string `json:"account_id"`
	Currency           string `json:"currency"`
	BalanceMinor       int64  `json:"balance_minor"`
	PendingDebitsMinor int64  `json:"pending_debits_minor"`
	AvailableMinor     int64  `json:"available_minor"`
	Unlimited          bool   `json:"unlimited"`
	Type               string `json:"type,omitempty"`
	TenantID           string `json:"tenant_id,omitempty"`
}

// CreateAccountRequest mirrors the request body of POST /v1/accounts.
// Tenant ownership is assigned server-side from the authenticated caller;
// admin callers select it via the X-Tenant-ID header (see WithTenant).
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited"`
	Type      string `json:"type,omitempty"`
}

// TransferRequest mirrors the request body of POST /v1/transfers.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account"`
	CostCenter  string `json:"cost_center"`
}

// JournalLine is one leg of a multi-leg journal request. Each leg carries
// its own GL dimensions.
type JournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"` // "debit" or "credit"
	AmountMinor int64  `json:"amount_minor"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// JournalRequest mirrors the request body of POST /v1/journals: an atomic,
// idempotent multi-leg journal that must balance per currency.
type JournalRequest struct {
	DedupID string        `json:"dedup_id"`
	Lines   []JournalLine `json:"lines"`
}

// TransferResponse mirrors the response body of POST /v1/transfers and
// POST /v1/journals.
type TransferResponse struct {
	Status    string `json:"status"` // "applied" or "duplicate"
	JournalID string `json:"journal_id"`
	RaftIndex int64  `json:"raft_index"`
}

// LeaderInfo mirrors the response body of GET /v1/leader.
type LeaderInfo struct {
	LeaderID       string `json:"leader_id"`
	LeaderHTTPAddr string `json:"leader_http_addr"`
}

// HealthResponse mirrors the response body of GET /healthz.
type HealthResponse struct {
	State string `json:"state"` // "Leader", "Follower", or "Candidate"
}

// APIError mirrors the error envelope returned for any non-2xx response.
type APIError struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		LeaderHint string `json:"leader_hint"`
	} `json:"error"`
}

// EventJournalLine is one leg of an applied journal as emitted on the
// event stream. Unlike the request-side JournalLine it carries the
// account's currency.
type EventJournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// ExpiredPending describes a hold released by deterministic timeout expiry,
// riding along on the event whose application triggered the sweep.
type ExpiredPending struct {
	PendingID   string `json:"pending_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// Event mirrors a single line of the GET /v1/events/stream NDJSON feed.
// Exactly one event is emitted per committed raft log entry (including
// command_rejected / command_duplicate), so raft_index is contiguous.
// Not every field is populated for every kind.
type Event struct {
	RaftIndex   int64  `json:"raft_index"`
	Kind        string `json:"kind"`
	Timestamp   string `json:"timestamp"`
	AccountID   string `json:"account_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	AccountType string `json:"account_type,omitempty"`
	TenantID    string `json:"tenant_id,omitempty"`
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`

	Lines      []EventJournalLine `json:"lines,omitempty"`
	ReversalOf string             `json:"reversal_of,omitempty"`

	PendingID  string `json:"pending_id,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	VoidReason string `json:"void_reason,omitempty"`

	ErrCode string `json:"err_code,omitempty"`
	ErrMsg  string `json:"err_msg,omitempty"`

	ExpiredPendings []ExpiredPending `json:"expired_pendings,omitempty"`
}

const (
	EventKindAccountCreated   = "account_created"
	EventKindTransferApplied  = "transfer_applied"
	EventKindJournalApplied   = "journal_applied"
	EventKindPendingCreated   = "pending_created"
	EventKindPendingPosted    = "pending_posted"
	EventKindPendingVoided    = "pending_voided"
	EventKindCommandRejected  = "command_rejected"
	EventKindCommandDuplicate = "command_duplicate"
)
