// Command ledgerline-fx runs the ledgerline-fx service: an independent FX
// rate service that ingests currency conversion rates and serves rate
// lookups and a conversion helper over HTTP.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ledgerline/ledgerline-fx/internal/fxsource"
	"github.com/ledgerline/ledgerline-fx/internal/fxstore"
	"github.com/ledgerline/ledgerline-fx/internal/httpapi"
)

func main() {
	httpAddr := flag.String("http-addr", "127.0.0.1:8590", "address to listen on")
	seedFile := flag.String("seed-file", "rates_seed.json", "path to the static seed rate JSON file")
	refresh := flag.Bool("refresh", false, "periodically refresh rates from a live public FX endpoint")
	refreshInterval := flag.Duration("refresh-interval", 15*time.Minute, "how often to refresh rates when -refresh is set")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	store, err := fxstore.NewStoreFromSeedFile(*seedFile, time.Now())
	if err != nil {
		logger.Fatalf("loading seed rates: %v", err)
	}
	logger.Printf("loaded seed rates from %s (as_of=%s)", *seedFile, store.AsOf().UTC().Format(time.RFC3339))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *refresh {
		source := fxsource.NewLive("")
		refresher := fxstore.NewRefresher(store, source, *refreshInterval, logger)
		logger.Printf("live refresh enabled: endpoint=%s interval=%s", source.Endpoint, *refreshInterval)
		go refresher.Run(ctx)
	}

	server := httpapi.NewServer(store, *refresh)
	httpServer := &http.Server{
		Addr:    *httpAddr,
		Handler: server,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	logger.Printf("ledgerline-fx listening on %s", *httpAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("server error: %v", err)
	}
}
