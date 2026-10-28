# ledgerline-gateway

The client-facing REST API gateway for a `ledgerline-raft` cluster.

## What it does

`ledgerline-raft` nodes form a Raft cluster: only the current leader accepts
writes, and the leader can change at any time (elections, restarts,
partitions). Clients shouldn't have to track that. `ledgerline-gateway` sits
in front of a static list of cluster node addresses, discovers the current
leader itself, and transparently retries against the right node — so callers
can just POST to the gateway and get on with their day.

It also sits in front of `ledgerline-accounting-core`, a service that knows
how to turn a business-level transfer ("pay seller-1 $100, platform keeps a
$3 fee") into a balanced double-entry journal (debit/credit lines with GL
accounts and cost centers). The gateway composes that journal first, then
translates it into the one or two simple two-party transfers that
`ledgerline-raft`'s engine actually understands, and submits those.

Both upstream services are treated as plain HTTP dependencies. This repo does
**not** import `ledgerline-raft` or `ledgerline-accounting-core` as Go
modules — it only defines small local structs mirroring their JSON wire
contracts and talks to them over `encoding/json` + `net/http`. Every
Ledgerline repo is independently buildable without its siblings checked out.

## API

### `POST /v1/transfers`

Request:

```json
{
  "dedup_id": "txn-1",
  "from_account": "platform",
  "to_account": "seller-1",
  "amount_minor": 10000,
  "fee_minor": 300,
  "currency": "USD",
  "category": "seller_payout",
  "country": "US"
}
```

`fee_minor` may be omitted (or `0`) for a fee-free transfer.

Response (`200`):

```json
{
  "status": "applied",
  "journal": { "lines": [ ... ] },
  "ledger_results": [ { "status": "applied", "journal_id": "jrn-7", "raft_index": 7 } ]
}
```

What happens under the hood:

1. The gateway calls `ledgerline-accounting-core`'s
   `POST /v1/journal/compose` with `{from_account, to_account, amount_minor,
   fee_minor, currency, category, country}`. If that call fails, the gateway
   returns `422 Unprocessable Entity` with the accounting error.
2. It reads the returned journal's lines: the (single) `debit` line is the
   primary leg; the `credit` line whose `account_id == to_account` is the net
   leg; and, if `fee_minor > 0`, the `credit` line whose
   `account_id == "platform-fee-revenue"` is the fee leg.
3. It submits transfer(s) to the ledger cluster via its leader-follow client:
   - **No fee** (`fee_minor == 0`): one `POST /v1/transfers` call using
     `dedup_id` as-is and the debit leg's `gl_account`/`cost_center`.
   - **With fee** (`fee_minor > 0`): **two** calls, because
     `ledgerline-raft`'s engine only understands simple two-party transfers,
     so the 3-line journal is split:
     - `dedup_id + "-net"`: `from_account -> to_account` for
       `amount_minor - fee_minor`, using the net-credit leg's
       `gl_account`/`cost_center`.
     - `dedup_id + "-fee"`: `from_account -> "platform-fee-revenue"` for
       `fee_minor`, using the fee leg's `gl_account`/`cost_center`.

   **`platform-fee-revenue` must exist as a real account in the ledger
   cluster** (created the normal way, via `POST /v1/accounts` against the
   cluster) for fee transfers to succeed — the gateway does not create it
   for you.
4. It returns the composed journal plus every ledger-engine result it got
   back. `status` is `"duplicate"` if any underlying ledger call reports a
   duplicate `dedup_id` (i.e. this exact transfer was already applied), so
   retrying a `POST /v1/transfers` with the same `dedup_id` is safe.

### `GET /v1/accounts/{id}`

Proxies to the ledger cluster's `GET /v1/accounts/{id}` (tries the
last-known leader first, falls back to any reachable node — reads work on
followers too, though they may be slightly stale).

### `GET /healthz`

Returns `{"state": "ok"}` once the HTTP server is up. This does not check
upstream health; see "Known simplifications" below.

## Leader discovery and retry

`ledgerline-raft` nodes reject writes with `409` and
`{"error":{"code":"not_leader","leader_hint":"<addr>"}}` when they aren't the
leader. The gateway's internal `ledgerclient.Client`:

1. Tries the last address it knows was the leader, if any; otherwise starts
   at the front of the configured `-ledger-nodes` list.
2. On a `not_leader` response with a non-empty `leader_hint`, retries against
   that hint directly.
3. Caps total attempts per logical request at **5** (write or read), after
   which it gives up and returns an error — this bounds retry storms during
   a prolonged leader election rather than looping forever.
4. Remembers the last node that returned a 2xx as the new "last-known
   leader" for next time, but will fall back to round-robining the full
   configured node list if a remembered leader turns out to be stale or
   unreachable.

This means a caller only needs to give the gateway the cluster's full static
node list once at startup; it never needs to know (or care) which node is
currently leading.

## Running it

```sh
go build -o bin/gateway ./cmd/gateway
./bin/gateway \
  -http-addr 127.0.0.1:8080 \
  -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 \
  -accounting-addr http://127.0.0.1:8090
```

Flags:

| Flag               | Default               | Description                                              |
|--------------------|------------------------|-----------------------------------------------------------|
| `-http-addr`        | `127.0.0.1:8080`       | Address the gateway itself listens on.                    |
| `-ledger-nodes`     | *(required)*           | Comma-separated `ledgerline-raft` node HTTP addresses.     |
| `-accounting-addr`  | `http://127.0.0.1:8090`| Base URL of the `ledgerline-accounting-core` REST service. |

## Development

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .   # should print nothing
```

Unit tests cover the journal-to-ledger-call translation logic
(`internal/gatewayapi`, with stubbed accounting/ledger clients) and the
leader-follow retry behavior (`internal/ledgerclient`, with `httptest`
servers simulating `not_leader` redirects, a never-resolving redirect chain
that must give up after 5 attempts, and the last-known-leader optimization).

## Known simplifications

- `GET /healthz` reports gateway process liveness only; it does not probe
  the ledger cluster or accounting service.
- The gateway does not cache or retry the `ledgerline-accounting-core` call
  itself — a transient accounting-service failure surfaces to the caller as
  a `422` rather than being retried internally. (The ledger-side calls *are*
  retried, since that's where leader elections create real, expected
  transient failures.)
- If the accounting journal succeeds but the *second* ledger call (the fee
  leg) fails after the first (net) one already applied, the gateway returns
  an error without attempting to compensate/reverse the first leg. The first
  leg is itself safely retryable (by `dedup_id + "-net"`) if the caller
  retries the whole gateway request, but true all-or-nothing atomicity
  across both ledger calls is out of scope for this service.
- No authentication/authorization is implemented; this is a bare
  demonstration gateway.
