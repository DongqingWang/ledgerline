# ledgerline-admin-console

A small, local-only demo web UI for [Ledgerline](https://github.com/ledgerline) — a way
to see and interact with the running stack (balances, a transfer form, FX
rates) without curling every endpoint by hand.

> **No authentication, no production hardening.** This is explicitly a local
> demo tool. Don't expose it publicly, and don't use the permissive CORS
> settings on its backends (see below) outside local development.

## What it does

Three tabs, talking directly (client-side, via `fetch`) to three Ledgerline
backend services:

1. **Balances** — lists every account from `ledgerline-balance-reader`'s
   `GET /v1/balances`, auto-refreshing every 3 seconds so it stays live while
   you submit transfers from the other tab.
2. **Submit Transfer** — a form that POSTs to `ledgerline-gateway`'s
   `POST /v1/transfers` (the full gateway contract: `dedup_id`, `from_account`,
   `to_account`, `amount_minor`, optional `fee_minor`/`category`/`country`),
   and shows the raw JSON response (including the composed journal and the
   underlying ledger result(s)) on the page.
3. **FX Rates** — queries `ledgerline-fx`'s `GET /v1/rates/{base}/{quote}` and
   `POST /v1/convert` for a handful of currencies.

Each view handles its backend being unreachable gracefully (an inline error
banner, not a crashed app) — useful since in local development it's common
to have only some of the stack running at a time.

## Backend URLs

All three backend base URLs are configurable via Vite env vars, so a
docker-compose setup (or any other deployment) can point this UI at
different hostnames without touching the code:

| Env var | Default | Backend |
|---|---|---|
| `VITE_GATEWAY_URL` | `http://127.0.0.1:8080` | `ledgerline-gateway` |
| `VITE_BALANCE_READER_URL` | `http://127.0.0.1:8280` | `ledgerline-balance-reader` |
| `VITE_FX_URL` | `http://127.0.0.1:8590` | `ledgerline-fx` |

**Note on CORS**: since this is a browser app running on its own dev-server
origin, calling these backends cross-origin requires them to send
`Access-Control-Allow-Origin` headers. All three backend repos include a
permissive (`*`) CORS middleware specifically to support this — fine for a
local demo, not something you'd want on a public deployment of those
services.

## Running

```sh
npm install

# point at whatever's actually running; these are the defaults shown above
VITE_GATEWAY_URL=http://127.0.0.1:8080 \
VITE_BALANCE_READER_URL=http://127.0.0.1:8280 \
VITE_FX_URL=http://127.0.0.1:8590 \
npm run dev

# production-style build + preview
npm run build
npm run preview
```

You'll need at least `ledgerline-balance-reader` running to see anything on
the Balances tab, `ledgerline-gateway` (+ a `ledgerline-raft` cluster behind
it, + `ledgerline-accounting-core`'s `accountingsvc`) to submit transfers,
and `ledgerline-fx` for the FX tab. See the top-level `ledgerline-docs` repo
for a docker-compose setup that brings up the whole stack at once.

## Known simplifications

- **No auth.** Anyone who can reach this UI can submit transfers against
  whatever gateway it's pointed at.
- **2-decimal-place currency formatting.** `formatMinorAmount` assumes every
  currency's minor unit is 1/100 of its major unit (true for USD/EUR/GBP,
  not true for all real-world currencies — e.g. JPY has no minor unit). A
  production tool would look up the actual decimal precision per currency
  code.
- **Auto-refresh polling, not push.** The Balances view polls every 3s
  rather than subscribing to a live update stream.
- **No routing library.** Three tabs are handled with local component state,
  not a URL-based router — fine at this scale, wouldn't scale past a few
  more views.
