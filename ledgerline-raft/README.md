# ledgerline-raft

The core of [Ledgerline](https://github.com/ledgerline) — a Raft-replicated,
event-sourced ledger engine. This is the piece every other Ledgerline service
builds on: an account/balance state machine, replicated via
[`hashicorp/raft`](https://github.com/hashicorp/raft), where every balance
change is first durably committed to a consensus log and only then applied —
which is what makes the committed log itself the audit trail, not a side
effect bolted onto one.

> **Provenance**: this is an original implementation inspired by well-known,
> publicly documented distributed-systems patterns (Raft consensus, event
> sourcing, CQRS, double-entry bookkeeping) — it is not derived from, and
> contains no code from, any proprietary system. See the
> [top-level README](../README.md) for the full architecture writeup.

## What it does

- **Idempotent, conditional transfers.** `AssertTransfer(dedup_id, from, to, amount, currency)`
  moves funds between two accounts exactly once, even if the client retries
  after a timeout — retries with the same `dedup_id` return the original
  result instead of double-applying.
- **Double-entry by construction.** A transfer always debits one account and
  credits another by the exact same amount — there's no way to specify two
  independent legs that don't balance.
- **Clearing/external-funding accounts.** An account created with
  `"unlimited": true` is allowed to go negative — the standard double-entry
  way money enters or leaves the ledger from outside the system (e.g. an
  `external-deposit` account), rather than a special-cased "mint" backdoor.
- **A committed-event stream** (`GET /v1/events/stream`) that other services
  (`ledgerline-balance-reader`, `ledgerline-persistence-sync`) tail instead of
  querying the engine synchronously for every change — the CQRS split that
  keeps the write path and the read paths independently scalable.
- **Leader-aware HTTP API.** Non-leader nodes reject writes with a `409` and
  a `leader_hint`; see `pkg/ledgerclient` for the client-side retry loop.

## Why plain HTTP+JSON instead of gRPC

The original design sketch called for gRPC, but this repo deliberately uses
plain JSON-over-HTTP instead: it's exactly as easy to express the same
request/response contracts, and it means anyone cloning this repo can talk to
it with nothing but `curl` — no protoc toolchain or generated stubs required.
If you want a gRPC transport, the FSM/raft core here doesn't change at all;
only `internal/httpapi` would need a sibling `internal/grpcapi`.

## Running a 3-node cluster locally

```sh
go build -o ledgerd ./cmd/ledgerd

PEERS="n1=127.0.0.1:7001=127.0.0.1:8001,n2=127.0.0.1:7002=127.0.0.1:8002,n3=127.0.0.1:7003=127.0.0.1:8003"

./ledgerd -node-id n1 -raft-addr 127.0.0.1:7001 -http-addr 127.0.0.1:8001 -data-dir ./data/n1 -peers "$PEERS" -bootstrap=true &
./ledgerd -node-id n2 -raft-addr 127.0.0.1:7002 -http-addr 127.0.0.1:8002 -data-dir ./data/n2 -peers "$PEERS" &
./ledgerd -node-id n3 -raft-addr 127.0.0.1:7003 -http-addr 127.0.0.1:8003 -data-dir ./data/n3 -peers "$PEERS" &
```

Then:

```sh
# who's the leader?
curl http://127.0.0.1:8001/v1/leader

# create an external-funding account and two real accounts
curl -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"external-deposit","currency":"USD","unlimited":true}'
curl -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"alice","currency":"USD"}'
curl -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"bob","currency":"USD"}'

# fund alice, then transfer alice -> bob
curl -X POST http://127.0.0.1:8001/v1/transfers -d '{"dedup_id":"seed-alice","from_account":"external-deposit","to_account":"alice","amount_minor":10000,"currency":"USD"}'
curl -X POST http://127.0.0.1:8001/v1/transfers -d '{"dedup_id":"txn-1","from_account":"alice","to_account":"bob","amount_minor":2500,"currency":"USD","gl_account":"4000"}'

# resubmitting the same dedup_id is a no-op ("duplicate"), not a double-spend
curl -X POST http://127.0.0.1:8001/v1/transfers -d '{"dedup_id":"txn-1","from_account":"alice","to_account":"bob","amount_minor":2500,"currency":"USD"}'

# read from a follower — confirms replication
curl http://127.0.0.1:8003/v1/accounts/bob

# tail the committed event log from the beginning
curl "http://127.0.0.1:8001/v1/events/stream?from_index=0"
```

## API contract

See the "Shared Contracts" section of the Ledgerline architecture doc. In
short:

| Method & path | Leader-only? | Purpose |
|---|---|---|
| `POST /v1/accounts` | yes | Create a zero-balance account (optionally `unlimited`) |
| `GET /v1/accounts/{id}` | no* | Read current balance (*not linearizable on a follower*) |
| `POST /v1/transfers` | yes | `AssertTransfer` — idempotent on `dedup_id` |
| `GET /v1/leader` | no | Resolve the current raft leader's HTTP address |
| `GET /v1/events/stream?from_index=N` | no | NDJSON stream of committed events from index N |

## Known simplifications (documented, not accidental)

- **In-memory event history.** `internal/fsm/eventbus.go` keeps the full
  event history in RAM to support replay-from-any-index. Fine for a demo;
  a production system would window this or replay from the durable store
  (`ledgerline-persistence-sync`) instead.
- **Follower reads are not linearizable.** `GET /v1/accounts/{id}` reads
  whatever this node has applied so far. For a strongly-consistent read,
  route through the leader.
- **Static peer configuration.** Cluster membership is fixed at startup via
  `-peers`; dynamic membership changes (`AddVoter`/`RemoveServer`) aren't
  wired up here — see `ledgerline-orchestrator` for how a shard-split would
  stand up a *new* cluster rather than reconfiguring this one live.

## Testing

```sh
go test ./...
```

`internal/fsm/fsm_test.go` is the most important test file in this repo — it
covers the double-entry invariant, dedup-based idempotency, insufficient
funds, unlimited/clearing accounts, and snapshot/restore round-tripping.
