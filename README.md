# Ledgerline

**Ledgerline** is an original, open-source distributed ledger platform —
a Raft-replicated, event-sourced core engine plus the gateway, read-path,
accounting, FX, reporting, sharding, and AI-access layers built around it.

It's inspired by the well-known, publicly documented architectural patterns
behind production ledger systems (Raft consensus, event sourcing, CQRS,
double-entry bookkeeping) — **it is not derived from, and contains no code
from, any proprietary system.** Every repo here was built from scratch for
this project, verified against a real, running stack at each step (not just
unit-tested in isolation), and several real bugs found during that
verification are documented below rather than swept under the rug.

## Layout

This is a monorepo: each service below is a subdirectory with its own
`go.mod` (or, for the admin console, its own `package.json`) and its own
README — independently buildable on its own, but versioned together here
with its full original commit history preserved (each service started life
as its own repo; see `git log -- <dir>` to see that history).

| Directory | Role |
|---|---|
| [`ledgerline-raft`](./ledgerline-raft) | The core: a Raft-replicated, event-sourced ledger engine. Idempotent `AssertTransfer` and atomic multi-leg `AssertJournal`, reversals, two-phase pending transfers, per-currency double-entry invariants, durable event replay, optional API-key auth with tenant isolation, and a committed-event stream for CQRS fan-out. |
| [`ledgerline-accounting-core`](./ledgerline-accounting-core) | Double-entry journal composer + GL-dimension rule engine. Classifies a transfer's business context into GL account/cost center before it's submitted to the ledger. |
| [`ledgerline-gateway`](./ledgerline-gateway) | Client-facing REST API. Leader discovery/retry into the raft cluster, composes journals via accounting-core and posts them as one atomic journal; FX cross-currency payouts via position accounts; reversal passthrough; optional tenant auth. |
| [`ledgerline-balance-reader`](./ledgerline-balance-reader) | Fast CQRS read-path: tails the engine's event stream into an in-memory cache instead of querying the engine on every read. Enforces raft-index contiguity and resyncs from scratch if a gap can't be healed. |
| [`ledgerline-persistence-sync`](./ledgerline-persistence-sync) | Projects committed events into durable Postgres tables — the queryable system of record for reporting/reconciliation. Per-leg journal rows with their own GL codes, pending-hold projection, index-continuity enforcement. |
| [`ledgerline-fx`](./ledgerline-fx) | Independent FX rate service: static seed rates + optional live refresh, cross-rate derivation, conversion. |
| [`ledgerline-orchestrator`](./ledgerline-orchestrator) | Demo shard-split controller: routing table + a (simplified) split operation that stands up a new cluster, replays history into it (transfers, journals, posted holds), and atomically swaps routing. |
| [`ledgerline-reporting`](./ledgerline-reporting) | Trial Balance / reconciliation batch CLI reading the Postgres projection. Reconciles debits = credits **per currency** and exits non-zero if any currency fails. |
| [`ledgerline-ai-gateway`](./ledgerline-ai-gateway) | An MCP server exposing read-only ledger-query tools to AI agents, with field-level redaction, a least-privilege DB role, and audit logging. |
| [`ledgerline-admin-console`](./ledgerline-admin-console) | A small local demo web UI: balances, a transfer form, FX rates. |
| *(repo root)* | This README, `docker-compose.yml`, and the demo scripts tying everything together. |

## Architecture

```mermaid
flowchart TB
    subgraph write_path["Write Path"]
        GW["ledgerline-gateway<br/>(leader-follow retry)"]
        ACC["ledgerline-accounting-core<br/>(journal composition)"]
        RAFT["ledgerline-raft<br/>3-node cluster<br/>(event-sourced, Raft-replicated)"]
        GW -->|compose journal| ACC
        GW -->|AssertJournal| RAFT
    end

    subgraph read_paths["Read Paths (CQRS fan-out from the committed event stream)"]
        BR["ledgerline-balance-reader<br/>(in-memory cache)"]
        PS["ledgerline-persistence-sync<br/>(durable Postgres projection)"]
        RAFT -->|event stream| BR
        RAFT -->|event stream| PS
    end

    subgraph downstream_postgres["Downstream of Postgres"]
        RPT["ledgerline-reporting<br/>(trial balance + reconciliation)"]
        AI["ledgerline-ai-gateway<br/>(MCP, redacted, least-privilege)"]
        PG[(Postgres)]
        PS --> PG
        PG --> RPT
        PG --> AI
    end

    FX["ledgerline-fx<br/>(independent)"]
    ORCH["ledgerline-orchestrator<br/>(shard routing + split)"]
    UI["ledgerline-admin-console<br/>(browser)"]

    UI -->|transfers| GW
    UI -->|balances| BR
    UI -->|rates| FX
    GW -->|convert (cross-currency)| FX
    ORCH -.->|admin ops on| RAFT
```

**Data flow for a transfer**: a client calls `ledgerline-gateway`, which asks
`ledgerline-accounting-core` to classify the transfer into a balanced,
GL-tagged journal, then submits it to `ledgerline-raft` as **one atomic
`AssertJournal`** — every leg (including a fee leg, each with its own GL
code) commits in a single raft log entry or none do. A cross-currency
request (`to_currency`) additionally asks `ledgerline-fx` for the converted
amount and posts a 4-leg journal through the platform's fx-position
accounts, with each currency balancing independently. The raft leader
commits it to its replicated log, applies it to the state machine, and emits
exactly one event per log entry on the committed-event stream.
`ledgerline-balance-reader` and `ledgerline-persistence-sync` both
tail that same stream independently — one for fast in-memory reads, one for
a durable, queryable Postgres projection — and both refuse to apply events
across a raft-index gap. `ledgerline-reporting` and
`ledgerline-ai-gateway` both read from that Postgres projection, never from
the engine directly, so analytics and AI-agent access are strictly read-path
concerns that can never compete with or block the operational write path.

## Running it

**Docker:**
```sh
./scripts/demo.sh
```
Brings up the whole stack, runs an end-to-end demo (accounts, a fee-split
transfer, idempotency check, fast-path vs. engine-path reads, trial balance,
an MCP tool call), then tells you what to try next (the admin console is at
http://localhost:5180). Fully verified live, including every container
reporting `healthy`, a real transfer applying through gateway → raft →
balance-reader/persistence-sync, the reporting CLI's reconciliation passing
inside its own container, and a genuine MCP protocol round-trip against the
dockerized `ai-gateway` showing `cost_center` correctly redacted.

**Without Docker:**
```sh
./scripts/local-demo.sh
```
Builds and runs every service as a plain OS process against a local
Postgres (`createdb ledgerline` first). Same end-to-end flow, also fully
verified live — this is the path used throughout most of this project's
development, before Docker Desktop finished its (surprisingly slow,
interactive-feeling) first-run initialization.

## Cross-cutting observations (and real bugs found along the way)

Mirroring the style of the architecture research this project was inspired
by: here's what actually went wrong while building it, not just what works.

- **A JSON-tag bug silently zeroed every field.** `accounting.TransferContext`
  (the accounting-core REST wrapper's request type) had no `json:"..."`
  struct tags, so `encoding/json` fell back to matching the Go field names
  verbatim — meaning every snake_case wire field (`amount_minor`,
  `from_account`, ...) silently decoded to its zero value. Every request
  failed with a confusing "amount must be positive" error. This slipped
  through the repo's own unit tests (which call `ComposeJournal` directly as
  a Go function, never through JSON) and was only caught when
  `ledgerline-gateway`'s live integration test actually exercised the real
  HTTP endpoint. **Lesson**: unit-testing business logic in isolation doesn't
  catch wire-format bugs at the boundary — you have to actually call the API.
- **No CORS headers, anywhere.** All the Go HTTP services were built and
  verified via `curl`, which doesn't enforce CORS. The first time the
  browser-based `ledgerline-admin-console` tried to call them, every request
  failed with a CORS preflight error. Fixed with a small permissive
  (wildcard) CORS middleware in `gateway`, `balance-reader`, and `fx` —
  documented in each as a local-demo-only simplification, not something
  you'd want on a public deployment.
- **A single, per-transfer GL code, not per-leg — FIXED.** `ledgerline-raft`'s
  original `AssertTransfer` took one `gl_account`/`cost_center` pair for the
  whole transfer (a simple two-party primitive), while
  `ledgerline-accounting-core` assigns *different* GL codes to the debit and
  credit legs. The old gateway tagged a raft-level transfer with the
  **credit leg's** GL code, so a fee-split payout's debit leg showed up under
  the credit leg's revenue GL code (`4000`) instead of its own payout code
  (`2100`). The books balanced, but GL attribution was wrong. Fixed by
  adding an atomic multi-leg `AssertJournal` primitive to the engine: each
  leg carries its own GL dimensions, the gateway posts the composed journal
  verbatim as one journal, and `ledgerline-persistence-sync` writes one
  `journal_entries` row per leg (uniqueness key `(transfer_dedup_id,
  line_index)`).
- **A fee split was two sequential transfers, not one atomic journal —
  FIXED.** The old gateway mapped a fee-split payout to a "net" transfer
  plus a "fee" transfer. Concrete counterexample: balance 90, amount 100,
  fee 20 → net(80) applied, fee(20) rejected for insufficient funds → 80
  already moved, the API returned an error, and nothing reversed or even
  guaranteed the retry that would eventually converge. Fixed by the same
  `AssertJournal` primitive: the engine validates every leg's aggregate net
  debit against available balance *before* mutating anything, so the whole
  payout is all-or-nothing. Regression-tested at three levels (FSM unit
  test, gateway stub test, live e2e: balance 90 / amount 100 / fee 20 → 422
  with zero balance movement).
- **The event stream could silently lose events — FIXED.** The old
  in-memory-only bus dropped events for slow subscribers (non-blocking send
  on a full channel) and lost all history on restart/snapshot compaction,
  while consumers advanced their watermark regardless — a projection could
  silently miss a transfer forever. Fixed on both sides: the engine now
  appends every event to a durable `events.ndjson` (replayed on startup,
  partial trailing line truncated) and *kicks* a subscriber whose buffer is
  full, forcing it to reconnect from its watermark; exactly one event is
  emitted per committed log entry (including `command_rejected` /
  `command_duplicate`), so raft-index contiguity is a valid gap detector.
  Both consumers now refuse to apply across a gap, and balance-reader
  falls back to a full resync if a gap can't be healed. Verified live: a
  restarted node replays its full history, and the stream's indices are
  strictly contiguous.
- **`reversal_of` linked to an ID that doesn't exist in the projection.**
  Found by the end-to-end run after adding reversals: the engine stamped
  the original entry's raft-derived `JournalID` (`jrn-12`) into
  `reversal_of`, but `journal_entries` has no journal-id column — its only
  entry identifier is `transfer_dedup_id`, so the link could never be
  joined. Fixed to carry the original entry's **DedupID**, the stable
  business idempotency key. **Lesson**: an event field's semantics have to
  be checked against the schema that consumes it, not just against the
  producer's own unit tests (both sides' tests had encoded the same wrong
  assumption).
- **`sync_state` is a single global counter with no notion of "which
  cluster."** `ledgerline-persistence-sync` resumes from
  `sync_state.last_raft_index` on restart — which is exactly right for
  resuming against the *same* cluster, but means pointing it at a **fresh**
  raft cluster while reusing an **old** Postgres database makes it resume
  from the old cluster's index and silently skip every event from the new
  one. Found while writing `scripts/local-demo.sh`, which re-ran against the
  same long-lived local Postgres database used throughout this project's
  development. Both demo scripts now reset state first (`docker compose down
  -v` / dropping the projection tables) specifically to avoid this.
- **A bash subshell gotcha leaked two zombie processes.** An early version of
  `scripts/local-demo.sh` started a couple of services as `(cd dir && binary
  args) &`, then tried to kill them via the subshell's PID on exit. Bash
  doesn't forward signals from a backgrounded subshell to its foreground
  child automatically, so `kill` hit the subshell (which was likely already
  done) while the actual binary kept running and held its port open,
  breaking the *next* run. Fixed with `(cd dir && exec binary args) &`,
  which replaces the subshell's process image with the binary instead of
  forking a child — so the tracked PID *is* the binary's PID. Confirmed fixed
  by checking `lsof` for leaked listeners after a run.

- **A `localhost` healthcheck against a hostname-bound listener.** The raft
  nodes bind their HTTP API to their own compose hostname (`raft-n1:8080`,
  not `0.0.0.0:8080`) on purpose — that exact string is what gets embedded in
  `-peers` and echoed back to callers as `leader_hint`, so it has to be
  something other containers can actually dial. The first version of
  `docker-compose.yml`'s healthcheck probed `http://localhost:8080/healthz`
  from *inside* that same container — which doesn't work, because binding to
  a specific hostname/interface in Go's `net.Listen` does not also bind the
  loopback interface. All three raft containers came up `unhealthy` (even
  though the application itself was working perfectly — raft leader election
  and replication succeeded in the logs the whole time), which cascaded into
  every dependent service refusing to start. Fixed by pointing each node's
  healthcheck at its own hostname instead of `localhost`. Found by actually
  running `docker compose up` and reading the failure, not by inspection.

## Known simplifications

The correctness core (atomic journals, per-currency balancing, deterministic
expiry, durable replay, gap detection) is real and tested. What remains
demo-grade, deliberately:

- **Auth is a static JSON API-key table** (`-auth-config` on the engine and
  gateway), not a real identity provider / key-management service. Tenant
  ownership is stamped server-side and enforced at the HTTP boundary
  (403 on cross-tenant writes, 404 on cross-tenant reads, admin-only event
  stream); the enforcement points are where a real IdP would plug in.
  Auth is off by default for local demos.
- **`sync_state` is a single global counter** with no notion of "which
  cluster" (see the bug story above) — one persistence-sync instance tracks
  one cluster.
- **The orchestrator's split does not migrate in-flight holds**: only
  posted holds (which moved balances) are replayed, as transfers;
  cross-shard journals/transfers are reported and skipped, not migrated.
- **Cross-currency payouts don't support fees yet** (`fee_minor > 0` with
  `to_currency` is a 422), and FX rates are fetched at request time with no
  rate-lock/quote expiry.
- **CORS is a permissive wildcard** in gateway/balance-reader/fx — local
  demo only.

## License

MIT — see each repo's own `LICENSE` file.
