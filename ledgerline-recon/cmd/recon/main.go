// Command recon runs the external reconciliation service: it matches the
// ledger's recorded journals against external authoritative records (scheme
// clearing files, bank statements), surfaces breaks, and posts adjustment
// journals for money that moved externally but was never recorded.
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

	"github.com/ledgerline/ledgerline-recon/internal/engineclient"
	"github.com/ledgerline/ledgerline-recon/internal/httpapi"
	"github.com/ledgerline/ledgerline-recon/internal/recon"
	"github.com/ledgerline/ledgerline-recon/internal/stream"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8980", "recon service listen address")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated ledgerline-raft node HTTP addresses (required)")
	ledgerAPIKey := flag.String("ledger-api-key", "", "engine admin API key when auth is enabled")
	flag.Parse()

	if strings.TrimSpace(*ledgerNodes) == "" {
		log.Fatal("recon: -ledger-nodes is required")
	}
	nodes := splitTrim(*ledgerNodes)
	logger := log.New(os.Stderr, "ledgerline-recon ", log.LstdFlags)

	engine := engineclient.New(nodes)
	engine.APIKey = *ledgerAPIKey
	svc := recon.NewService(engine)
	svc.Logger = logger

	// Tail the ledger event stream to collect the ledger side automatically;
	// the external side (scheme/bank files) is ingested via the API.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st := &stream.Streamer{Nodes: nodes, APIKey: *ledgerAPIKey, Handler: svc.HandleEvent, Logger: logger}
	go func() {
		if err := st.Run(ctx); err != nil && ctx.Err() == nil {
			logger.Printf("event stream stopped: %v", err)
		}
	}()

	server := &httpapi.Server{Svc: svc}
	logger.Printf("listening on %s (ledger %v, tailing event stream)", *httpAddr, nodes)
	if err := http.ListenAndServe(*httpAddr, server.NewMux()); err != nil {
		logger.Fatalf("server exited: %v", err)
	}
}

func splitTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
