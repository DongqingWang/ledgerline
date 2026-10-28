// Command ledgerline-orchestrator runs a demo shard-split controller for
// Ledgerline. It maintains an in-memory routing table mapping hashed
// account-ID ranges to the ledgerline-raft cluster that currently owns
// them, and exposes an endpoint to perform a "split": stand up a new
// cluster for a sub-range, replay that range's history into it, health
// check it, and atomically swap the routing table - or abort cleanly if
// anything looks wrong.
//
// See README.md for the HTTP API, how to run this against live
// ledgerline-raft clusters, and this demo's known simplifications.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/ledgerline/ledgerline-orchestrator/internal/controller"
	"github.com/ledgerline/ledgerline-orchestrator/internal/httpapi"
	"github.com/ledgerline/ledgerline-orchestrator/internal/ledgerclient"
	"github.com/ledgerline/ledgerline-orchestrator/internal/shard"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8480", "address for the orchestrator's own HTTP API")
	initialNodes := flag.String("initial-ledger-nodes", "", "comma-separated HTTP addresses of the initial ledgerline-raft cluster's nodes (required); this cluster is assigned the single starting shard covering the full key range")
	flag.Parse()

	logger := log.New(os.Stderr, "ledgerline-orchestrator ", log.LstdFlags)

	nodes := splitAndTrim(*initialNodes)
	if len(nodes) == 0 {
		fmt.Fprintln(os.Stderr, "error: -initial-ledger-nodes is required (comma-separated list of host:port addresses)")
		flag.Usage()
		os.Exit(2)
	}

	table := shard.NewTable(nodes)
	client := ledgerclient.NewClient()
	orch := controller.New(table, client, logger)

	server := &httpapi.Server{
		Table:        table,
		Orchestrator: orch,
		Logger:       logger,
	}

	logger.Printf("starting on %s with initial shard [0, %d) -> %v", *httpAddr, shard.MaxRange, nodes)
	if err := http.ListenAndServe(*httpAddr, server.NewMux()); err != nil {
		logger.Fatalf("server failed: %v", err)
	}
}

func splitAndTrim(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
