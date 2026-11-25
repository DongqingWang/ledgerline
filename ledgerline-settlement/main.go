// Command ledgerline-settlement runs the async bank-settlement driver in front
// of a ledgerline-raft cluster. The settlement lifecycle lives in the ledger
// FSM (raft-durable); this service submits commands and drives a simulated
// rail, holding no authoritative state of its own.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ledgerline/ledgerline-settlement/internal/engineclient"
	"github.com/ledgerline/ledgerline-settlement/internal/httpapi"
	"github.com/ledgerline/ledgerline-settlement/internal/rail"
	"github.com/ledgerline/ledgerline-settlement/internal/settlement"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8680", "settlement service listen address")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated ledgerline-raft node HTTP addresses (required)")
	ledgerAPIKey := flag.String("ledger-api-key", "", "engine admin API key when auth is enabled")
	railDelay := flag.Duration("rail-delay", 1*time.Second, "simulated rail processing delay")
	failAmounts := flag.String("fail-amounts", "", "comma-separated amount_minor values the simulated rail rejects")
	fundingAccount := flag.String("funding-account", "bank-clearing", "unlimited nexus account representing the external bank")
	timeout := flag.Int64("settlement-timeout", 7*24*3600, "settlement timeout in seconds (covers rail window)")
	flag.Parse()

	if strings.TrimSpace(*ledgerNodes) == "" {
		log.Fatal("settlement: -ledger-nodes is required")
	}
	nodes := splitTrim(*ledgerNodes)
	logger := log.New(os.Stderr, "ledgerline-settlement ", log.LstdFlags)

	engine := engineclient.New(nodes)
	engine.APIKey = *ledgerAPIKey

	orch := settlement.NewOrchestrator(engine, nil) // rail set below
	orch.FundingAccount = *fundingAccount
	orch.TimeoutSeconds = *timeout
	orch.Logger = logger

	r := rail.New(*railDelay, buildRule(*failAmounts), func(id string, o rail.Outcome, reason string) {
		if err := orch.OnRailResult(context.Background(), id, string(o), reason); err != nil {
			logger.Printf("rail outcome for %s failed: %v", id, err)
		}
	})
	orch.Rail = r

	server := &httpapi.Server{Orch: orch}
	logger.Printf("listening on %s (ledger %v, rail delay %s)", *httpAddr, nodes, *railDelay)
	if err := http.ListenAndServe(*httpAddr, server.NewMux()); err != nil {
		logger.Fatalf("server exited: %v", err)
	}
}

func buildRule(csv string) rail.Rule {
	amounts := make(map[int64]bool)
	for _, s := range splitTrim(csv) {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			amounts[n] = true
		}
	}
	if len(amounts) == 0 {
		return rail.SettledRule
	}
	return rail.FailAmounts(amounts)
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
