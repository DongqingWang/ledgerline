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
	if err := http.ListenAndServe(*httpAddr, withCORS(server.Routes())); err != nil {
		log.Fatalf("gateway: server exited: %v", err)
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
