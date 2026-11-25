// Command acquiring runs the card-acquiring service: it processes payments
// (auth -> capture) against a simulated scheme and records balanced journals
// in the ledgerline-raft ledger, with dual-write protection.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/ledgerline/ledgerline-acquiring/internal/acquiring"
	"github.com/ledgerline/ledgerline-acquiring/internal/engineclient"
	"github.com/ledgerline/ledgerline-acquiring/internal/httpapi"
	"github.com/ledgerline/ledgerline-acquiring/internal/scheme"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8780", "acquiring service listen address")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated ledgerline-raft node HTTP addresses (required)")
	ledgerAPIKey := flag.String("ledger-api-key", "", "engine admin API key when auth is enabled")
	failAmounts := flag.String("fail-amounts", "", "comma-separated amount_minor values the simulated scheme declines")
	flag.Parse()

	if strings.TrimSpace(*ledgerNodes) == "" {
		log.Fatal("acquiring: -ledger-nodes is required")
	}
	nodes := splitTrim(*ledgerNodes)
	logger := log.New(os.Stderr, "ledgerline-acquiring ", log.LstdFlags)

	engine := engineclient.New(nodes)
	engine.APIKey = *ledgerAPIKey

	svc := acquiring.NewService(engine, scheme.NewSimulated(buildFailSet(*failAmounts)), acquiring.NewStore(nil))
	svc.Logger = logger

	server := &httpapi.Server{Svc: svc}
	logger.Printf("listening on %s (ledger %v)", *httpAddr, nodes)
	if err := http.ListenAndServe(*httpAddr, server.NewMux()); err != nil {
		logger.Fatalf("server exited: %v", err)
	}
}

func buildFailSet(csv string) map[int64]bool {
	out := make(map[int64]bool)
	for _, s := range splitTrim(csv) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			out[n] = true
		}
	}
	return out
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
