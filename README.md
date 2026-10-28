# ledgerline-ai-gateway

An MCP (Model Context Protocol) server that lets an AI agent query
Ledgerline's ledger data — account balances and double-entry journal
entries — over a narrow, read-only, content-aware boundary, distinct from
the normal service-to-service trust the rest of Ledgerline runs on.

This repo is one piece of the larger Ledgerline platform (Raft consensus,
event sourcing, CQRS, double-entry bookkeeping). It does not write ledger
state; it only reads it, through three tightly-scoped MCP tools, each with
an explicit redaction/guardrail story.

## Why an AI-agent gateway is a different trust boundary

Normal service-to-service access in Ledgerline is identity-based: a service
authenticates as itself, and is trusted with whatever its role can see,
because *what it will ask for* is a fixed, auditable set of queries baked
into its code at compile time.

An AI agent talking over MCP is different in one specific way: the natural
language request driving it is not known in advance, and the agent decides
at runtime which tool to call, with which arguments, how many times, and
how it recombines the results. You cannot review "the query" ahead of time,
because there isn't one fixed query — there's a tool contract, and the
contract is all you get to defend.

That means access control has to live partly *inside* each tool's response,
not just at the connection/identity layer:

- **Identity-based control** (who is allowed to call `search_transactions`
  at all) is necessary but not sufficient.
- **Content-aware control** (what fields come back, how much data one call
  can return, whether a broad query is redacted more aggressively than a
  narrow one) is what actually bounds what an agent can learn from a
  single natural-language-driven interaction.

This repo demonstrates that second layer concretely: a field-level
redaction policy (`redaction.yaml`), a hard per-call row cap, and an audit
log of every real invocation — on top of (not instead of) a dedicated
least-privilege database role.

## The three tools

All three are read-only and backed directly by SQL against
`journal_entries` and `account_balances` (schema owned by a sibling
service, `ledgerline-persistence-sync`).

### `get_account_balance`

- **Input:** `{"account_id": "..."}`
- **Output:** `{"account_id", "currency", "balance_minor"}` from
  `account_balances`.
- **Redaction:** none. This is the "tool-level scoping" case: the tool's
  declared contract is already a narrow, single-row lookup for the exact
  account the caller named, and `account_balances` has no internal-only
  columns to begin with. There's nothing left to redact.

### `get_journal_entry`

- **Input:** `{"transfer_dedup_id": "..."}`
- **Output:** every `journal_entries` row sharing that dedup id (both legs
  of the transfer), each as
  `{"account_id", "direction", "amount_minor", "currency", "gl_account", "posted_at"}`.
- **Redaction:** `cost_center` is **omitted** from every row (classified
  `internal_sensitive`, action `redact` in `redaction.yaml`).

### `search_transactions`

- **Input:** `{"account_id": "...", "limit": N}`
- **Output:** up to `limit` `journal_entries` rows for that account, most
  recent first by `posted_at`.
- **Redaction:** `cost_center` is omitted (same as above), **and**
  `transfer_dedup_id` is **masked** down to a short prefix/suffix (e.g.
  `txn-...7890`, 4 characters shown on each side; if the value isn't long
  enough for that to hide anything, it's fully replaced with asterisks
  instead — see `redact.MaskValue`).
- **Guardrail:** `limit` is hard-capped server-side at
  `-max-search-limit` (default **20**) regardless of what the caller
  requests. This is independent of field redaction: it bounds how much
  data a single broad call can pull, not which fields are visible in it.

The policy is deliberately asymmetric: `get_journal_entry` is a narrow,
single-dedup-id lookup (the caller already supplied the exact identifier,
so echoing it back is low-risk), while `search_transactions` is a broader
scan over an account's history, so it gets the stricter treatment
(masking, not just omission, of the dedup id) on top of the same
row-count cap. A less-scoped query gets more redaction, not less.

## Redaction policy: `redaction.yaml`

Field classification is config, not code. `internal/redact/policy.go`
loads `redaction.yaml` at startup and the handlers call
`policy.ActionFor("field_name")` — nothing in `internal/redact/redact.go`
hardcodes which field gets redacted. Changing what's exposed is an edit to
`redaction.yaml`, not a recompile.

```yaml
fields:
  cost_center:
    classification: internal_sensitive
    action: redact      # omitted from every response
  transfer_dedup_id:
    classification: internal_sensitive
    action: mask        # shown as prefix...suffix
  account_id:
    classification: public
    action: none
  # ...direction, amount_minor, currency, gl_account, posted_at,
  # balance_minor are all "public" / "none"
```

`classification` is the two-value scheme the brief asks for
(`public` / `internal_sensitive`); `action` is the mechanism used to
enforce it (`none` / `redact` / `mask`). If `action` is omitted,
`internal_sensitive` defaults to `redact` and `public` defaults to `none`,
so the minimal correct config is just classifying a field.

`internal/redact/redact.go` exposes this as pure functions operating on
plain structs — `RedactJournalRow(row, policy)` and
`RedactSearchRow(row, policy)` — with no database or MCP dependency, so
they're unit-testable in isolation (see `internal/redact/redact_test.go`).

## Least-privilege database role (one-time setup)

The gateway connects as a dedicated, read-only Postgres role,
`ledgerline_ai_readonly`, never as the admin/owner role other Ledgerline
services use to write. Create it once against the shared `ledgerline`
database:

```sql
CREATE ROLE ledgerline_ai_readonly LOGIN;
GRANT CONNECT ON DATABASE ledgerline TO ledgerline_ai_readonly;
GRANT USAGE ON SCHEMA public TO ledgerline_ai_readonly;
GRANT SELECT ON journal_entries, account_balances TO ledgerline_ai_readonly;
```

(Local Postgres here uses `trust` auth for local connections, so no
password is required; adjust `CREATE ROLE ... LOGIN` to
`... LOGIN PASSWORD '...'` plus the corresponding `-postgres-url` if your
`pg_hba.conf` requires a password.)

The `-postgres-url` flag defaults to this role's connection string:

```
postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable
```

**Verify the role genuinely cannot write** (done for real against the live
local Postgres as part of this repo's verification — see below):

```console
$ psql "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" -c "SELECT count(*) FROM journal_entries;"
 count
-------
    10
(1 row)

$ psql "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" -c "DELETE FROM journal_entries;"
ERROR:  permission denied for table journal_entries

$ psql "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" -c "UPDATE account_balances SET balance_minor = 0;"
ERROR:  permission denied for table account_balances

$ psql "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" -c "INSERT INTO journal_entries (transfer_dedup_id, account_id, direction, amount_minor, currency, posted_at, raft_index) VALUES ('x','x','debit',1,'USD',now(),1);"
ERROR:  permission denied for table journal_entries
```

## Audit logging

Every tool call — success or failure — emits exactly one structured JSON
line, via `internal/audit`:

```json
{"timestamp":"2026-10-28T07:55:21.561805Z","tool":"get_journal_entry","args":{"transfer_dedup_id":"txn-2"},"result_row_count":2,"redacted_fields":["cost_center"]}
```

Fields:

- `timestamp` — UTC, RFC3339Nano.
- `tool` — the MCP tool name.
- `args` — the raw caller input, unredacted (so the exact request is
  reconstructable later). For `search_transactions`, this also includes
  `effective_limit`, the post-cap value actually used, alongside the raw
  `limit` the caller requested.
- `result_row_count` — rows matched/returned (`0` on error or empty
  result).
- `redacted_fields` — distinct field names redacted/masked anywhere in the
  response; always an array, `[]` when nothing was redacted (e.g.
  `get_account_balance`).
- `error` — present (and the call's error message) only on failure.

By default this is written one JSON line per call to **stderr**, not
stdout — because in stdio transport mode, stdout is the MCP JSON-RPC
protocol stream itself, and mixing audit lines into it would corrupt the
protocol. Pass `-audit-log /path/to/file.jsonl` to append to a file
instead.

This is the auditability story for this gateway: since an AI agent's
queries aren't statically known in advance, the only thing that *can* be
reviewed ahead of time is the tool contract (schema + redaction policy);
what actually happened has to be reconstructed from the audit log after
the fact.

## Build, test, run

```console
$ go build ./...
$ go vet ./...
$ gofmt -l .        # expect no output
$ go test ./...
```

`go test ./...` runs entirely against pure Go structs and a fake in-memory
`DataStore` (see `internal/tools/tools_test.go`) — no live Postgres
connection is required for any of it. Coverage includes: field redaction
is applied per-policy, masking format (`prefix...suffix`, with
full-asterisk fallback for short values) is correct, `search_transactions`
enforces the hard cap regardless of the requested limit, and every audit
log entry produced during a call is well-formed JSON with the required
keys.

Run the server itself (after the one-time role setup above):

```console
$ go build -o bin/ledgerline-ai-gateway ./cmd/ledgerline-ai-gateway
$ ./bin/ledgerline-ai-gateway \
    -postgres-url "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" \
    -redaction-policy redaction.yaml \
    -audit-log /tmp/ledgerline-ai-gateway-audit.jsonl
```

It speaks MCP over stdio (`initialize`, `tools/list`, `tools/call`) — wire
it up to any MCP-compatible client/agent by pointing it at this binary and
those flags.

### Manually exercising it end-to-end

`cmd/verify-mcp` is a small standalone Go program (not shipped as part of
the gateway) that spawns the compiled binary as a subprocess and drives it
with mcp-go's own **client** package — a genuine protocol round trip over
stdio, not a direct call into the Go functions:

```console
$ go build -o bin/ledgerline-ai-gateway ./cmd/ledgerline-ai-gateway
$ go run ./cmd/verify-mcp \
    -binary ./bin/ledgerline-ai-gateway \
    -account-id alice \
    -dedup-id txn-2 \
    -max-search-limit 3 \
    -audit-log /tmp/ledgerline-ai-gateway-audit.jsonl
```

`-account-id`/`-dedup-id` should be real values from your `ledgerline`
database (look them up with `psql ledgerline -c "select account_id from account_balances"`,
etc.). `-max-search-limit` is optional and passed straight through to the
server; it's useful for proving the hard cap against a small real dataset
without needing 20+ rows of live data.

## MCP implementation: library, not hand-rolled

`github.com/mark3labs/mcp-go@v1.4.0` resolved cleanly via `go get` and its
current API (`server.NewMCPServer`, `mcp.NewTool`/`mcp.With*` schema
builders, `server.ServeStdio`, plus a real `client` package for the
verification harness) matched its README/godoc, so this gateway is built
directly on it rather than a hand-rolled JSON-RPC implementation. Tool
responses use `mcp.NewToolResultJSON` (structured content + a JSON text
fallback); errors use `mcp.NewToolResultError` so a failed call is reported
as a tool-level error (`isError: true`) rather than a transport failure.

## Known simplifications

This is a demonstration of the *pattern*, not a production PCI/PII
compliance system. In particular:

- **The redaction policy is illustrative.** `cost_center` and
  `transfer_dedup_id` were picked to show "omit" vs. "mask" against this
  schema; a real deployment would need a real data-classification review,
  not two example fields in a YAML file.
- **No rate limiting.** An agent can call any tool as many times as it
  wants; `search_transactions`' row cap bounds one call's result size, not
  call frequency.
- **No per-caller auth/session scoping beyond the DB role.** The gateway
  process authenticates to Postgres as `ledgerline_ai_readonly`, but there
  is no notion of distinct AI-agent identities, API keys, or per-agent
  scoping at the MCP layer itself — every caller of this process gets the
  same three tools and the same policy. A real deployment would likely
  need per-session/per-agent identity in front of this.
- **Masking degrades to full redaction for short values.** With this
  demo's seed data (`txn-1`..`txn-5`), `transfer_dedup_id` is shorter than
  the 4+4 character prefix/suffix window, so it's fully replaced with
  asterisks rather than showing a `prefix...suffix` shape — see
  `internal/redact/redact_test.go` for the `prefix...suffix` case
  exercised against longer values.
- **No connection-level TLS/mTLS configuration** is included for the
  Postgres connection; this assumes a trusted local/private network, as is
  the case for the local trust-auth Postgres this was built and verified
  against.
- **Audit log is a local file/stderr, not shipped anywhere.** A real
  deployment would forward it to a centralized, tamper-evident log sink.
