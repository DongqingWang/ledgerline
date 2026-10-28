# ledgerline-balance-reader

A fast CQRS read-path balance service for Ledgerline. It is the "query side"
counterpart to `ledgerline-raft`'s "command side": instead of answering
balance lookups by consulting the ledger engine's own read path on every
request, it tails the engine's committed event stream once into an
in-memory cache and then answers reads straight out of that cache.

## Why this exists (CQRS framing)

`ledgerline-raft` is the source of truth for the ledger: every transfer goes
through Raft consensus and is only durable once a majority of nodes have
committed it. Its own `GET /v1/accounts/{id}` endpoint is correct, but
"correct" isn't the same as "cheap" — on a busy cluster, routing every
balance read through the same node(s) that are also handling writes adds
load and per-request latency to the write path's neighborhood, and reads
don't need linearizability with the very latest write the way a transfer
does.

This service follows a CQRS (Command Query Responsibility Segregation)
split: writes still go to `ledgerline-raft`, but reads are served from a
separate, purpose-built read model that is kept up to date by subscribing to
the same committed event stream every node already exposes
(`GET /v1/events/stream`). Because the read model lives entirely in this
process's memory, a balance lookup here is a single mutex-guarded map read —
no RPC, no consensus, no disk I/O. The trade-off is staleness: this service
is only as fresh as the last event it has applied, typically milliseconds
behind the leader, in exchange for reads that are orders of magnitude
cheaper than going through the engine itself. That trade-off is the entire
point of a CQRS read replica.

## How it works

1. On startup, given a comma-separated list of candidate `ledgerline-raft`
   HTTP addresses (`-ledger-nodes`), it tries each node in turn until one
   accepts a streaming connection to `GET /v1/events/stream?from_index=0`.
   Any node — leader or follower — can serve this endpoint, since it's just
   replaying the committed log.
2. Each `LedgerEvent` line of the newline-delimited JSON stream is applied
   to an in-memory cache:
   - `account_created` inserts a zero-balance entry for the account.
   - `transfer_applied` debits `from_account` and credits `to_account` by
     `amount_minor`.
3. The last-applied `raft_index` is tracked in memory. If the stream
   connection drops (e.g. the node goes down or network hiccups), the
   service reconnects — trying the next candidate node — resuming from
   `last_processed_index + 1` via `from_index`, so no events are replayed
   twice and none are skipped. Reconnect attempts back off exponentially
   (500ms up to 10s) between full passes over the candidate list.
4. Its own HTTP API serves balance queries straight from the cache:
   - `GET /v1/balances/{id}` → `{"account_id":"...","currency":"...","balance_minor":2500}`,
     or 404 if the account hasn't been seen yet.
   - `GET /v1/balances` → a JSON array of every cached balance (for
     debugging/demos).
   - `GET /healthz` → `{"status":"ok","last_raft_index":N,"cached_accounts":M}`.

## Running it

```sh
go build -o /tmp/ledgerline-balance-reader .
/tmp/ledgerline-balance-reader \
  -http-addr 127.0.0.1:8280 \
  -ledger-nodes 127.0.0.1:8201,127.0.0.1:8202,127.0.0.1:8203
```

Flags:

- `-http-addr` — this service's own HTTP listen address (default
  `127.0.0.1:8280`).
- `-ledger-nodes` — comma-separated list of candidate `ledgerline-raft` HTTP
  addresses to stream events from (required).

Then query it:

```sh
curl http://127.0.0.1:8280/v1/balances/alice
curl http://127.0.0.1:8280/v1/balances
curl http://127.0.0.1:8280/healthz
```

## Design notes

- This repo has **no Go module dependency on `ledgerline-raft`**. The event
  JSON shape (`LedgerEvent` in `events.go`) is a small, hand-maintained
  local mirror of the wire contract, and all communication with the ledger
  engine happens over plain HTTP + `encoding/json`. This keeps the two
  repos independently buildable and deployable.
- The stream is decoded incrementally with `json.Decoder.Decode` in a loop,
  not `json.Unmarshal` on the whole response body, since the body is an
  unbounded newline-delimited JSON stream that stays open for live events.

## Known simplifications

- **In-memory-only cache, no persistence.** The cache and the
  last-processed `raft_index` live only in process memory. A restart of
  this service currently re-streams from `from_index=0` and rebuilds the
  entire cache from scratch. This was a deliberate simplification for this
  iteration — the brief calls out on-disk persistence/resume-from-last-index
  as a nice-to-have, not a requirement. For a cluster with a very long
  event history, a production version of this service would periodically
  snapshot `(last_processed_index, balances)` to disk (or a local embedded
  store) and resume from that snapshot plus `from_index=snapshot_index+1`
  on startup, rather than always replaying from genesis.
- **Duplicate/out-of-order event handling is index-based and coarse.** Any
  incoming event whose `raft_index` is less than or equal to the last
  applied index is dropped wholesale. This is sufficient to make a
  reconnect-triggered replay of already-seen indices safe (it won't
  double-apply a transfer), but it does mean that if a node ever delivered
  events wildly out of order within a single connection (which the
  `ledgerline-raft` contract does not do — the log is replayed in index
  order), only the "future" ones relative to the running high-water mark
  would be applied.
- **No authentication/TLS** on either the connection to `ledgerline-raft`
  or this service's own API — both are assumed to run inside a trusted
  network for now, consistent with the rest of the early-stage Ledgerline
  stack.
- **Accounts created on the fly for transfers naming an unseen account.**
  Because this service always starts streaming from `from_index=0`, in
  practice every account's `account_created` event is seen before any
  transfer referencing it. As a defensive fallback (e.g. if a future
  version started mid-stream), a `transfer_applied` naming an account not
  yet in the cache creates a zero-balance entry with an empty currency
  on the fly rather than crashing or dropping the event.

## Testing

`cache_test.go` unit-tests the replay logic directly (no network required):
feeding sequences of `LedgerEvent` values into `BalanceCache.Apply` and
asserting on the resulting balances, including a transfer between two
accounts and a duplicate/out-of-order `raft_index` edge case.

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
```
