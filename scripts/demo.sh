#!/usr/bin/env bash
# End-to-end demo of the Ledgerline stack via docker compose: brings the
# whole thing up, creates accounts, submits transfers (including a fee
# split), shows the fast balance-reader path vs. the ledger's own read path,
# grants the AI gateway's read-only role its table access (deferred past
# postgres-init since the tables don't exist until persistence-sync creates
# them), runs the trial-balance/reconciliation report, and queries the
# AI/MCP gateway to show field-level redaction in action.
#
# Requires: docker, docker compose, curl, psql (for one verification step).
set -euo pipefail
cd "$(dirname "$0")/.."

GATEWAY=http://localhost:8080
BALANCE_READER=http://localhost:8280
RAFT_N1=http://localhost:18081

echo "==> Tearing down any previous run, including its Postgres volume, for a clean/deterministic demo..."
# persistence-sync's sync_state.last_raft_index is a single global counter
# with no notion of "which raft cluster" it's tracking - reusing an old
# Postgres volume against a fresh raft cluster makes it resume from the
# wrong index and silently skip every event from this run. See the
# top-level README's "Known simplifications" section.
docker compose down -v 2>/dev/null || true

echo "==> Building and starting the core stack (this can take a few minutes the first time)..."
docker compose up -d --build postgres raft-n1 raft-n2 raft-n3 accountingsvc gateway balance-reader persistence-sync fx orchestrator admin-console

echo "==> Waiting for everything to report healthy..."
for i in $(seq 1 60); do
  unhealthy=$(docker compose ps --format '{{.Name}} {{.Health}}' | grep -v 'healthy' | grep -v '^$' || true)
  if [ -z "$unhealthy" ]; then echo "All services healthy."; break; fi
  sleep 3
done

echo "==> Who's the raft leader?"
curl -s "$RAFT_N1/v1/leader"; echo
# n1 bootstraps the cluster and reliably becomes its first leader in this
# compose setup, so the rest of this script talks to it directly for
# simplicity. A real client would use ledgerline-raft's leader-follow retry
# pattern (see pkg/ledgerclient) instead of assuming a fixed leader.

echo "==> Creating accounts (platform is an 'unlimited' external-funding account)..."
# Account creation is a raft-engine operation, not something the gateway
# exposes (the gateway's job is composing+submitting transfers) - create
# accounts straight against the raft leader:
curl -s -X POST "$RAFT_N1/v1/accounts" -d '{"account_id":"platform","currency":"USD","unlimited":true}'; echo
curl -s -X POST "$RAFT_N1/v1/accounts" -d '{"account_id":"seller-1","currency":"USD"}'; echo
curl -s -X POST "$RAFT_N1/v1/accounts" -d '{"account_id":"platform-fee-revenue","currency":"USD"}'; echo

echo "==> Submitting a fee-split transfer via the gateway (platform -> seller-1, fee to platform-fee-revenue)..."
curl -s -X POST "$GATEWAY/v1/transfers" -d '{
  "dedup_id":"demo-txn-1","from_account":"platform","to_account":"seller-1",
  "amount_minor":10000,"fee_minor":300,"currency":"USD",
  "category":"seller_payout","country":"US"
}'; echo

echo "==> Resubmitting the SAME request: should come back as a no-op duplicate, not a double-apply..."
curl -s -X POST "$GATEWAY/v1/transfers" -d '{
  "dedup_id":"demo-txn-1","from_account":"platform","to_account":"seller-1",
  "amount_minor":10000,"fee_minor":300,"currency":"USD",
  "category":"seller_payout","country":"US"
}'; echo

echo "==> Reading seller-1's balance via the FAST path (balance-reader, in-memory cache)..."
curl -s "$BALANCE_READER/v1/balances/seller-1"; echo

echo "==> ...and via the ledger engine's OWN read path (raft leader), for comparison..."
curl -s "$RAFT_N1/v1/accounts/seller-1"; echo

echo "==> Granting the AI gateway's read-only role access to the tables persistence-sync just created..."
sleep 2 # give persistence-sync a moment to create the tables from the events above
docker compose exec -T postgres psql -U postgres -d ledgerline -c \
  "GRANT SELECT ON journal_entries, account_balances TO ledgerline_ai_readonly;"

echo "==> Running the Trial Balance / reconciliation report..."
docker compose run --rm reporting

echo "==> Querying the AI/MCP gateway's tools/list and get_account_balance (shows the MCP protocol works)..."
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"demo","version":"0"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_account_balance","arguments":{"account_id":"seller-1"}}}' | docker compose run --rm -i ai-gateway || true

cat <<'EOF'

==> Demo complete. Things to try next:
  - Open http://localhost:5180 for the admin console (balances / submit transfer / FX rates).
  - curl http://localhost:8480/v1/routing to see the orchestrator's single-shard routing table.
  - docker compose run --rm reporting -format json  (or -format csv)
  - docker compose down   # tear everything down when you're done
EOF
