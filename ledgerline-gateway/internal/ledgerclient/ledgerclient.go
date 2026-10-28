// Package ledgerclient implements an HTTP client for the ledgerline-raft
// cluster's client-facing REST API, with a leader-follow retry pattern so
// callers never need to know which node currently holds raft leadership.
//
// This package deliberately does not import ledgerline-raft as a Go module
// dependency. It only knows about the JSON wire contract documented in the
// ledgerline-raft README, mirrored here as local structs.
package ledgerclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// MaxAttempts caps the number of nodes the client will try for a single
// logical request, mirroring the "retry across up to 5 nodes" framing from
// the original architecture.
const MaxAttempts = 5

// Account mirrors ledgerline-raft's account JSON representation.
type Account struct {
	AccountID    string `json:"account_id"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
	Unlimited    bool   `json:"unlimited"`
}

// CreateAccountRequest mirrors the POST /v1/accounts request body.
type CreateAccountRequest struct {
	AccountID string `json:"account_id"`
	Currency  string `json:"currency"`
	Unlimited bool   `json:"unlimited"`
}

// TransferRequest mirrors the POST /v1/transfers request body.
type TransferRequest struct {
	DedupID     string `json:"dedup_id"`
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// TransferResult mirrors the POST /v1/transfers response body.
type TransferResult struct {
	Status    string `json:"status"`
	JournalID string `json:"journal_id"`
	RaftIndex int64  `json:"raft_index"`
}

// LeaderInfo mirrors the GET /v1/leader response body.
type LeaderInfo struct {
	LeaderID       string `json:"leader_id"`
	LeaderHTTPAddr string `json:"leader_http_addr"`
}

// apiError mirrors ledgerline-raft's error envelope.
type apiError struct {
	Error struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		LeaderHint string `json:"leader_hint"`
	} `json:"error"`
}

// Error is returned when a ledger node responds with a non-2xx status.
type Error struct {
	StatusCode int
	Code       string
	Message    string
	LeaderHint string
}

func (e *Error) Error() string {
	return fmt.Sprintf("ledgerclient: %s (code=%s, status=%d)", e.Message, e.Code, e.StatusCode)
}

// Client talks to a ledgerline-raft cluster using the leader-follow retry
// pattern: try a candidate node, and if told "not_leader" with a leader
// hint, retry against that hint. The last-known-good leader address is
// remembered across calls as an optimization.
type Client struct {
	HTTPClient *http.Client

	mu         sync.Mutex
	nodes      []string // static list of node http addresses, as configured
	lastLeader string   // last known-good leader address, "" if unknown
}

// New creates a Client that round-robins across nodes, starting with no
// known leader.
func New(nodes []string) *Client {
	cp := make([]string, len(nodes))
	copy(cp, nodes)
	return &Client{
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		nodes:      cp,
	}
}

// candidateOrder returns the sequence of node addresses to try, starting
// with the last-known leader (if any) followed by the configured node list
// (deduplicated).
func (c *Client) candidateOrder() []string {
	c.mu.Lock()
	leader := c.lastLeader
	nodes := make([]string, len(c.nodes))
	copy(nodes, c.nodes)
	c.mu.Unlock()

	order := make([]string, 0, len(nodes)+1)
	seen := make(map[string]bool)
	if leader != "" {
		order = append(order, leader)
		seen[leader] = true
	}
	for _, n := range nodes {
		if !seen[n] {
			order = append(order, n)
			seen[n] = true
		}
	}
	return order
}

func (c *Client) setLeader(addr string) {
	if addr == "" {
		return
	}
	c.mu.Lock()
	c.lastLeader = addr
	c.mu.Unlock()
}

func (c *Client) clearLeader(addr string) {
	c.mu.Lock()
	if c.lastLeader == addr {
		c.lastLeader = ""
	}
	c.mu.Unlock()
}

// doWithRetry sends a request to candidate nodes, following not_leader
// hints, up to MaxAttempts total tries. method/path/body describe the
// request to issue against each candidate's base address. On success, out
// (if non-nil) is populated by decoding the JSON response body.
func (c *Client) doWithRetry(method, path string, body []byte, out interface{}) error {
	candidates := c.candidateOrder()
	if len(candidates) == 0 {
		return errors.New("ledgerclient: no ledger nodes configured")
	}

	var lastErr error
	tried := make(map[string]bool)
	attempts := 0

	next := candidates[0]
	idx := 0

	for attempts < MaxAttempts {
		addr := next
		attempts++
		tried[addr] = true

		status, respBody, err := c.doOnce(method, addr, path, body)
		if err != nil {
			lastErr = err
			c.clearLeader(addr)
			// Network-level failure: fall back to round-robin over the
			// remaining configured nodes.
			next = pickUntried(candidates, tried, &idx)
			if next == "" {
				break
			}
			continue
		}

		if status >= 200 && status < 300 {
			c.setLeader(addr)
			if out != nil && len(respBody) > 0 {
				if err := json.Unmarshal(respBody, out); err != nil {
					return fmt.Errorf("ledgerclient: decoding response from %s: %w", addr, err)
				}
			}
			return nil
		}

		var ae apiError
		_ = json.Unmarshal(respBody, &ae)

		if status == http.StatusConflict && ae.Error.Code == "not_leader" && ae.Error.LeaderHint != "" {
			c.clearLeader(addr)
			lastErr = &Error{StatusCode: status, Code: ae.Error.Code, Message: ae.Error.Message, LeaderHint: ae.Error.LeaderHint}
			hint := ae.Error.LeaderHint
			if tried[hint] {
				// Already tried this hint; fall back to round robin to
				// avoid looping forever against a stale hint.
				next = pickUntried(candidates, tried, &idx)
				if next == "" {
					break
				}
				continue
			}
			next = hint
			continue
		}

		// Any other non-2xx is a terminal error for this request; the
		// problem isn't "wrong node", it's an application-level failure.
		return &Error{StatusCode: status, Code: ae.Error.Code, Message: ae.Error.Message, LeaderHint: ae.Error.LeaderHint}
	}

	if lastErr != nil {
		return fmt.Errorf("ledgerclient: giving up after %d attempts: %w", attempts, lastErr)
	}
	return fmt.Errorf("ledgerclient: giving up after %d attempts: no reachable node", attempts)
}

// pickUntried returns the next candidate address not yet tried, walking the
// candidate list starting at *idx (which is advanced). Returns "" if all
// candidates have been tried.
func pickUntried(candidates []string, tried map[string]bool, idx *int) string {
	for i := 0; i < len(candidates); i++ {
		c := candidates[*idx%len(candidates)]
		*idx++
		if !tried[c] {
			return c
		}
	}
	return ""
}

func (c *Client) doOnce(method, addr, path string, body []byte) (int, []byte, error) {
	url := "http://" + addr + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// CreateAccount creates an account on the leader, following not_leader
// hints as needed.
func (c *Client) CreateAccount(req CreateAccountRequest) (*Account, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var acct Account
	if err := c.doWithRetry(http.MethodPost, "/v1/accounts", body, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

// Transfer submits a transfer to the leader, following not_leader hints as
// needed.
func (c *Client) Transfer(req TransferRequest) (*TransferResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var result TransferResult
	if err := c.doWithRetry(http.MethodPost, "/v1/transfers", body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetAccount fetches an account's current state. Any reachable node can
// serve this (follower reads may be slightly stale).
func (c *Client) GetAccount(accountID string) (*Account, error) {
	var acct Account
	if err := c.doWithRetry(http.MethodGet, "/v1/accounts/"+accountID, nil, &acct); err != nil {
		return nil, err
	}
	return &acct, nil
}

// Leader returns the cluster's current leader info, as reported by whichever
// node answers first.
func (c *Client) Leader() (*LeaderInfo, error) {
	var info LeaderInfo
	if err := c.doWithRetry(http.MethodGet, "/v1/leader", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}
