package main

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

// Streamer connects to one of several candidate ledgerline-raft HTTP nodes,
// tails GET /v1/events/stream, and applies every event to a BalanceCache.
// If the connection drops or a candidate node is unreachable, it reconnects
// -- trying candidates in order -- resuming from the last-processed
// raft_index+1 so no events are skipped or double counted.
type Streamer struct {
	nodes  []string
	cache  *BalanceCache
	client *http.Client
}

// NewStreamer builds a Streamer over the given candidate node HTTP
// addresses (host:port, no scheme).
func NewStreamer(nodes []string, cache *BalanceCache) *Streamer {
	return &Streamer{
		nodes: nodes,
		cache: cache,
		client: &http.Client{
			// No overall timeout: this is a long-lived streaming connection.
			// Per-attempt dial/header timeouts are handled via the request
			// context and transport settings below.
			Transport: &http.Transport{
				ResponseHeaderTimeout: 10 * time.Second,
			},
		},
	}
}

// Run drives the connect/replay/reconnect loop until ctx is canceled.
func (s *Streamer) Run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		connected := false
		for _, node := range s.nodes {
			if ctx.Err() != nil {
				return
			}

			// Recompute fromIndex fresh before every single connection
			// attempt (not once per pass over the candidate list): if an
			// earlier candidate in this same pass streamed us forward
			// before dropping, we must resume from that new high-water
			// mark, not from whatever it was when the pass started.
			fromIndex := uint64(0)
			if last, ok := s.cache.LastProcessedIndex(); ok {
				fromIndex = last + 1
			}

			log.Printf("streamer: attempting to connect to %s from_index=%d", node, fromIndex)
			err := s.streamFrom(ctx, node, fromIndex)
			if err == nil {
				// streamFrom only returns nil if ctx was canceled.
				return
			}
			if ctx.Err() != nil {
				return
			}
			log.Printf("streamer: connection to %s ended: %v", node, err)
			connected = true // we at least tried; move to next candidate
		}

		if !connected {
			log.Printf("streamer: no candidate nodes configured")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// streamFrom opens a stream to a single node starting at fromIndex and
// applies every decoded event to the cache until the connection ends or ctx
// is canceled. It returns nil only when ctx was canceled; any other
// termination (EOF, decode error, non-200 status) is returned as an error
// so the caller can try the next candidate node.
func (s *Streamer) streamFrom(ctx context.Context, node string, fromIndex uint64) error {
	url := fmt.Sprintf("http://%s/v1/events/stream?from_index=%d", node, fromIndex)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	log.Printf("streamer: connected to %s, replaying from_index=%d", node, fromIndex)

	dec := json.NewDecoder(bufio.NewReader(resp.Body))
	count := 0
	for {
		if ctx.Err() != nil {
			return nil
		}
		var ev LedgerEvent
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF {
				return fmt.Errorf("stream from %s closed (EOF)", node)
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("decode error from %s: %w", node, err)
		}
		s.cache.Apply(ev)
		count++
		if count%1000 == 0 {
			log.Printf("streamer: applied %d events from %s (last index %d)", count, node, ev.RaftIndex)
		}
	}
}
