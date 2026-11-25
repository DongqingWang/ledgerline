# ledgerline-recon

The **external reconciliation** layer: it matches the ledger's recorded
journals against external authoritative records (card-scheme clearing files,
bank statements) and surfaces breaks. Reconciliation is the backstop that
catches anything the idempotent-retry path missed — money that moved
externally but was never recorded, or recorded but never moved.

## Break taxonomy

| Break | Meaning | Resolution |
|---|---|---|
| `missing_in_ledger` | external has it, ledger doesn't | **补记** (post adjustment) |
| `missing_in_external` | ledger has it, external doesn't | investigate / void |
| `amount_mismatch` | both have it, amounts differ | adjust difference |
| `duplicate` | same ref twice on one side | reverse the extra |

The matching engine (`internal/recon.Reconcile`) is pure and deterministic
(sorted by type, then ref), keyed on a shared reference.

## Adjustment

For a `missing_in_ledger` break, `Adjust` posts a balancing journal parking the
unexplained external movement in `recon-suspense` (append-only — history is
never mutated). The adjustment is idempotent on `recon-adj-<ref>`.

## API

| Endpoint | Purpose |
|---|---|
| `POST /v1/recon/ledger` | ingest ledger-side records `[{ref, amount_minor, currency}]` |
| `POST /v1/recon/external` | ingest external-side records (scheme file / bank stmt) |
| `POST /v1/recon/run` | match → breaks |
| `GET /v1/recon/breaks` | last run's breaks |
| `POST /v1/recon/adjust` | post an adjustment journal for a break |
| `GET /healthz` | liveness |

## Running

```sh
go build -o bin/recon ./cmd/recon
./bin/recon -http-addr 127.0.0.1:8980 -ledger-nodes 127.0.0.1:8001,...
```

## Known limitations (demo-grade)

- Both sides are ingested explicitly (production reads the ledger side from the
  Postgres projection / event stream and parses real scheme/bank files).
- Matching is by exact ref + amount; no fuzzy/amount-tolerant or many-to-one
  matching, no timing-window handling beyond flagging.
- Only `missing_in_ledger` auto-adjusts; other break types surface for manual
  investigation.
- In-memory record stores.