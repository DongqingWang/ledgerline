// Command clearing runs the netting/clearing service: it accumulates gross
// captured transactions, nets them per (merchant, currency), and records a
// clearing journal that moves the net obligation to a clearing-suspense
// account ready for settlement payout.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/ledgerline/ledgerline-clearing/internal/clearing"
	"github.com/ledgerline/ledgerline-clearing/internal/engineclient"
	"github.com/ledgerline/ledgerline-clearing/internal/httpapi"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8880", "clearing service listen address")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated ledgerline-raft node HTTP addresses (required)")
	ledgerAPIKey := flag.String("ledger-api-key", "", "engine admin API key when auth is enabled")
	flag.Parse()

	if strings.TrimSpace(*ledgerNodes) == "" {
		log.Fatal("clearing: -ledger-nodes is required")
	}
	nodes := splitTrim(*ledgerNodes)
	logger := log.New(os.Stderr, "ledgerline-clearing ", log.LstdFlags)

	engine := engineclient.New(nodes)
	engine.APIKey = *ledgerAPIKey
	svc := clearing.NewService(engine)
	svc.Logger = logger

	server := &httpapi.Server{Svc: svc}
	logger.Printf("listening on %s (ledger %v)", *httpAddr, nodes)
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
