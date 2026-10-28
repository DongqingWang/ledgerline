// Command persistence-sync tails a ledgerline-raft cluster's committed
// event stream and projects it into a durable, queryable Postgres schema
// (journal_entries, account_balances, sync_state) for reporting,
// reconciliation, and analytics workloads.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ledgerline/ledgerline-persistence-sync/internal/events"
	"github.com/ledgerline/ledgerline-persistence-sync/internal/store"
	"github.com/ledgerline/ledgerline-persistence-sync/internal/streamclient"
)

func main() {
	postgresURL := flag.String("postgres-url", "postgres://localhost:5432/ledgerline?sslmode=disable",
		"Postgres connection string for the durable projection schema")
	ledgerNodes := flag.String("ledger-nodes", "",
		"comma-separated candidate ledgerline-raft HTTP addresses, e.g. 127.0.0.1:8001,127.0.0.1:8002")
	httpAddr := flag.String("http-addr", "127.0.0.1:8380", "address for this service's own HTTP health endpoint")
	flag.Parse()

	candidates := splitAndTrim(*ledgerNodes)
	if len(candidates) == 0 {
		log.Fatal("persistence-sync: -ledger-nodes must list at least one ledgerline-raft HTTP address")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.New(ctx, *postgresURL)
	if err != nil {
		log.Fatalf("persistence-sync: connect to postgres: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("persistence-sync: migrate: %v", err)
	}

	startIdx, err := db.LastRaftIndex(ctx)
	if err != nil {
		log.Fatalf("persistence-sync: read initial sync_state: %v", err)
	}
	log.Printf("persistence-sync: starting; last_raft_index=%d, ledger-nodes=%v, http-addr=%s", startIdx, candidates, *httpAddr)

	srv := newHealthzServer(*httpAddr, db)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("persistence-sync: healthz server error: %v", err)
		}
	}()

	client := streamclient.New(candidates)
	handle := func(ctx context.Context, ev events.LedgerEvent) error {
		switch ev.Kind {
		case events.KindAccountCreated:
			if err := db.ApplyAccountCreated(ctx, ev); err != nil {
				return err
			}
			log.Printf("persistence-sync: applied account_created raft_index=%d account_id=%s currency=%s", ev.RaftIndex, ev.AccountID, ev.Currency)
		case events.KindTransferApplied:
			if err := db.ApplyTransferApplied(ctx, ev); err != nil {
				return err
			}
			log.Printf("persistence-sync: applied transfer_applied raft_index=%d dedup_id=%s from=%s to=%s amount_minor=%d",
				ev.RaftIndex, ev.DedupID, ev.FromAccount, ev.ToAccount, ev.AmountMinor)
		default:
			log.Printf("persistence-sync: skipping unknown event kind %q at raft_index=%d", ev.Kind, ev.RaftIndex)
		}
		return nil
	}

	err = streamclient.RunWithReconnect(ctx, client, db.LastRaftIndex, handle)
	if err != nil && ctx.Err() == nil {
		log.Fatalf("persistence-sync: stream loop exited: %v", err)
	}

	log.Println("persistence-sync: shutting down")
	_ = srv.Close()
}

func newHealthzServer(addr string, db *store.Store) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		idx, err := db.LastRaftIndex(r.Context())
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "error", "error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":          "ok",
			"last_raft_index": idx,
		})
	})
	return &http.Server{Addr: addr, Handler: mux}
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
