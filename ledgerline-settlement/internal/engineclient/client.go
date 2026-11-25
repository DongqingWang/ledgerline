// Package engineclient is a minimal HTTP client for the ledgerline-raft
// engine, mirroring its JSON wire contract locally (no Go-module dependency
// on the engine). It follows not_leader hints so callers need not track the
// current leader.
package engineclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Account mirrors the engine account JSON.
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

// CreateAccountRequest mirrors POST /v1/accounts.
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited"`
	Type      string `json:"type,omitempty"`
}

// TransferRequest mirrors POST /v1/transfers.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// TransferResult mirrors the transfer/journal/reversal response.
type TransferResult struct {
	Status    string `json:"status"`
	JournalID string `json:"journal_id"`
	RaftIndex int64  `json:"raft_index"`
}

// PendingTransferRequest mirrors POST /v1/pending-transfers.
type PendingTransferRequest struct {
	DedupID        string `json:"dedup_id"`
	FromAccount    string `json:"from_account"`
	ToAccount      string `json:"to_account"`
	AmountMinor    int64  `json:"amount_minor"`
	Currency       string `json:"currency"`
	TimeoutSeconds int64  `json:"timeout_seconds"`
}

// PendingTransfer mirrors the hold-creation response.
type PendingTransfer struct {
	PendingID   string `json:"pending_id"`
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

// PendingActionRequest mirrors the post/void body.
type PendingActionRequest struct {
	DedupID string `json:"dedup_id"`
}

// ReverseRequest mirrors POST /v1/reversals.
type ReverseRequest struct {
	DedupID         string `json:"dedup_id"`
	OriginalDedupID string `json:"original_dedup_id"`
}

// SettlementKind mirrors the engine settlement kind.
type SettlementKind string

const (
	SettlementDeposit    SettlementKind = "deposit"
	SettlementWithdrawal SettlementKind = "withdrawal"
)

// CreateSettlementRequest mirrors POST /v1/settlements.
type CreateSettlementRequest struct {
	DedupID        string         `json:"dedup_id"`
	Kind           SettlementKind `json:"kind"`
	AccountID      string         `json:"account_id"`
	FundingAccount string         `json:"funding_account"`
	AmountMinor    int64          `json:"amount_minor"`
	Currency       string         `json:"currency"`
	TimeoutSeconds int64          `json:"timeout_seconds"`
}

// SettlementActionRequest mirrors mark-processing / settle-outcome bodies.
type SettlementActionRequest struct {
	DedupID string `json:"dedup_id"`
	Outcome string `json:"outcome,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// Settlement mirrors the engine settlement entry.
type Settlement struct {
	SettlementID   string         `json:"settlement_id"`
	DedupID        string         `json:"dedup_id"`
	Kind           SettlementKind `json:"kind"`
	AccountID      string         `json:"account_id"`
	FundingAccount string         `json:"funding_account"`
	AmountMinor    int64          `json:"amount_minor"`
	Currency       string         `json:"currency"`
	Status         string         `json:"status"`
	JournalDedupID string         `json:"journal_dedup_id,omitempty"`
}

// Error is a non-2xx engine response.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	LeaderHint string
}

func (e *Error) Error() string {
	return fmt.Sprintf("engineclient: %s (code=%s, status=%d)", e.Message, e.Code, e.StatusCode)
}

// Client talks to a ledgerline-raft cluster with leader-follow retry.
type Client struct {
	HTTPClient *http.Client
	APIKey     string
	nodes      []string
	lastLeader string
}

// New creates a client over the given node addresses (host:port, no scheme).
func New(nodes []string) *Client {
	cp := make([]string, len(nodes))
	copy(cp, nodes)
	return &Client{HTTPClient: &http.Client{Timeout: 10 * time.Second}, nodes: cp}
}

func (c *Client) do(ctx context.Context, method, path string, body, out interface{}) error {
	candidates := c.order()
	if len(candidates) == 0 {
		return fmt.Errorf("engineclient: no nodes configured")
	}
	var data []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		data = b
	}
	tried := make(map[string]bool)
	next := candidates[0]
	for attempt := 0; attempt < 5; attempt++ {
		addr := next
		tried[addr] = true
		status, respBody, err := c.once(ctx, method, addr, path, data)
		if err != nil {
			c.lastLeader = ""
			next = pick(candidates, tried)
			if next == "" {
				return fmt.Errorf("engineclient: no reachable node: %w", err)
			}
			continue
		}
		if status >= 200 && status < 300 {
			c.lastLeader = addr
			if out != nil && len(respBody) > 0 {
				return json.Unmarshal(respBody, out)
			}
			return nil
		}
		var ae struct {
			Error struct {
				Code       string `json:"code"`
				Message    string `json:"message"`
				LeaderHint string `json:"leader_hint"`
			} `json:"error"`
		}
		_ = json.Unmarshal(respBody, &ae)
		if status == http.StatusConflict && ae.Error.Code == "not_leader" && ae.Error.LeaderHint != "" && !tried[ae.Error.LeaderHint] {
			c.lastLeader = ""
			next = ae.Error.LeaderHint
			continue
		}
		return &Error{StatusCode: status, Code: ae.Error.Code, Message: ae.Error.Message, LeaderHint: ae.Error.LeaderHint}
	}
	return fmt.Errorf("engineclient: giving up after 5 attempts")
}

func (c *Client) order() []string {
	out := make([]string, 0, len(c.nodes)+1)
	seen := make(map[string]bool)
	if c.lastLeader != "" {
		out = append(out, c.lastLeader)
		seen[c.lastLeader] = true
	}
	for _, n := range c.nodes {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out
}

func pick(candidates []string, tried map[string]bool) string {
	for _, c := range candidates {
		if !tried[c] {
			return c
		}
	}
	return ""
}

func (c *Client) once(ctx context.Context, method, addr, path string, data []byte) (int, []byte, error) {
	var reader io.Reader
	if data != nil {
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("X-API-Key", c.APIKey)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, b, err
}

// CreateAccount creates an account.
func (c *Client) CreateAccount(ctx context.Context, req CreateAccountRequest) (*Account, error) {
	var out Account
	if err := c.do(ctx, http.MethodPost, "/v1/accounts", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccount reads an account.
func (c *Client) GetAccount(ctx context.Context, id string) (*Account, error) {
	var out Account
	if err := c.do(ctx, http.MethodGet, "/v1/accounts/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AssertTransfer submits a two-party transfer.
func (c *Client) AssertTransfer(ctx context.Context, req TransferRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/transfers", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePending creates a two-phase hold.
func (c *Client) CreatePending(ctx context.Context, req PendingTransferRequest) (*PendingTransfer, error) {
	var out PendingTransfer
	if err := c.do(ctx, http.MethodPost, "/v1/pending-transfers", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PostPending posts a hold.
func (c *Client) PostPending(ctx context.Context, pendingID string, req PendingActionRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/pending-transfers/"+pendingID+"/post", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// VoidPending voids a hold.
func (c *Client) VoidPending(ctx context.Context, pendingID string, req PendingActionRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/pending-transfers/"+pendingID+"/void", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reverse reverses a previously applied entry.
func (c *Client) Reverse(ctx context.Context, req ReverseRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/reversals", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSettlement records a settlement intent in the ledger FSM.
func (c *Client) CreateSettlement(ctx context.Context, req CreateSettlementRequest) (*Settlement, error) {
	var out Settlement
	if err := c.do(ctx, http.MethodPost, "/v1/settlements", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MarkSettlementProcessing marks a settlement submitted to the rail.
func (c *Client) MarkSettlementProcessing(ctx context.Context, id string, req SettlementActionRequest) (*Settlement, error) {
	var out Settlement
	if err := c.do(ctx, http.MethodPost, "/v1/settlements/"+id+"/mark-processing", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SettleOutcome applies a rail outcome to a settlement.
func (c *Client) SettleOutcome(ctx context.Context, id string, req SettlementActionRequest) (*Settlement, error) {
	var out Settlement
	if err := c.do(ctx, http.MethodPost, "/v1/settlements/"+id+"/settle-outcome", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSettlement reads a settlement entry.
func (c *Client) GetSettlement(ctx context.Context, id string) (*Settlement, error) {
	var out Settlement
	if err := c.do(ctx, http.MethodGet, "/v1/settlements/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
