// Package engineclient is a minimal HTTP client for the ledgerline-raft
// engine, mirroring its JSON wire contract locally (no Go-module dependency).
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
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Unlimited    bool   `json:"unlimited"`
	Type         string `json:"type,omitempty"`
}

// CreateAccountRequest mirrors POST /v1/accounts.
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited"`
	Type      string `json:"type,omitempty"`
}

// JournalLine is one leg of a multi-leg journal.
type JournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// JournalRequest mirrors POST /v1/journals.
type JournalRequest struct {
	DedupID string        `json:"dedup_id"`
	Lines   []JournalLine `json:"lines"`
}

// TransferResult mirrors the journal response.
type TransferResult struct {
	Status    string `json:"status"`
	JournalID string `json:"journal_id"`
	RaftIndex int64  `json:"raft_index"`
}

// ReverseRequest mirrors POST /v1/reversals.
type ReverseRequest struct {
	DedupID         string `json:"dedup_id"`
	OriginalDedupID string `json:"original_dedup_id"`
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

// New creates a client over node addresses (host:port, no scheme).
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

// AssertJournal submits an atomic multi-leg journal (idempotent on DedupID).
func (c *Client) AssertJournal(ctx context.Context, req JournalRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/journals", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reverse reverses a previously applied journal.
func (c *Client) Reverse(ctx context.Context, req ReverseRequest) (*TransferResult, error) {
	var out TransferResult
	if err := c.do(ctx, http.MethodPost, "/v1/reversals", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
