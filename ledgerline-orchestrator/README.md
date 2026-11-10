# ledgerline-orchestrator

A demo shard-split controller for [Ledgerline](https://github.com/ledgerline), an
original, from-scratch distributed ledger platform (Raft consensus, event
sourcing, CQRS, double-entry bookkeeping). It does not run consensus or
store ledger state itself - it owns the **routing table** that tells the
rest of the system which `ledgerline-raft` cluster currently owns which
accounts, and it can perform a **shard split**: carve a sub-range of the
key space off onto a brand-new cluster.

This repository has no Go module dependency on `ledgerline-raft` (no
`replace` directive, no import of its packages). It talks to
`ledgerline-raft` clusters purely over the HTTP contract they expose,
using small local structs in `internal/ledgerclient` that mirror the wire
JSON. That keeps this repo independently buildable and testable without
the sibling repo checked out.

## What it does

- Hashes account IDs into a `uint32` key space with FNV-1a
  (`internal/shard.ShardFor`).
- Maintains an in-memory routing table: an ordered set of non-overlapping
  half-open `[start, end)` ranges, each pointing at the HTTP addresses of
  the `ledgerline-raft` cluster nodes that own it. Starts as a single
  shard covering the whole space, pointed at the cluster given on the
  command line.
- `GET /v1/routing` - dump the current routing table as JSON.
- `POST /v1/split` - given a `split_point` and the node addresses of a
  brand-new (already-running, empty) cluster, split whichever shard
  currently contains that point into two: `[old_start, split_point)` stays
  on the old cluster, `[split_point, old_end)` moves to the new one. See
  "How a split works" below for the exact sequence.
- `GET /healthz` - the orchestrator's own liveness check.

## How a split works

1. **Health-check** every address in `new_shard_ledger_nodes` via
   `GET /healthz`. If any node is unreachable or reports something other
   than `Leader`/`Follower`/`Candidate`, abort immediately - the routing
   table is not touched.
2. **Discover history.** Find the old cluster's leader (`GET /v1/leader`),
   then read its entire committed event history from
   `GET /v1/events/stream?from_index=0` to build the set of accounts that
   fall in the upper range (there's no "list accounts" endpoint, so this
   is reconstructed from `account_created` events and the
   `from_account`/`to_account` fields on `transfer_applied` events).
3. **Recreate accounts** on the new cluster's leader, in the order they
   first appeared in the old cluster's history. Each account's `unlimited`
   flag is re-fetched from `GET /v1/accounts/{id}` on the *old* cluster
   first, since the event stream doesn't carry that flag.
4. **Replay transfers.** Every `transfer_applied` event where *both*
   `from_account` and `to_account` fall in the migrated range is replayed
   against the new cluster with the same `dedup_id` (so retries are safe)
   via `POST /v1/transfers`.
5. **Atomically swap the routing table** - but only if every step above
   succeeded. On any failure, the split is aborted and the routing table
   is left exactly as it was; the (possibly partially-provisioned) new
   cluster is simply discarded by the caller. There is no partial/visible
   intermediate state.

`POST /v1/split` returns:

```json
{"status": "applied", "migrated_accounts": 3, "migrated_transfers": 1}
```

or on failure:

```json
{"status": "aborted", "reason": "health check failed for new node 127.0.0.1:8421: ..."}
```

## Why a *real* production split is much harder than this

This is a deliberately simplified demonstration of the "PU-split" pattern
(partition-unit split / live shard migration), useful for understanding
the shape of the problem, but it cuts real corners that a production
system could not:

- **No write-freeze / dual-write window.** Steps 2-4 above read the old
  cluster's history once and replay it. If writes land on the old cluster
  *for the migrating range* while that replay is in flight, they will not
  be reflected on the new cluster, and the split will silently "lose"
  them. A real system needs one of: a brief write-freeze on the migrating
  range while the tail of the log is replayed, a dual-write window where
  writes are applied to both clusters until they're confirmed caught up,
  or a change-data-capture pipeline that keeps tailing the old cluster's
  log until the new cluster is provably caught up, immediately before the
  atomic cutover. This orchestrator does none of that - it assumes the
  old cluster is quiescent (or close enough) for the migrating range while
  a split runs. Running writes against the migrating accounts during a
  split is not safe.
- **No cross-shard transfer support.** If a transfer's `from_account` and
  `to_account` land in different shards post-split, it cannot be
  cleanly replayed as a single same-cluster operation. This demo detects
  such transfers, logs a warning, and skips them entirely (they are
  reported in server logs but not reflected in `migrated_transfers`). A
  real system needs a cross-shard transaction protocol (e.g. a
  two-phase/sagas-style commit spanning both clusters, or restricting
  splits to boundaries that don't bisect active relationships).
  In this demo, pick a `split_point` so that any transfers you care about
  keeping have both legs on the same side of it.
- **One split at a time, synchronously.** `Orchestrator.Split` holds a
  single mutex for the whole request; a second `POST /v1/split` call will
  simply block until the first finishes. There's no queueing, no
  cancellation, and no resumability if the orchestrator process itself
  crashes mid-split (if it does, the old routing table is still active,
  since the table is only swapped at the very end - so a crash is safe,
  just not graceful).
- **No persistence for the routing table itself.** It lives in memory and
  starts over from the `-initial-ledger-nodes` flag on every restart. A
  real orchestrator would need to persist/replicate the routing table.
- **Backlog-read heuristic.** `GET /v1/events/stream` has no "end of
  backlog" marker - it's a long-lived connection that replays history and
  then keeps streaming live events forever. This orchestrator reads until
  an idle gap (`internal/controller.DefaultStreamIdleTimeout`, 750ms) with
  no new event elapses, then closes the connection and treats whatever it
  has read as the complete backlog. On a genuinely busy cluster this
  heuristic could (incorrectly) decide it has "caught up" while a burst of
  writes is merely paused, or could take longer than necessary waiting out
  a quiet gap that isn't really the end. A production system would compare
  against a known high-water-mark index instead of using a timing
  heuristic.

## How to run

```sh
go build -o ledgerline-orchestrator .
./ledgerline-orchestrator \
  -http-addr 127.0.0.1:8480 \
  -initial-ledger-nodes 127.0.0.1:8401,127.0.0.1:8402,127.0.0.1:8403
```

Flags:

- `-http-addr` - address for the orchestrator's own HTTP API (default
  `127.0.0.1:8480`).
- `-initial-ledger-nodes` - comma-separated HTTP addresses of the
  `ledgerline-raft` cluster nodes that own the starting single shard
  (required).

Check the routing table:

```sh
curl http://127.0.0.1:8480/v1/routing
```

Trigger a split, moving the upper half of the key space to a fresh
cluster:

```sh
curl -X POST http://127.0.0.1:8480/v1/split \
  -H 'Content-Type: application/json' \
  -d '{"split_point": 2147483648, "new_shard_ledger_nodes": ["127.0.0.1:8411","127.0.0.1:8412","127.0.0.1:8413"]}'
```

## Known simplifications

- No write-freeze/dual-write mechanism for the window between "start
  reading old cluster history" and "cut the routing table over" - see
  above. Writes against the migrating range during a split are not safe.
- Cross-shard transfers (one leg on each side of the split point) are
  detected, logged as a warning, and skipped - not migrated, not
  rejected up front.
- One split processed at a time, synchronously, behind a single mutex; no
  persistence of in-flight state across orchestrator restarts.
- The routing table is in-memory only; it is reinitialized from
  `-initial-ledger-nodes` on every process restart.
- "Caught up" on the old cluster's event history is decided by an
  idle-read-gap heuristic (see `internal/ledgerclient.Client.StreamEvents`
  and `internal/controller.DefaultStreamIdleTimeout`), not an authoritative
  high-water-mark comparison.
- Health checking the new cluster only confirms each node answers
  `GET /healthz` with a recognized Raft state (`Leader`/`Follower`/
  `Candidate`); it does not confirm the new cluster has elected a leader
  before beginning migration (that's verified separately, and causes an
  abort, when the orchestrator looks up the new cluster's leader before
  writing to it).

## Package layout

- `internal/shard` - `ShardFor` hashing and the routing `Table`
  (pure, no I/O; unit tested).
- `internal/migrate` - pure filtering logic that turns an ordered event
  slice plus an "is this account in range" predicate into the set of
  accounts/transfers to migrate (no I/O; unit tested with fixed fake event
  slices).
- `internal/ledgerclient` - local wire-contract structs and the HTTP
  client used to talk to `ledgerline-raft` nodes.
- `internal/controller` - wires the above together into the end-to-end
  split workflow (health check -> read history -> filter -> migrate ->
  swap routing table or abort).
- `internal/httpapi` - the orchestrator's own HTTP handlers.

## Testing

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .   # should print nothing
```

Unit tests cover the pure logic only (`shardFor` determinism/distribution,
routing-table split/lookup, and event-filtering-by-range) and do not
require a live `ledgerline-raft` cluster. End-to-end behavior against real
clusters has been verified manually; see the project's development notes
for the exact commands used.
