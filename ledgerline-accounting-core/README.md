# ledgerline-accounting-core

The double-entry bookkeeping and GL-dimension rule engine for
[Ledgerline](https://github.com/ledgerline) — a pure business-logic
library that every transfer must pass through *before* it's submitted to
`ledgerline-raft`, because nothing should post to the ledger without first
being correctly classified.

> Original implementation of a well-known pattern (rule-driven GL
> dimension derivation in double-entry accounting) — not derived from any
> proprietary source.

## What it does

`ComposeJournal(ctx TransferContext) (Journal, error)`:

1. Resolves a `TransferContext` (who, how much, what business category, what
   country) against an ordered `RuleSet` — first match wins, same pattern as
   a conditional rule engine. Always keep a final `category: "*", country:
   "*"` catch-all rule so every transfer resolves to *something* (even if
   that's an "unclassified" GL bucket flagged for manual review) rather than
   hard-failing.
2. Builds a balanced `Journal`: a debit line and a credit line, each tagged
   with the GL account/cost center/tax code the matched rule assigned. If a
   `FeeMinor` is set, the credit side splits into a net-payout line and a
   separate platform-fee line — demonstrating that "balanced" doesn't mean
   "exactly two lines," just that debits and credits sum to the same total.
3. **Validates** the result (`Validate(j Journal) error`) before returning it
   — debits must equal credits, full stop. This function is also exported
   standalone so other services (e.g. a reconciliation job) can re-check a
   journal they read back from storage.

## Rule file

See `accounting/rules.yaml` for a worked example with a US-specific
`seller_payout` rule, an international fallback, a `refund` rule, and a
catch-all. Rules are matched in file order; `"*"` (or an omitted field)
matches anything.

## Running the REST wrapper

```sh
go run ./cmd/accountingsvc -rules accounting/rules.yaml -http-addr 127.0.0.1:8090

curl -X POST http://127.0.0.1:8090/v1/journal/compose -d '{
  "from_account": "platform", "to_account": "seller-1",
  "amount_minor": 10000, "fee_minor": 300, "currency": "USD",
  "category": "seller_payout", "country": "US"
}'
```

## Testing

```sh
go test ./... -v
```

`accounting/journal_test.go` is table-driven and is the highest-value test
suite in this repo — it covers exact-match rules, catch-all fallthrough, the
fee-split invariant (net + fee == original amount), rejecting unbalanced or
single-line journals, and a round-trip through the real `rules.yaml` file.
