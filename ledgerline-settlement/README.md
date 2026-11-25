# ledgerline-settlement

The async bank-settlement driver for a `ledgerline-raft` cluster.

## Fat-ledger design: the lifecycle lives in the FSM

Unlike a thin orchestrator that keeps settlement state in its own (volatile)
store, here the **settlement state machine lives inside the ledger FSM**, so
in-flight state is **raft-durable** and survives crashes:

```
pending -> processing -> posted
        \-> voided      \-> voided
pending/processing -> expired   (deterministic sweep)
```

This service holds **no authoritative state** — it submits commands to the
engine and drives a simulated rail. If it crashes, nothing is lost: the
in-flight settlements are recoverable from the ledger (queryable via the
engine's `GET /v1/settlements/{id}`).

> Core invariant: **ledger atomicity ≠ settlement atomicity.** Banks give no
> cross-rail 2PC, so rail outcomes drive *compensating* ledger transitions
> (void the reservation / reverse the posted entry), never a rollback.

## The two flows (asymmetric by design)

| | withdrawal (**optimistic post**) | deposit (**late-tolerant**) |
|---|---|---|
| pending | intent only, no balance | intent only, no balance |
| processing | **posted immediately** (debit user) | still nothing (not spendable) |
| posted (settled) | no change (already posted) | credited (now spendable) |
| voided (failed) | **reverse** the post (credit back) | nothing to undo |
| expired | only *pending* expires; processing stays posted | pending/processing expire; a **late settled re-credits** |

**Only optimistically move balance in the direction that reduces the user's
available funds.** A withdrawal posts at submission (no expirable reservation →
no double-spend race); a deposit is not spendable until confirmed, and a rail
confirmation that arrives *after* expiry is still honored (the money really
arrived, so it is credited).

## How it drives the engine

| Step | Engine command |
|---|---|
| create | `POST /v1/settlements` (FSM records `pending`) |
| submit to rail | `POST /v1/settlements/{id}/mark-processing` (withdrawal **posts here**) |
| rail settled | `settle-outcome` `{outcome:"settled"}` → `posted` (deposit credits; late-after-expiry honored) |
| rail failed | `settle-outcome` `{outcome:"failed"}` → `voided` (withdrawal reverses) |
| delayed return | `POST /v1/settlements/{id}/return` → reverses the posted entry |
| rail timeout | deterministic FSM sweep → `expired` (withdrawal processing stays posted) |

Every command is idempotent (engine dedup), so re-delivery and crash-recovery
re-driving are safe.

## Simulated rail

`internal/rail` reproduces async delay + deterministic failure injection
(`-rail-delay`, `-fail-amounts`). A delayed return is simulated via the
`/return` endpoint.

## API (this service)

| Endpoint | Purpose |
|---|---|
| `POST /v1/deposits` / `POST /v1/withdrawals` | create `{dedup_id, account_id, amount_minor, currency}` |
| `GET /v1/settlements/{id}` | read state (proxied from the engine FSM) |
| `POST /v1/settlements/{id}/return` | simulate a delayed rail return → reversal |
| `GET /healthz` | liveness |

## Running

```sh
go build -o bin/settlement .
./bin/settlement -http-addr 127.0.0.1:8680 \
  -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
  -rail-delay 1s -fail-amounts 13
```

A `bank-clearing` unlimited asset account is auto-created as the external-bank
nexus.

## Known limitations (demo-grade)

- **Spent-funds deposit returns are blocked, not absorbed.** Reversing a
  deposit credit debits the user; if already spent, the engine refuses to
  overdraw (422) — a credit-loss break a real system resolves with a
  loss/receivable account. The e2e asserts this protection.
- **Simulated rail only**; single-currency nexus; no bank-statement
  reconciliation, risk-based availability grading, or dunning.
- The settlement *driver* is stateless, but the simulated rail's in-memory
  timers are not persisted (a real rail is an external durable system).