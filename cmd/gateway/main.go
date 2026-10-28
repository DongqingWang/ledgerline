// Command gateway runs ledgerline-gateway: the client-facing REST API in
// front of a ledgerline-raft cluster, with leader discovery/retry baked in
// and journal composition via ledgerline-accounting-core.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"github.com/ledgerline/ledgerline-gateway/internal/accountingclient"
	"github.com/ledgerline/ledgerline-gateway/internal/gatewayapi"
	"github.com/ledgerline/ledgerline-gateway/internal/ledgerclient"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8080", "address for the gateway's own HTTP listener")
	ledgerNodes := flag.String("ledger-nodes", "", "comma-separated list of ledgerline-raft node HTTP addresses (required)")
	accountingAddr := flag.String("accounting-addr", "http://127.0.0.1:8090", "base URL of the ledgerline-accounting-core service")
	flag.Parse()

	if strings.TrimSpace(*ledgerNodes) == "" {
		log.Fatal("gateway: -ledger-nodes is required (comma-separated list of ledgerline-raft HTTP addresses)")
	}
	nodes := strings.Split(*ledgerNodes, ",")
	for i := range nodes {
		nodes[i] = strings.TrimSpace(nodes[i])
	}

	ledger := ledgerclient.New(nodes)
	accounting := accountingclient.New(*accountingAddr)

	server := gatewayapi.NewServer(ledger, accounting)

	log.Printf("gateway: listening on %s (ledger nodes: %v, accounting: %s)", *httpAddr, nodes, *accountingAddr)
	if err := http.ListenAndServe(*httpAddr, server.Routes()); err != nil {
		log.Fatalf("gateway: server exited: %v", err)
	}
}
