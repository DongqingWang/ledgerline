// Package report renders a []trialbalance.GLBalance as table, CSV, or JSON
// text. Like internal/trialbalance, this is pure formatting logic over
// already-computed data — no I/O beyond writing to the given io.Writer.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/ledgerline/ledgerline-reporting/internal/trialbalance"
)

// Format identifies the requested output rendering.
type Format string

const (
	FormatTable Format = "table"
	FormatCSV   Format = "csv"
	FormatJSON  Format = "json"
)

// Write renders balances in the given format to w. An unrecognized format
// is an error so misuse fails loudly instead of silently defaulting.
func Write(w io.Writer, format Format, balances []trialbalance.GLBalance) error {
	switch format {
	case FormatTable:
		return writeTable(w, balances)
	case FormatCSV:
		return writeCSV(w, balances)
	case FormatJSON:
		return writeJSON(w, balances)
	default:
		return fmt.Errorf("unknown format %q (want table, csv, or json)", format)
	}
}

func writeTable(w io.Writer, balances []trialbalance.GLBalance) error {
	headers := []string{"GL ACCOUNT", "TOTAL DEBIT", "TOTAL CREDIT", "NET"}

	rows := make([][]string, 0, len(balances))
	for _, b := range balances {
		rows = append(rows, []string{
			b.GLAccount,
			strconv.FormatInt(b.TotalDebitMinor, 10),
			strconv.FormatInt(b.TotalCreditMinor, 10),
			strconv.FormatInt(b.NetMinor, 10),
		})
	}

	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	printRow := func(cells []string) {
		// Column 0 (GL account) is left-aligned; numeric columns are right-aligned.
		fmt.Fprintf(w, "%-*s  %*s  %*s  %*s\n",
			widths[0], cells[0],
			widths[1], cells[1],
			widths[2], cells[2],
			widths[3], cells[3],
		)
	}

	printRow(headers)

	sepLen := widths[0] + widths[1] + widths[2] + widths[3] + 6
	for i := 0; i < sepLen; i++ {
		if _, err := io.WriteString(w, "-"); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}

	for _, row := range rows {
		printRow(row)
	}

	if len(rows) == 0 {
		fmt.Fprintln(w, "(no journal entries in range)")
	}

	return nil
}

func writeCSV(w io.Writer, balances []trialbalance.GLBalance) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"gl_account", "total_debit_minor", "total_credit_minor", "net_minor"}); err != nil {
		return err
	}
	for _, b := range balances {
		record := []string{
			b.GLAccount,
			strconv.FormatInt(b.TotalDebitMinor, 10),
			strconv.FormatInt(b.TotalCreditMinor, 10),
			strconv.FormatInt(b.NetMinor, 10),
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func writeJSON(w io.Writer, balances []trialbalance.GLBalance) error {
	if balances == nil {
		balances = []trialbalance.GLBalance{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(balances)
}
