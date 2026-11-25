// Command recon runs the external reconciliation service: it matches the
// ledger's recorded journals against external authoritative records (scheme
// clearing files, bank statements), surfaces breaks, and posts adjustment
// journals for money that moved externally but was never recorded.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/ledgerline/ledgerline-recon/internal/engineclient"
	"github.com/ledgerline/ledgerline-recon/internal/httpapi"
	"github.com/ledgerline/ledgerline-recon/internal/recon"
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
