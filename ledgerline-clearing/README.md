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

## Event-stream driven

Clearing **tails the ledger event stream** and automatically extracts
acquiring captures from `journal_applied` events (the merchant-payable credit
leg carries the merchant in `cost_center`). No explicit ingest is needed —
`POST /v1/clearing/captures` remains only for manual/backfill feeding. Ingest
is idempotent by ref, so stream replay never double-counts.

## Known limitations (demo-grade)

- Single `merchant-payable` / `clearing-suspense` account pair; multi-currency
  needs per-currency accounts.
- In-memory pending captures and seen-ref set; a clearing cycle is triggered
  manually, not on a schedule, and the consumed watermark is not persisted
  (a restart re-consumes from index 0, deduped by ref).
- No scheme-file-driven clearing (that is reconciliation's input, P3).