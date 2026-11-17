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
posts it to `ledgerline-raft` as **one atomic multi-leg `AssertJournal`**:
every leg — including the fee leg, each with its own GL code — commits in a
single raft log entry or none do. Cross-currency requests (`to_currency`)
are converted via `ledgerline-fx` and posted as a 4-leg journal through the
platform's fx-position accounts.

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

`fee_minor` may be omitted (or `0`) for a fee-free transfer. Setting
`to_currency` (different from `currency`) requests an FX conversion: the
recipient is credited in `to_currency` at the rate returned by
`ledgerline-fx`. Fees are not supported on cross-currency transfers (422).

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
2. **Same currency**: it maps the composed journal's lines verbatim onto one
   `POST /v1/journals` (`AssertJournal`) request — same `dedup_id`, every
   leg keeping its own `gl_account`/`cost_center`. The engine validates
   per-currency debit=credit and every account's available balance before
   mutating anything, so a fee-split payout is all-or-nothing. (The old
   two-transfer `-net`/`-fee` mapping was not atomic and is gone.)

   **`platform-fee-revenue` must exist as a real account in the ledger
   cluster** (created the normal way, via `POST /v1/accounts` against the
   cluster) for fee transfers to succeed — the gateway does not create it
   for you.
3. **Cross-currency** (`to_currency` set): it calls `ledgerline-fx`'s
   `POST /v1/convert`, ensures the platform's `fx-position-<CCY>` accounts
   exist (unlimited asset accounts, auto-created; tenant-prefixed
   `<tenant>:fx-position-<CCY>` when auth is enabled), and posts one 4-leg
   journal:

   ```
   debit  from            amount     (currency)
   credit fx-position-A   amount     (currency)
   debit  fx-position-B   converted  (to_currency)
   credit to              converted  (to_currency)
   ```

   Each currency balances independently — the engine's per-currency
   journal invariant. The response `journal` shows these actual posted legs.
4. It returns the posted journal plus the single ledger-engine result.
   `status` is `"duplicate"` if the engine reports a duplicate `dedup_id`
   (this exact journal was already applied), so retrying a
   `POST /v1/transfers` with the same `dedup_id` is safe. Reusing a
   `dedup_id` with *different* parameters is a `409 Conflict`.

### `POST /v1/reversals`

Request: `{"dedup_id": "rev-1", "original_dedup_id": "txn-1"}`. Passed
through to the engine's `POST /v1/reversals`: the exact opposite legs are
posted as a new journal linked via `reversal_of` (the original entry's
`dedup_id`); the original is never mutated. An entry can be reversed at
most once (`409 already_reversed` on a second attempt).

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
| `-fx-addr`          | *(empty = FX disabled)*| Base URL of `ledgerline-fx`; required for `to_currency` transfers. |
| `-auth-config`      | *(empty = auth off)*   | JSON API-key table for client auth (same format as the engine's). |
| `-ledger-api-key`   | *(empty)*              | Engine **admin** service key, when the engine runs with `-auth-config`. The gateway forwards the authenticated tenant via `X-Tenant-ID`. |

## Authentication and tenant isolation

With `-auth-config`, every request (except `/healthz`) needs an API key
(`X-API-Key` or `Authorization: Bearer`). The key table maps keys to
tenants; admin keys may act on behalf of a tenant via `X-Tenant-ID`. The
gateway then calls the engine with its own admin service key
(`-ledger-api-key`) and the authenticated tenant's `X-Tenant-ID` — tenant
identity is never taken from a request body, and the engine stamps
ownership server-side. FX position accounts are tenant-prefixed so
positions never commingle across tenants.

## Development

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .   # should print nothing
```

Unit tests cover the journal mapping (per-leg GL preserved, fee split as
one atomic journal, insufficient-funds surfacing with no partial follow-up),
the FX 4-leg mapping (position-account creation/reuse, tenant prefixing,
fee rejection), and the leader-follow retry behavior
(`internal/ledgerclient`, with `httptest` servers simulating `not_leader`
redirects, a never-resolving redirect chain that must give up after 5
attempts, the last-known-leader optimization, and auth-header forwarding).

## Known simplifications

- `GET /healthz` reports gateway process liveness only; it does not probe
  the ledger cluster or accounting service.
- The gateway does not cache or retry the `ledgerline-accounting-core` call
  itself — a transient accounting-service failure surfaces to the caller as
  a `422` rather than being retried internally. (The ledger-side calls *are*
  retried, since that's where leader elections create real, expected
  transient failures.)
- Cross-currency transfers don't support fees (`fee_minor > 0` with
  `to_currency` → 422), and the FX rate is fetched at request time — no
  rate-lock / quote expiry.
- Auth is a demo-grade static key table, not a real identity provider; the
  middleware + server-side tenant stamping are the enforcement points a
  real IdP would plug into.
