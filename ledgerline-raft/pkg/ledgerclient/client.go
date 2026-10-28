// Package ledgerclient is a small Go client for the ledgerline-raft HTTP
// API. It implements the leader-discovery-and-retry pattern described in the
// Ledgerline architecture docs: a client doesn't need to know which node is
// currently the raft leader — it tries a candidate, and on a 409 "not
// leader" response follows the returned leader_hint, up to a bounded number
// of attempts (mirroring the "retry across up to 5 nodes" framing from the
// original design).
package ledgerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/ledgerline/ledgerline-raft/pkg/ledgertypes"
)

const maxAttempts = 5

// Client talks to a ledgerline-raft cluster given the HTTP addresses of its
// members. It does not need to know in advance which one is the leader.
type Client struct {
	httpClient *http.Client

	mu          sync.Mutex
	nodeAddrs   []string
	leaderGuess string // empty until we've learned one
}

func New(nodeAddrs []string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 5 * time.Second},
		nodeAddrs:  nodeAddrs,
	}
}

func (c *Client) candidates() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.leaderGuess == "" {
		out := make([]string, len(c.nodeAddrs))
		copy(out, c.nodeAddrs)
		return out
	}
	// Try our current best guess first, then fall back to the rest.
	out := []string{c.leaderGuess}
	for _, a := range c.nodeAddrs {
		if a != c.leaderGuess {
			out = append(out, a)
		}
	}
	return out
}

func (c *Client) rememberLeader(addr string) {
	if addr == "" {
		return
	}
	c.mu.Lock()
	c.leaderGuess = addr
	c.mu.Unlock()
}

// CreateAccount creates a new zero-balance account, retrying against the
// cluster's current leader as needed.
func (c *Client) CreateAccount(ctx context.Context, req ledgertypes.CreateAccountRequest) (*ledgertypes.Account, error) {
	var out ledgertypes.Account
	if err := c.writeRequest(ctx, "POST", "/v1/accounts", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AssertTransfer submits an idempotent conditional transfer. Safe to retry
// client-side with the same DedupID after a timeout — the engine will
// report TransferStatusDuplicate rather than double-applying it.
func (c *Client) AssertTransfer(ctx context.Context, req ledgertypes.TransferRequest) (*ledgertypes.TransferResult, error) {
	var out ledgertypes.TransferResult
	if err := c.writeRequest(ctx, "POST", "/v1/transfers", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetAccount reads account state from the first reachable node (not
// necessarily the leader — see the server-side caveat about read
// linearizability).
func (c *Client) GetAccount(ctx context.Context, accountID string) (*ledgertypes.Account, error) {
	var out ledgertypes.Account
	for _, addr := range c.candidates() {
		err := c.getJSON(ctx, addr, "/v1/accounts/"+accountID, &out)
		if err == nil {
			return &out, nil
		}
	}
	return nil, fmt.Errorf("account %q: no reachable node returned it", accountID)
}

// writeRequest implements the leader-follow-retry loop for mutating calls.
func (c *Client) writeRequest(ctx context.Context, method, path string, body, out interface{}) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		candidates := c.candidates()
		if len(candidates) == 0 {
			return fmt.Errorf("no known node addresses configured")
		}
		addr := candidates[0]

		req, err := http.NewRequestWithContext(ctx, method, addr+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			c.rememberLeader("") // forget a dead guess, fall back to round-robin next attempt
			continue
		}

		if resp.StatusCode == http.StatusConflict {
			var eb ledgertypes.ErrorBody
			_ = json.NewDecoder(resp.Body).Decode(&eb)
			resp.Body.Close()
			if eb.Error.Code == ledgertypes.ErrNotLeader && eb.Error.LeaderHint != "" {
				c.rememberLeader(eb.Error.LeaderHint)
				lastErr = fmt.Errorf("not leader, redirected to %s", eb.Error.LeaderHint)
				continue
			}
			lastErr = fmt.Errorf("conflict: %s", eb.Error.Message)
			continue
		}

		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			var eb ledgertypes.ErrorBody
			_ = json.NewDecoder(resp.Body).Decode(&eb)
			return fmt.Errorf("%s: %s", eb.Error.Code, eb.Error.Message)
		}

		c.rememberLeader(addr) // this node answered successfully as leader
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return fmt.Errorf("giving up after %d attempts: %w", maxAttempts, lastErr)
}

func (c *Client) getJSON(ctx context.Context, addr, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, "GET", addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, addr)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// StreamEvents tails the engine's committed-event stream from fromIndex,
// invoking handler for each event until ctx is cancelled or handler returns
// an error. It automatically follows redirects to whichever node is
// reachable (any node can serve reads of the committed log).
func (c *Client) StreamEvents(ctx context.Context, fromIndex uint64, handler func(ledgertypes.LedgerEvent) error) error {
	var lastErr error
	for _, addr := range c.candidates() {
		url := fmt.Sprintf("%s/v1/events/stream?from_index=%d", addr, fromIndex)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 300 {
			resp.Body.Close()
			lastErr = fmt.Errorf("unexpected status %d from %s", resp.StatusCode, addr)
			continue
		}

		dec := json.NewDecoder(resp.Body)
		for {
			var ev ledgertypes.LedgerEvent
			if err := dec.Decode(&ev); err != nil {
				resp.Body.Close()
				return err
			}
			if err := handler(ev); err != nil {
				resp.Body.Close()
				return err
			}
		}
	}
	return fmt.Errorf("could not open event stream against any node: %w", lastErr)
}
