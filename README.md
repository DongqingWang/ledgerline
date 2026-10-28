# ledgerline-fx

An independent foreign-exchange (FX) rate service for the Ledgerline
distributed ledger platform (the "fxcenter" role in the overall design). It
ingests currency conversion rates and serves rate lookups plus a minor-unit
conversion helper over HTTP.

This repository has **no dependency on any other Ledgerline repo** — it is a
small, standalone Go service that can be built, tested, and run entirely on
its own.

## Why centralize FX as its own service?

Every consumer in the platform that needs to convert between currencies —
journaling (recording a transaction in the ledger's functional currency),
pricing, and reporting — must use the **exact same rate for the exact same
transaction**. If each consumer pulled its own rate independently (different
providers, different fetch times, different rounding), the resulting
double-entry postings would not reconcile: two services converting the same
USD amount to EUR at slightly different rates would produce books that don't
balance. Centralizing rate sourcing and serving behind one service gives the
rest of the platform a single, authoritative, point-in-time rate to reference
(the `as_of` timestamp on every response), so reconciliation is possible.

## What it does

- Loads a static, USD-anchored seed rate table (`rates_seed.json`) at
  startup. This is the default, always-available data source: normal
  operation and `go test` work fully offline against it, no network
  required.
- Optionally (via `-refresh`) periodically fetches live rates from a public,
  no-API-key FX endpoint and atomically swaps them into the in-memory table.
- Serves:
  - `GET /v1/rates/{base}/{quote}` — the rate to convert 1 unit of `base`
    into `quote`, derived from the USD-anchored table. Works for **any**
    pair derivable from the table, not just USD-as-base (e.g. `EUR/GBP` is
    computed as `(USD/GBP) / (USD/EUR)`).
  - `POST /v1/convert` — converts a minor-unit amount (e.g. cents) from
    `base` to `quote` using the current rate.
  - `GET /healthz` — liveness/health info including the as-of timestamp of
    the currently served rates and whether live refresh is enabled.

## Running

```sh
go run ./cmd/ledgerline-fx -http-addr 127.0.0.1:8590
```

Flags:

| Flag                | Default               | Description                                                   |
|---------------------|------------------------|----------------------------------------------------------------|
| `-http-addr`         | `127.0.0.1:8590`       | Address to listen on                                           |
| `-seed-file`         | `rates_seed.json`      | Path to the static seed rate JSON file                         |
| `-refresh`           | `false`                | Enable periodic live refresh from a public FX API              |
| `-refresh-interval`  | `15m`                  | How often to refresh when `-refresh` is set                    |

### Live refresh

When run with `-refresh`, the service periodically calls
`https://open.er-api.com/v6/latest/USD` (a free, no-API-key-required FX rate
endpoint; verified reachable at the time of writing) and, on success,
atomically replaces the in-memory rate table. The actual HTTP fetch sits
behind a small `fxstore.RateSource` interface
(`FetchRates(ctx) (map[string]float64, error)`), so unit tests inject a fake
source instead of making real network calls — the default `go test ./...`
run never touches the network.

If a live fetch fails (network error, non-200 response, unexpected payload),
the error is logged and the service keeps serving the last-known-good rate
table — the static seed if no refresh has ever succeeded yet — rather than
crashing or serving empty data.

```sh
go run ./cmd/ledgerline-fx -refresh -refresh-interval 5m
```

## API examples

```sh
curl http://127.0.0.1:8590/v1/rates/USD/EUR
# {"base":"USD","quote":"EUR","rate":0.92,"as_of":"2026-01-01T00:00:00Z"}

curl http://127.0.0.1:8590/v1/rates/EUR/GBP
# {"base":"EUR","quote":"GBP","rate":0.858695...,"as_of":"2026-01-01T00:00:00Z"}

curl -X POST http://127.0.0.1:8590/v1/convert \
  -H 'Content-Type: application/json' \
  -d '{"base":"USD","quote":"EUR","amount_minor":10000}'
# {"base":"USD","quote":"EUR","amount_minor":10000,"converted_amount_minor":9200,"rate":0.92}

curl http://127.0.0.1:8590/healthz
# {"status":"ok","rates_as_of":"2026-01-01T00:00:00Z","refresh_enabled":false}

curl http://127.0.0.1:8590/v1/rates/USD/XYZ
# 404 {"error":"unknown currency: XYZ"}
```

## Known simplifications

- **No minor-unit-granularity handling across currencies.** `amount_minor`
  is treated uniformly as "amount times 100" for every currency in both the
  request and response, and the converted amount is simply
  `round(amount_minor * rate)`. Real-world currencies differ (e.g. JPY has
  no minor unit in practice, some currencies use 3 decimal places), but
  modeling that is out of scope for this demo service.
- **Live refresh data source reliability.** `open.er-api.com` is a free,
  community-run service with no uptime SLA, and the upstream `result` field
  and schema could change. The refresher treats any fetch/parse failure as
  non-fatal and keeps serving the last-known-good table, but there is no
  retry backoff, circuit breaker, or alerting if the live source is down for
  an extended period — for a production deployment you'd want a paid,
  SLA-backed provider and better observability around refresh health.
- **No persistence.** The rate table lives in memory only; a restart resets
  it to the static seed (or triggers a fresh live fetch if `-refresh` is
  set and the first tick has run).
- **Single-threaded refresh scheduling.** Only one refresh loop runs at a
  time; there's no jitter or staggering, which is fine for a single
  instance of this demo service but would need thought in a
  multi-replica production deployment to avoid a thundering herd against
  the upstream API.

## Development

```sh
go build ./...
go vet ./...
go test ./...
gofmt -l .   # should print nothing
```
