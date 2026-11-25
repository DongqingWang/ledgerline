# ledgerline-clearing

The **netting / clearing** layer between acquiring (gross per-transaction) and
settlement (net fund movement). It nets many captured transactions per
(merchant, currency) into one obligation, reducing rail traffic and cost, and
records a clearing journal that moves the net obligation to a clearing-suspense
account ready for settlement payout.

## What clearing does

```
gross captures (per transaction)
   → net per (merchant, currency)
   → clearing journal: Dr merchant-payable / Cr clearing-suspense
   → settlement pays from clearing-suspense
```

Clearing is the **information-flow** layer: it computes who nets owes whom.
No money moves here — that is settlement's job.

## Netting engine

`internal/clearing.Net` is pure and deterministic (sorted output): groups
captures by (merchant, currency), sums net amounts (captures positive,
refunds negative), keeps zero-net positions (fully-refunded merchants) so the
gross payable can be zeroed out.

## API

| Endpoint | Purpose |
|---|---|
| `POST /v1/clearing/captures` | ingest a gross capture `{merchant_id, currency, net_minor, ref}` |
| `POST /v1/clearing/run` | net pending captures → batch + clearing journal(s) |
| `GET /v1/clearing/batches` | recorded batches |
| `GET /healthz` | liveness |

The clearing journal is idempotent per batch+currency (`dedup_id`), so a
re-run never double-posts.

## Running

```sh
go build -o bin/clearing ./cmd/clearing
./bin/clearing -http-addr 127.0.0.1:8880 -ledger-nodes 127.0.0.1:8001,...
```

## Known limitations (demo-grade)

- Captures are ingested explicitly (production would consume the ledger event
  stream or acquiring's captured events automatically).
- Single `merchant-payable` / `clearing-suspense` account pair; multi-currency
  needs per-currency accounts.
- In-memory pending captures; a clearing cycle is triggered manually, not on a
  schedule.
- No scheme-file-driven clearing (that is reconciliation's input, P3).