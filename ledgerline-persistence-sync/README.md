# ledgerline-persistence-sync

A durable, queryable system-of-record projector for Ledgerline. It tails a
`ledgerline-raft` cluster's committed-event stream over HTTP and writes
durable rows into Postgres, so that reporting, analytics, and reconciliation
workloads can run plain SQL against Postgres instead of depending on the
ledger engine's own in-memory, replay-only event log.

This is the Ledgerline analogue of a classic "projector" / "read-model
builder" in an event-sourced / CQRS architecture: the ledger engine
(`ledgerline-raft`) is the single source of truth for consensus and
commitment, but it is optimized for low-latency transaction processing, not
for ad-hoc querying. Its event log lives in memory and is only meant to be
replayed from the start to rebuild state — it is not a convenient query
target, it has no SQL interface, and asking it to serve heavy analytical
queries would compete with the operational write path for resources on the
same process. `ledgerline-persistence-sync` decouples those concerns: it is
a separate, independently-scalable process that consumes the same committed
event stream any other reader would, and incrementally builds a normal
Postgres schema that downstream services (`ledgerline-reporting`,
`ledgerline-ai-gateway`, ad-hoc reconciliation scripts, etc.) can query with
ordinary SQL, with no load placed on the ledger engine's hot path beyond
serving the stream.

## Design

- **No Go dependency on `ledgerline-raft`.** This service talks to the
  ledger engine purely over HTTP + `encoding/json`. `internal/events`
  contains a small hand-maintained mirror of the `LedgerEvent` wire shape
  documented by `ledgerline-raft`'s `/v1/events/stream` endpoint. This keeps
  the two repositories independently buildable and deployable.
- **Pure translation logic is separated from database I/O.** `internal/translate`
  turns `LedgerEvent` values into the row shapes / balance deltas this
  service writes, with no network or DB calls, so it is fully unit-testable.
  `internal/store` is the only package that talks to Postgres (via
  `github.com/jackc/pgx/v5`), and `internal/streamclient` is the only
  package that talks to `ledgerline-raft`.
- **Idempotent, crash-safe processing.** Every event is applied inside a
  single Postgres transaction that: (a) writes the resulting rows, (b)
  updates `account_balances` guarded by `raft_index`, and (c) advances
  `sync_state.last_raft_index` — so a crash mid-event can never partially
  apply it, and reconnecting/replaying the stream (which always resumes from
  `sync_state.last_raft_index + 1`, re-read from Postgres on every
  (re)connect, never from an in-memory counter) cannot double-apply it
  either.

## Schema

This service owns and migrates (via simple `CREATE TABLE IF NOT EXISTS`
statements run on startup) three tables. **Table and column names are a
cross-repo contract** — `ledgerline-reporting` and `ledgerline-ai-gateway`
read from these same tables, so they must not be renamed:

```sql
CREATE TABLE IF NOT EXISTS journal_entries (
  id BIGSERIAL PRIMARY KEY,
  transfer_dedup_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  direction TEXT NOT NULL CHECK (direction IN ('debit','credit')),
  amount_minor BIGINT NOT NULL,
  currency TEXT NOT NULL,
  gl_account TEXT,
  cost_center TEXT,
  posted_at TIMESTAMPTZ NOT NULL,
  raft_index BIGINT NOT NULL,
  UNIQUE (transfer_dedup_id, account_id, direction)
);

CREATE TABLE IF NOT EXISTS account_balances (
  account_id TEXT PRIMARY KEY,
  currency TEXT NOT NULL,
  balance_minor BIGINT NOT NULL DEFAULT 0,
  last_raft_index BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sync_state (
  id INT PRIMARY KEY DEFAULT 1,
  last_raft_index BIGINT NOT NULL DEFAULT 0
);
```

Every `transfer_applied` event produces exactly two `journal_entries` rows
(one `debit` for `from_account`, one `credit` for `to_account`), so
`SUM(amount_minor) FILTER (WHERE direction='debit')` always equals
`SUM(amount_minor) FILTER (WHERE direction='credit')` across the whole
table — the basic double-entry reconciliation invariant.

### Currency

The `transfer_applied` wire event does not itself carry a `currency` field
(currency is a property of the accounts involved, not of the transfer
record). `journal_entries.currency` is therefore derived, inside the same
transaction that processes the event, by looking up the transacting
account's currency from this service's own `account_balances` table (that
row was seeded by the account's earlier `account_created` event, and the
ledger engine's FSM guarantees the from/to/request currencies all match, so
either account's row has the right value). If neither account has a seeded
`account_balances` row yet — which should not happen in practice, since
`account_created` always precedes transfers on an account — this falls back
to an empty string and logs a warning rather than failing the whole sync.

## Running

```sh
go build -o persistence-sync ./cmd/persistence-sync
./persistence-sync \
  -postgres-url "postgres://localhost:5432/ledgerline?sslmode=disable" \
  -ledger-nodes "127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003" \
  -http-addr "127.0.0.1:8380"
```

Flags:

| Flag | Default | Description |
|---|---|---|
| `-postgres-url` | `postgres://localhost:5432/ledgerline?sslmode=disable` | Postgres connection string for the projection schema |
| `-ledger-nodes` | *(required)* | Comma-separated candidate `ledgerline-raft` HTTP addresses; any reachable node (leader or follower) can serve the stream |
| `-http-addr` | `127.0.0.1:8380` | Address for this service's own health endpoint |

`GET /healthz` returns `{"status":"ok","last_raft_index":N}` (read fresh
from Postgres on every request).

On startup the service connects to Postgres, runs the migration, reads
`sync_state.last_raft_index`, then connects to the ledger event stream
starting at `last_raft_index + 1`, trying each candidate address in order
until one accepts the connection. If the stream connection drops, it
reconnects with exponential backoff (capped at 30s), re-reading the resume
point from Postgres each time — so a process restart resumes correctly too.

## Testing

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .
```

`internal/translate` has unit tests covering event-to-row translation
(including optional-field-absent cases), balance-delta computation (with a
debits-net-to-zero invariant check), and the replay idempotency guard
(`ShouldApplyBalanceUpdate`) — all pure logic, no database required.
Everything that touches Postgres or the live event stream
(`internal/store`, `internal/streamclient`) was instead verified live
against a real 3-node `ledgerline-raft` cluster and a real local Postgres
instance; see the project's verification notes for the exact commands and
observed output.

## Known simplifications

- **Migration is "CREATE TABLE IF NOT EXISTS" only.** There is no schema
  versioning, no down-migrations, and no handling of incompatible schema
  drift introduced by a sibling service. This is adequate for the current
  scope (one owner of this schema) but would need a real migration tool
  (e.g. `golang-migrate`) if the schema needs to evolve with history.
- **Currency fallback is best-effort.** If a transfer references an account
  that was never seen via `account_created` (e.g. a synthetic/system
  account created out-of-band, or a stream started mid-history without ever
  replaying that account's creation), `journal_entries.currency` is written
  as an empty string with a logged warning rather than blocking the sync.
  This keeps the projector live but means such rows need manual
  backfill/reconciliation.
- **No parallelism.** Events are applied strictly in stream order, one at a
  time, each in its own transaction. This is the simplest way to guarantee
  correctness (especially the `raft_index` idempotency guard) but caps
  throughput at one event's round-trip latency to Postgres; a higher-volume
  deployment would want to batch multiple events per transaction.
- **No dead-letter / poison-event handling.** If translating or applying an
  event ever returns an error (e.g. a malformed event from a future,
  incompatible version of the wire contract), the stream loop treats it the
  same as a dropped connection and retries with backoff — which will retry
  the same bad event forever rather than skipping it. A production version
  would want a way to quarantine a poison event and move on.
- **Single ledger "instance" assumed.** The schema and sync_state row
  (`id=1`) assume one logical ledger cluster is being projected into this
  database. Projecting multiple independent ledger clusters into one
  Postgres database would need per-cluster sync_state rows and a
  cluster-id column throughout.
- **At-least-once delivery, relying on uniqueness constraints for
  idempotency**, not a stronger exactly-once guarantee. This is standard for
  event-sourced projectors and is sufficient here because every write is
  either naturally idempotent (`ON CONFLICT ... DO NOTHING` /
  `DO UPDATE ... WHERE` guarded by `raft_index`) or safe to repeat.
