# ledgerline-reporting

A batch Trial Balance / reconciliation CLI for the [Ledgerline](https://github.com/ledgerline) distributed
ledger platform. This is the "fasreportsvc" equivalent in Ledgerline's
service topology: a standalone, offline reporting tool that reads the
durable Postgres projection written by `ledgerline-persistence-sync` and
produces the authoritative Trial Balance report, plus a global double-entry
reconciliation check suitable for cron/CI gating.

This repository does **not** depend on `ledgerline-raft` or
`ledgerline-persistence-sync` as Go modules. It only needs to know the
Postgres schema those services maintain; it talks to Postgres directly.

## What it does

1. Reads rows from `journal_entries` (optionally filtered by `posted_at`
   date range).
2. Groups entries by `gl_account` (a `NULL` `gl_account` is bucketed under
   `"UNSPECIFIED"`), summing `amount_minor` separately for `debit` and
   `credit` directions.
3. Emits a Trial Balance report — one row per GL account with
   `total_debit_minor`, `total_credit_minor`, and `net_minor`
   (`debit - credit`) — sorted by GL account, in `table`, `csv`, or `json`
   format.
4. Runs a global reconciliation check: sum of all debits across every GL
   account must equal the sum of all credits. Prints
   `RECONCILIATION: PASS` or `RECONCILIATION: FAIL (debits=X, credits=Y, diff=Z)`
   and **exits non-zero on failure**, so it can be used as an automated gate
   (cron job, CI step, alerting pipeline) rather than something a human has
   to remember to eyeball.

## Why a separate reporting pass over the durable projection?

Ledgerline's operational ledger path (Raft-replicated, event-sourced,
serving the CQRS write/read model) is optimized for low-latency transfer
posting and balance lookups. Finance-grade reporting — trial balances,
period-close reconciliation, audit exports — has very different
requirements: it needs to scan large historical ranges, tolerate being slow,
and be re-run idempotently without putting load on (or taking a dependency
on the availability of) the hot operational path.

By building this as a standalone batch job against the already-durable
Postgres projection:

- **No coupling to the operational service's uptime or latency budget.**
  This tool can run on a schedule (nightly close, intraday reconciliation)
  without contending for resources with transfer processing, and it keeps
  working even if the Raft cluster or the operational API is temporarily
  unavailable — the projection in Postgres is the durable system of record
  for everything already committed.
- **A clear, auditable boundary.** The trial balance aggregation logic
  (`internal/trialbalance`) is a pure function over plain row structs, with
  zero knowledge of Postgres, Raft, or the write path. That makes the
  accounting logic itself trivially unit-testable and reviewable in
  isolation from infrastructure concerns — exactly the kind of code finance
  and audit stakeholders want to be able to read and trust, including the
  reconciliation check.
- **Reporting changes don't risk the ledger.** Evolving report formats, GL
  account bucketing rules, or adding new reconciliation checks never
  touches the Raft/event-sourcing/CQRS write path — it's purely additive,
  read-only consumption of a projection that already exists for other
  purposes.

## Building

```sh
go build ./cmd/trialbalance
```

## Running

```sh
# Human-readable table to stdout, all data
./trialbalance -postgres-url 'postgres://localhost:5432/ledgerline?sslmode=disable' -format table

# CSV, written to a file
./trialbalance -format csv -output trial_balance.csv

# JSON to stdout
./trialbalance -format json

# Restrict to a date range (inclusive, by posted_at date)
./trialbalance -format table -from 2026-01-01 -to 2026-01-31
```

Flags:

| Flag             | Default                                                        | Description                                                             |
|------------------|-----------------------------------------------------------------|---------------------------------------------------------------------------|
| `-postgres-url`  | `postgres://localhost:5432/ledgerline?sslmode=disable`         | Postgres connection URL for the durable projection.                     |
| `-format`        | `table`                                                          | Output format: `table`, `csv`, or `json`.                               |
| `-from`          | (unset = no lower bound)                                        | Only include entries posted on/after this date (`YYYY-MM-DD`).          |
| `-to`            | (unset = no upper bound)                                        | Only include entries posted on/before this date (`YYYY-MM-DD`, inclusive of the whole day). |
| `-output`        | (unset = stdout)                                                 | File path to write the report to.                                       |

The `RECONCILIATION: PASS`/`FAIL` line is always printed to stdout (even
when `-output` redirects the report body to a file), and the process exit
code reflects reconciliation status: `0` on pass, `1` on fail (or on any
fetch/connection error), `2` on bad flag usage.

### Example output

```
GL ACCOUNT   TOTAL DEBIT  TOTAL CREDIT  NET
-------------------------------------------
1000              100000        100000    0
4000                2750          2750    0
UNSPECIFIED          600           600    0
RECONCILIATION: PASS
```

## Testing

The aggregation and reconciliation logic in `internal/trialbalance` is a
pure function over in-memory structs and is fully covered by unit tests
that require **no live database**:

```sh
go test ./...
```

Test fixtures cover: multiple GL accounts, the `NULL` → `UNSPECIFIED`
bucket, an intentionally unbalanced fixture (verifying `Reconcile` reports
the correct diff), and the empty-input edge case.

The only code that talks to Postgres lives in `internal/store`; it is a
thin, directly-reviewable fetch layer with no business logic, verified by
running the CLI against a real database (see the repo's commit history /
CI for example output against live data).

## Known simplifications

- **Single currency assumption in aggregation.** The report sums
  `amount_minor` without grouping by `currency`. If `journal_entries` ever
  holds genuinely multi-currency data for the same `gl_account`, those
  amounts would be summed together without conversion. The existing schema
  allows this; a production version should either assert single-currency
  usage or add currency as a grouping key.
- **No `cost_center` breakdown.** The brief only requires grouping by
  `gl_account`; `cost_center` is read from the schema notionally but not
  surfaced in this report. A natural extension would be a second grouping
  dimension or a `-cost-center` filter flag.
- **Date filtering is calendar-day granularity.** `-from`/`-to` take
  `YYYY-MM-DD` and are translated to UTC-less local calendar-day boundaries
  compared against `posted_at` (a `timestamptz`); there's no way to filter
  by a specific time-of-day.
- **No pagination/streaming for very large tables.** `FetchJournalRows`
  loads the full filtered result set into memory. For Ledgerline's current
  data volumes this is fine for a batch reporting job; a very large ledger
  might want a streaming/cursor-based aggregation instead.
- **No read-only transaction/snapshot isolation guarantee.** The query runs
  as a single `SELECT` under the connection's default isolation level. For
  a period-close report with concurrent writers, a production system would
  likely want an explicit `REPEATABLE READ` (or higher) snapshot to
  guarantee a consistent point-in-time view across the whole scan.
- **Reconciliation is global, not per-GL-account.** The brief asks for a
  single global debit/credit check across all GL accounts combined, which
  is what's implemented. In practice each GL account in the current sample
  data also balances individually, but the tool does not assert or report
  that stronger per-account invariant.
