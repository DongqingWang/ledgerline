# ledgerline-acquiring

The **card-acquiring domain**: processes a payment (auth → capture) against a
simulated card scheme and records the resulting balanced double-entry journal
in the `ledgerline-raft` ledger. The ledger remains the single source of fund
truth; this service owns the payment-processing lifecycle.

## PaymentIntent lifecycle (P1)

```
pending -> authorized -> captured
        \-> failed     \-> failed
pending/authorized -> voided
captured -> voided   (refund: reverse the journal)
```

`clearing` and `settled` are later phases (P2/P3).

## The acquiring journal

For a payment of `amount` with `fee`:

```
Dr scheme-receivable      amount        (asset, unlimited)
Cr merchant-payable       amount - fee  (liability)
Cr acquiring-fee-revenue  fee           (revenue)
```

Per-leg GL (1100 receivable / 2200 payable / 4000 fee). The three accounts are
auto-created if absent. Zero fee → two legs.

## Dual-write protection (the core design)

Capturing on the scheme and writing the ledger cannot be atomic. So:

1. **Durable intent FIRST** — the PaymentIntent is recorded before any scheme
   action, so "money in flight" is never lost.
2. **Scheme auth → capture** (simulated, deterministic decline injection).
3. **Idempotent journal write, retried** — `dedup_id` makes retries safe; a
   transient ledger failure is retried, never double-recorded.
4. **Resumable** — if capture succeeded but the journal write failed, the
   intent stays `authorized` (durably known); re-driving the same `dedup_id`
   resumes and records exactly once.

This is the same principle as the fat-ledger settlement: never lose the
knowledge that an external action happened; propagate to the ledger
idempotently; reconcile as the backstop.

## API

| Endpoint | Purpose |
|---|---|
| `POST /v1/payment-intents` | create `{dedup_id, merchant_id, amount_minor, fee_minor, currency}` |
| `GET /v1/payment-intents/{id}` | intent status |
| `GET /v1/payment-intents` | list |
| `POST /v1/payment-intents/{id}/void` | cancel; if captured, reverses the journal (refund) |
| `GET /healthz` | liveness |

## Running

```sh
go build -o bin/acquiring ./cmd/acquiring
./bin/acquiring -http-addr 127.0.0.1:8780 \
  -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
  -fail-amounts 13
```

## Known limitations (demo-grade)

- **Simulated scheme only** — no real card-network integration.
- **In-memory intent store** — production persists it so a crash never loses
  the durable intent (the resumable design assumes a durable store).
- **No clearing/netting (P2), no scheme-funding/external reconciliation (P3)** —
  see the extension design.
- Fee is a simple amount; no real interchange/scheme fee modeling.