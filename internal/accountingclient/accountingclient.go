// Package accountingclient implements an HTTP client for
// ledgerline-accounting-core's journal composition endpoint.
//
// This package deliberately does not import ledgerline-accounting-core as a
// Go module dependency. It only knows about the JSON wire contract
// documented in that service's README, mirrored here as local structs.
package accountingclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ComposeRequest mirrors the POST /v1/journal/compose request body.
type ComposeRequest struct {
	FromAccount string `json:"from_account"`
	ToAccount   string `json:"to_account"`
	AmountMinor int64  `json:"amount_minor"`
	FeeMinor    int64  `json:"fee_minor,omitempty"`
	Currency    string `json:"currency"`
	Category    string `json:"category"`
	Country     string `json:"country"`
}

// JournalLine mirrors one line of a composed Journal.
type JournalLine struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"` // "debit" or "credit"
	AmountMinor int64  `json:"amount_minor"`
	GLAccount   string `json:"gl_account"`
	CostCenter  string `json:"cost_center"`
}

// Journal mirrors the response of POST /v1/journal/compose.
type Journal struct {
	Lines []JournalLine `json:"lines"`
}

// errorResponse mirrors ledgerline-accounting-core's simple error shape.
type errorResponse struct {
	Error string `json:"error"`
}

// Error is returned when accountingsvc responds with a non-2xx status.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("accountingclient: %s (status=%d)", e.Message, e.StatusCode)
}

// Client talks to a single ledgerline-accounting-core instance.
type Client struct {
	Addr       string // base address, e.g. "http://127.0.0.1:8090"
	HTTPClient *http.Client
}

// New creates a Client pointed at addr (a full base URL, e.g.
// "http://127.0.0.1:8090").
func New(addr string) *Client {
	return &Client{
		Addr:       addr,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Compose calls POST /v1/journal/compose and returns the resulting Journal.
func (c *Client) Compose(req ComposeRequest) (*Journal, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.Addr+"/v1/journal/compose", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("accountingclient: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("accountingclient: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp errorResponse
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = string(respBody)
		}
		return nil, &Error{StatusCode: resp.StatusCode, Message: msg}
	}

	var journal Journal
	if err := json.Unmarshal(respBody, &journal); err != nil {
		return nil, fmt.Errorf("accountingclient: decoding response: %w", err)
	}
	return &journal, nil
}
