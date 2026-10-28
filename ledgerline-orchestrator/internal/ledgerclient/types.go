// Package ledgerclient is a minimal, dependency-free client for the
// ledgerline-raft HTTP contract. It intentionally does NOT import
// ledgerline-raft as a Go module; it defines its own small structs that
// mirror the wire JSON so this repo can be built and tested independently
// of the ledgerline-raft checkout.
package ledgerclient

// Account mirrors the JSON body of GET/POST /v1/accounts{,/{id}}.
type Account struct {
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Unlimited    bool   `json:"unlimited"`
}

// CreateAccountRequest mirrors the request body of POST /v1/accounts.
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited"`
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

// TransferResponse mirrors the response body of POST /v1/transfers.
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

// Event mirrors a single line of the GET /v1/events/stream NDJSON feed.
// Not every field is populated for every event kind: account_created
// events carry AccountID/Currency; transfer_applied events carry
// DedupID/JournalID/FromAccount/ToAccount/AmountMinor/GLAccount/CostCenter.
type Event struct {
	RaftIndex   int64  `json:"raft_index"`
	Kind        string `json:"kind"`
	Timestamp   string `json:"timestamp"`
	AccountID   string `json:"account_id,omitempty"`
	Currency    string `json:"currency,omitempty"`
	DedupID     string `json:"dedup_id,omitempty"`
	JournalID   string `json:"journal_id,omitempty"`
	FromAccount string `json:"from_account,omitempty"`
	ToAccount   string `json:"to_account,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

const (
	EventKindAccountCreated  = "account_created"
	EventKindTransferApplied = "transfer_applied"
)
