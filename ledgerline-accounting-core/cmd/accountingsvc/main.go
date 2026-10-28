// Command accountingsvc is a thin REST wrapper around the accounting rule
// engine, so ledgerline-gateway (or anyone else) can compose a journal over
// HTTP instead of importing the Go package directly.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"

	"github.com/ledgerline/ledgerline-accounting-core/accounting"
)

func main() {
	addr := flag.String("http-addr", "127.0.0.1:8090", "address to listen on")
	rulesPath := flag.String("rules", "accounting/rules.yaml", "path to the GL-dimension rule set YAML file")
	flag.Parse()

	rs, err := accounting.LoadRuleSet(*rulesPath)
	if err != nil {
		log.Fatalf("load rule set: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/journal/compose", func(w http.ResponseWriter, r *http.Request) {
		var ctx accounting.TransferContext
		if err := json.NewDecoder(r.Body).Decode(&ctx); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		journal, err := rs.ComposeJournal(ctx)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(journal)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Printf("accountingsvc listening on %s (rules=%s)", *addr, *rulesPath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("http server: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
