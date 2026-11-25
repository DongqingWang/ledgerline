// Package stream is a minimal ledger event-stream consumer: it tails
// GET /v1/events/stream (NDJSON), dispatches each event to a handler, tracks
// the raft-index watermark, and reconnects (from the watermark) on error.
package stream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Line is one leg of a journal_applied event.
type Line struct {
	AccountID   string `json:"account_id"`
	Direction   string `json:"direction"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	GLAccount   string `json:"gl_account,omitempty"`
	CostCenter  string `json:"cost_center,omitempty"`
}

// Event mirrors the subset of the ledger event wire contract consumers need.
type Event struct {
	RaftIndex   uint64 `json:"raft_index"`
	Kind        string `json:"kind"`
	DedupID     string `json:"dedup_id,omitempty"`
	AmountMinor int64  `json:"amount_minor,omitempty"`
	Currency    string `json:"currency,omitempty"`
	Lines       []Line `json:"lines,omitempty"`
}

// Streamer tails the event stream and feeds each event to Handler.
type Streamer struct {
	Nodes   []string
	APIKey  string
	Handler func(Event)
	Logger  *log.Logger

	lastIndex uint64
	hasAny    bool
	client    *http.Client
}

// Run blocks, streaming events until ctx is cancelled. It reconnects from the
// watermark on any error.
func (s *Streamer) Run(ctx context.Context) error {
	s.client = &http.Client{} // no timeout: long-lived stream
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		node := s.Nodes[0]
		if err := s.streamFrom(ctx, node); err != nil && ctx.Err() == nil {
			if s.Logger != nil {
				s.Logger.Printf("stream from %s ended: %v; reconnecting from index %d", node, err, s.lastIndex)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Streamer) streamFrom(ctx context.Context, node string) error {
	from := uint64(0)
	if s.hasAny {
		from = s.lastIndex + 1
	}
	url := fmt.Sprintf("http://%s/v1/events/stream?from_index=%d", node, from)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if s.APIKey != "" {
		req.Header.Set("X-API-Key", s.APIKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stream status %d: %s", resp.StatusCode, string(body))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		s.Handler(ev)
		s.lastIndex = ev.RaftIndex
		s.hasAny = true
	}
	return sc.Err()
}
