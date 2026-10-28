// Command ledgerline-balance-reader is a fast CQRS read-path balance
// service for Ledgerline. It tails the committed event stream from a
// ledgerline-raft cluster into an in-memory cache, and serves balance
// queries from that cache instead of hitting the ledger engine's own read
// path on every request. See README.md for the full rationale.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const shutdownTimeout = 5 * time.Second

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8280", "this service's own HTTP listen address")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated list of candidate ledgerline-raft HTTP addresses, e.g. 127.0.0.1:8201,127.0.0.1:8202")
	flag.Parse()

	nodes := splitAndTrim(*ledgerNodes)
	if len(nodes) == 0 {
		log.Fatal("-ledger-nodes is required (comma-separated list of ledgerline-raft HTTP addresses)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cache := NewBalanceCache()

	streamer := NewStreamer(nodes, cache)
	go streamer.Run(ctx)

	srv := &http.Server{
		Addr:    *httpAddr,
		Handler: withCORS(NewServer(cache)),
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("ledgerline-balance-reader listening on %s, tailing ledger nodes: %v", *httpAddr, nodes)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http server error: %v", err)
	}
}

// withCORS allows any origin to call this API. Fine for a local demo tool
// (e.g. ledgerline-admin-console running on a different dev-server port) —
// NOT appropriate for a public/production deployment, which would need a
// real allowlist and auth instead of a permissive wildcard.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func splitAndTrim(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
