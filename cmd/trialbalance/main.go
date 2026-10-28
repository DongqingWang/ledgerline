// Command trialbalance reads the durable Postgres journal_entries
// projection (written by ledgerline-persistence-sync) and produces the
// authoritative Trial Balance report for finance/reporting purposes,
// followed by a global double-entry reconciliation check.
//
// It exits non-zero if the reconciliation fails, so it can be wired into a
// cron job or CI pipeline as a reconciliation gate.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ledgerline/ledgerline-reporting/internal/report"
	"github.com/ledgerline/ledgerline-reporting/internal/store"
	"github.com/ledgerline/ledgerline-reporting/internal/trialbalance"
)

const dateLayout = "2006-01-02"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trialbalance", flag.ContinueOnError)
	fs.SetOutput(stderr)

	postgresURL := fs.String("postgres-url", "postgres://localhost:5432/ledgerline?sslmode=disable", "Postgres connection URL for the ledgerline durable projection")
	format := fs.String("format", "table", "Output format: table, csv, or json")
	fromStr := fs.String("from", "", "Only include entries posted on/after this date (YYYY-MM-DD); omit for no lower bound")
	toStr := fs.String("to", "", "Only include entries posted on/before this date (YYYY-MM-DD); omit for no upper bound")
	output := fs.String("output", "", "File path to write the report to; defaults to stdout")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	var from, to time.Time
	var err error
	if *fromStr != "" {
		from, err = time.Parse(dateLayout, *fromStr)
		if err != nil {
			fmt.Fprintf(stderr, "invalid -from date %q: %v\n", *fromStr, err)
			return 2
		}
	}
	if *toStr != "" {
		to, err = time.Parse(dateLayout, *toStr)
		if err != nil {
			fmt.Fprintf(stderr, "invalid -to date %q: %v\n", *toStr, err)
			return 2
		}
		// Treat -to as inclusive of the whole day.
		to = to.Add(24*time.Hour - time.Nanosecond)
	}

	var out io.Writer = stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			fmt.Fprintf(stderr, "creating output file %q: %v\n", *output, err)
			return 1
		}
		defer f.Close()
		out = f
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := store.Open(ctx, *postgresURL)
	if err != nil {
		fmt.Fprintf(stderr, "connecting to postgres: %v\n", err)
		return 1
	}
	defer db.Close()

	rows, err := store.FetchJournalRows(ctx, db, from, to)
	if err != nil {
		fmt.Fprintf(stderr, "fetching journal entries: %v\n", err)
		return 1
	}

	tbRows := make([]trialbalance.JournalRow, 0, len(rows))
	for _, r := range rows {
		tbRows = append(tbRows, trialbalance.JournalRow{
			GLAccount:   r.GLAccount,
			Direction:   r.Direction,
			AmountMinor: r.AmountMinor,
		})
	}

	balances := trialbalance.BuildTrialBalance(tbRows)

	if err := report.Write(out, report.Format(*format), balances); err != nil {
		fmt.Fprintf(stderr, "writing report: %v\n", err)
		return 1
	}

	ok, debits, credits := trialbalance.Reconcile(balances)
	if ok {
		fmt.Fprintln(stdout, "RECONCILIATION: PASS")
		return 0
	}

	fmt.Fprintf(stdout, "RECONCILIATION: FAIL (debits=%d, credits=%d, diff=%d)\n", debits, credits, debits-credits)
	return 1
}
