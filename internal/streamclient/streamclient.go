// Package streamclient implements an HTTP client for the ledger engine's
// ndjson committed-event stream (GET /v1/events/stream?from_index=N). It
// talks to ledgerline-raft purely over HTTP + JSON, matching the wire
// contract mirrored in internal/events — it does not import ledgerline-raft
// as a Go dependency.
package streamclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
)

// Handler is invoked once per decoded LedgerEvent, in order. If it returns
// an error, the stream is aborted and the error is returned to the caller
// of Client.Stream.
type Handler func(context.Context, events.LedgerEvent) error

// Client streams committed ledger events from any reachable node in a list
// of candidate ledgerline-raft HTTP addresses.
type Client struct {
	// Candidates are "host:port" addresses of ledgerline-raft nodes. Any
	// node (leader or follower) can serve the event stream.
	Candidates []string
	HTTPClient *http.Client
}

// New constructs a Client for the given candidate addresses.
func New(candidates []string) *Client {
	return &Client{
		Candidates: candidates,
		// No per-request timeout: this is a long-lived streaming connection.
		// Connection establishment itself is bounded via the request context
		// passed to StreamOnce/Stream.
		HTTPClient: &http.Client{},
	}
}

// ErrNoCandidateReachable is returned when every candidate address in one
// pass failed to even establish the streaming connection.
var ErrNoCandidateReachable = errors.New("streamclient: no candidate ledger node accepted the stream connection")

// Stream connects to the first reachable candidate and feeds every event
// from fromIndex onward to handle, blocking until the connection drops, the
// handler returns an error, or ctx is cancelled. On a dropped connection it
// returns a non-nil error describing the drop (not necessarily
// ErrNoCandidateReachable) so the caller can decide whether/how to
// reconnect (this package does not loop/retry internally — see cmd/
// persistence-sync for the outer reconnect loop, which re-reads the resume
// point from Postgres between attempts).
func (c *Client) Stream(ctx context.Context, fromIndex int64, handle Handler) error {
	if len(c.Candidates) == 0 {
		return errors.New("streamclient: no candidate addresses configured")
	}

	var lastErr error
	for _, addr := range c.Candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := c.streamFrom(ctx, addr, fromIndex, handle)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.Printf("persistence-sync: stream from %s failed: %v", addr, err)
		lastErr = err
	}
	if lastErr != nil {
		return fmt.Errorf("%w: last error: %v", ErrNoCandidateReachable, lastErr)
	}
	return ErrNoCandidateReachable
}

// streamFrom opens the stream against a single candidate address and decodes
// events from it until EOF, an error, or ctx cancellation.
func (c *Client) streamFrom(ctx context.Context, addr string, fromIndex int64, handle Handler) error {
	url := fmt.Sprintf("http://%s/v1/events/stream?from_index=%d", addr, fromIndex)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	log.Printf("persistence-sync: streaming committed events from %s starting at raft_index >= %d", addr, fromIndex)

	dec := json.NewDecoder(resp.Body)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ev events.LedgerEvent
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("stream from %s closed (EOF)", addr)
			}
			return fmt.Errorf("decode event from %s: %w", addr, err)
		}
		if err := handle(ctx, ev); err != nil {
			return fmt.Errorf("handle event (raft_index=%d): %w", ev.RaftIndex, err)
		}
	}
}

// RunWithReconnect runs Stream in a loop, reconnecting with backoff whenever
// the connection drops, until ctx is cancelled. resumeFrom is called fresh
// before every (re)connect attempt so that resuming after a drop (or a
// process restart) always starts from the durable, DB-backed position
// rather than an in-memory counter.
func RunWithReconnect(ctx context.Context, client *Client, resumeFrom func(context.Context) (int64, error), handle Handler) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		lastIndex, err := resumeFrom(ctx)
		if err != nil {
			return fmt.Errorf("streamclient: resolve resume point: %w", err)
		}

		err = client.Stream(ctx, lastIndex+1, handle)
		if err == nil {
			// Stream returning nil without ctx cancellation shouldn't happen
			// in practice (streamFrom only returns nil-free paths), but treat
			// it as "reconnect" rather than "done" defensively.
			continue
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}

		log.Printf("persistence-sync: event stream disconnected, retrying in %s: %v", backoff, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
