// Package fxclient implements an HTTP client for ledgerline-fx's
// conversion endpoint. Like the other clients in this repo it mirrors the
// JSON wire contract locally instead of importing ledgerline-fx as a Go
// module.
package fxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ConvertRequest mirrors the POST /v1/convert request body.
type ConvertRequest struct {
	Base        string `json:"base"`
	Quote       string `json:"quote"`
	AmountMinor int64  `json:"amount_minor"`
}

// ConvertResult mirrors the POST /v1/convert response body.
type ConvertResult struct {
	Base                 string  `json:"base"`
	Quote                string  `json:"quote"`
	AmountMinor          int64   `json:"amount_minor"`
	ConvertedAmountMinor int64   `json:"converted_amount_minor"`
	Rate                 float64 `json:"rate"`
}

// Error is returned when the fx service responds with a non-2xx status.
type Error struct {
	StatusCode int
	Message    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("fxclient: %s (status=%d)", e.Message, e.StatusCode)
}

// Client talks to a single ledgerline-fx instance.
type Client struct {
	Addr       string // base URL, e.g. "http://127.0.0.1:8095"
	HTTPClient *http.Client
}

// New creates a Client pointed at addr (a full base URL).
func New(addr string) *Client {
	return &Client{
		Addr:       addr,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Convert calls POST /v1/convert to convert amountMinor from base to quote.
func (c *Client) Convert(ctx context.Context, base, quote string, amountMinor int64) (*ConvertResult, error) {
	body, err := json.Marshal(ConvertRequest{Base: base, Quote: quote, AmountMinor: amountMinor})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Addr+"/v1/convert", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fxclient: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("fxclient: reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(respBody, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = string(respBody)
		}
		return nil, &Error{StatusCode: resp.StatusCode, Message: msg}
	}

	var out ConvertResult
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("fxclient: decoding response: %w", err)
	}
	return &out, nil
}
