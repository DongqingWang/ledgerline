package ledgerclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to ledgerline-raft nodes over plain HTTP using only
// encoding/json and net/http from the standard library.
type Client struct {
	HTTPClient *http.Client
}

// NewClient returns a Client with a sane default timeout for ordinary
// request/response calls. Use StreamEvents separately for the long-lived
// NDJSON stream, which manages its own deadlines.
func NewClient() *Client {
	return &Client{HTTPClient: &http.Client{Timeout: 10 * time.Second}}
}

func addrToURL(addr, path string) string {
	return fmt.Sprintf("http://%s%s", addr, path)
}

// doJSON issues an HTTP request with an optional JSON body and decodes a
// JSON response into out (if out is non-nil). On a non-2xx response it
// attempts to decode the standard ledgerline-raft error envelope and
// returns it as the error.
func (c *Client) doJSON(ctx context.Context, method, url string, body, out interface{}) (int, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("request %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr APIError
		if jsonErr := json.Unmarshal(respBody, &apiErr); jsonErr == nil && apiErr.Error.Code != "" {
			return resp.StatusCode, fmt.Errorf("%s %s: %s (%s), leader_hint=%q", method, url, apiErr.Error.Message, apiErr.Error.Code, apiErr.Error.LeaderHint)
		}
		return resp.StatusCode, fmt.Errorf("%s %s: unexpected status %d: %s", method, url, resp.StatusCode, string(respBody))
	}

	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response body: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// Health fetches the /healthz state of a single node.
func (c *Client) Health(ctx context.Context, addr string) (HealthResponse, error) {
	var out HealthResponse
	_, err := c.doJSON(ctx, http.MethodGet, addrToURL(addr, "/healthz"), nil, &out)
	return out, err
}

// Leader fetches the current leader info by asking the given node.
func (c *Client) Leader(ctx context.Context, addr string) (LeaderInfo, error) {
	var out LeaderInfo
	_, err := c.doJSON(ctx, http.MethodGet, addrToURL(addr, "/v1/leader"), nil, &out)
	return out, err
}

// LeaderAddrAmong asks each candidate address in turn for the cluster's
// current leader and returns the first successful answer's
// leader_http_addr. This lets callers find a usable leader even if some
// nodes in the list are down or mid-election.
func (c *Client) LeaderAddrAmong(ctx context.Context, addrs []string) (string, error) {
	var lastErr error
	for _, addr := range addrs {
		info, err := c.Leader(ctx, addr)
		if err != nil {
			lastErr = err
			continue
		}
		if info.LeaderHTTPAddr != "" {
			return info.LeaderHTTPAddr, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no node in %v reported a leader", addrs)
	}
	return "", lastErr
}

// GetAccount fetches an account's current state from any node.
func (c *Client) GetAccount(ctx context.Context, addr, accountID string) (Account, error) {
	var out Account
	_, err := c.doJSON(ctx, http.MethodGet, addrToURL(addr, "/v1/accounts/"+accountID), nil, &out)
	return out, err
}

// CreateAccount creates an account on the given (leader) node.
func (c *Client) CreateAccount(ctx context.Context, leaderAddr string, req CreateAccountRequest) (Account, error) {
	var out Account
	_, err := c.doJSON(ctx, http.MethodPost, addrToURL(leaderAddr, "/v1/accounts"), req, &out)
	return out, err
}

// SubmitTransfer submits a transfer on the given (leader) node. It is
// idempotent on DedupID, so safe to retry.
func (c *Client) SubmitTransfer(ctx context.Context, leaderAddr string, req TransferRequest) (TransferResponse, error) {
	var out TransferResponse
	_, err := c.doJSON(ctx, http.MethodPost, addrToURL(leaderAddr, "/v1/transfers"), req, &out)
	return out, err
}

// StreamEvents reads the NDJSON event stream from addr starting at
// fromIndex and returns every event seen before the stream is considered
// "caught up".
//
// ledgerline-raft's /v1/events/stream has no explicit "end of backlog"
// marker - it's a long-lived connection that replays history and then
// keeps streaming live events forever. For the orchestrator's
// replay-and-filter use case we only need the backlog (everything
// committed so far), so we use an idle-gap heuristic: we keep reading
// events, resetting a read deadline after each one, and stop (closing the
// connection) once no new event has arrived within idleTimeout. This
// assumes the cluster is quiescent (or close to it) while the split is
// running; a system with continuous live traffic would need a different
// signal (e.g. comparing against the leader's last known raft_index).
func (c *Client) StreamEvents(ctx context.Context, addr string, fromIndex int64, idleTimeout time.Duration) ([]Event, error) {
	url := addrToURL(addr, fmt.Sprintf("/v1/events/stream?from_index=%d", fromIndex))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Use a client without the default fixed Timeout, since this is a
	// long-lived connection; we bound it ourselves via the deadline loop
	// below and the caller's context.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("open event stream %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("open event stream %s: unexpected status %d: %s", url, resp.StatusCode, string(body))
	}

	type lineResult struct {
		line []byte
		err  error
	}
	lines := make(chan lineResult)
	done := make(chan struct{})
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case lines <- lineResult{line: append([]byte(nil), scanner.Bytes()...)}:
			case <-done:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case lines <- lineResult{err: err}:
			case <-done:
			}
		}
	}()
	defer close(done)

	var events []Event
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return events, ctx.Err()
		case <-timer.C:
			// Idle gap elapsed with no new event: treat as caught up.
			return events, nil
		case res, ok := <-lines:
			if !ok {
				// Server closed the connection; whatever we have is the
				// full backlog.
				return events, nil
			}
			if res.err != nil {
				return events, fmt.Errorf("read event stream: %w", res.err)
			}
			if len(bytes.TrimSpace(res.line)) == 0 {
				continue
			}
			var ev Event
			if err := json.Unmarshal(res.line, &ev); err != nil {
				return events, fmt.Errorf("decode event line %q: %w", string(res.line), err)
			}
			events = append(events, ev)
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(idleTimeout)
		}
	}
}
