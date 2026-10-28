#!/usr/bin/env bash
# Runs the full Ledgerline stack directly as Go binaries (no Docker) against
# your local Postgres. This is the exact shape of verification actually run
# while building every repo in this project -- see each repo's own README
# for the specific commands that were used to validate it in isolation.
#
# Prereqs:
#   - Go toolchain installed
#   - A local Postgres reachable at postgres://localhost:5432/ledgerline
#     (create the DB first: `createdb ledgerline`)
#   - All ledgerline-* repos checked out as siblings of this one
#     (../ledgerline-raft, ../ledgerline-gateway, etc.)
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK=/tmp/ledgerline-local-demo
rm -rf "$WORK" && mkdir -p "$WORK"
PIDS=()
cleanup() {
  echo "==> Shutting down..."
  for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

echo "==> Resetting the Postgres projection tables for a clean, deterministic run..."
# persistence-sync's sync_state.last_raft_index is a single global counter -
# it has no notion of "which raft cluster" it's tracking. Reusing a database
# that already has state from a previous (now-gone) cluster would make it
# resume from the wrong index and silently skip this run's events entirely
# (a real gotcha, discovered while building this script - see the top-level
# README's "Known simplifications" section). Dropping the tables here makes
# persistence-sync recreate them fresh and start from index 0.
psql "postgres://localhost:5432/ledgerline?sslmode=disable" -c \
  "DROP TABLE IF EXISTS journal_entries, account_balances, sync_state CASCADE;" >/dev/null

echo "==> Building every binary..."
go build -C "$ROOT/ledgerline-raft" -o /tmp/ledgerd ./cmd/ledgerd
go build -C "$ROOT/ledgerline-accounting-core" -o /tmp/accountingsvc ./cmd/accountingsvc
go build -C "$ROOT/ledgerline-gateway" -o /tmp/gateway ./cmd/gateway
go build -C "$ROOT/ledgerline-balance-reader" -o /tmp/balance-reader .
go build -C "$ROOT/ledgerline-persistence-sync" -o /tmp/persistence-sync ./cmd/persistence-sync
go build -C "$ROOT/ledgerline-fx" -o /tmp/ledgerline-fx ./cmd/ledgerline-fx
go build -C "$ROOT/ledgerline-orchestrator" -o /tmp/orchestrator .
go build -C "$ROOT/ledgerline-reporting" -o /tmp/trialbalance ./cmd/trialbalance

echo "==> Starting a 3-node ledgerline-raft cluster (ports 7001-3 / 8001-3)..."
PEERS="n1=127.0.0.1:7001=127.0.0.1:8001,n2=127.0.0.1:7002=127.0.0.1:8002,n3=127.0.0.1:7003=127.0.0.1:8003"
/tmp/ledgerd -node-id n1 -raft-addr 127.0.0.1:7001 -http-addr 127.0.0.1:8001 -data-dir "$WORK/n1" -peers "$PEERS" -bootstrap=true &
PIDS+=($!)
/tmp/ledgerd -node-id n2 -raft-addr 127.0.0.1:7002 -http-addr 127.0.0.1:8002 -data-dir "$WORK/n2" -peers "$PEERS" &
PIDS+=($!)
/tmp/ledgerd -node-id n3 -raft-addr 127.0.0.1:7003 -http-addr 127.0.0.1:8003 -data-dir "$WORK/n3" -peers "$PEERS" &
PIDS+=($!)
sleep 3
curl -sf http://127.0.0.1:8001/v1/leader; echo

echo "==> Starting accountingsvc (8090), gateway (8080), balance-reader (8280), fx (8590), persistence-sync (8380)..."
(cd "$ROOT/ledgerline-accounting-core" && exec /tmp/accountingsvc -rules accounting/rules.yaml -http-addr 127.0.0.1:8090) &
PIDS+=($!)
/tmp/gateway -http-addr 127.0.0.1:8080 -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 -accounting-addr http://127.0.0.1:8090 &
PIDS+=($!)
/tmp/balance-reader -http-addr 127.0.0.1:8280 -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 &
PIDS+=($!)
(cd "$ROOT/ledgerline-fx" && exec /tmp/ledgerline-fx -http-addr 127.0.0.1:8590 -seed-file rates_seed.json) &
PIDS+=($!)
/tmp/persistence-sync -postgres-url "postgres://localhost:5432/ledgerline?sslmode=disable" -ledger-nodes 127.0.0.1:8001,127.0.0.1:8002,127.0.0.1:8003 -http-addr 127.0.0.1:8380 &
PIDS+=($!)
sleep 2

echo "==> Creating accounts and submitting a fee-split transfer..."
curl -s -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"platform","currency":"USD","unlimited":true}'; echo
curl -s -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"seller-1","currency":"USD"}'; echo
curl -s -X POST http://127.0.0.1:8001/v1/accounts -d '{"account_id":"platform-fee-revenue","currency":"USD"}'; echo
curl -s -X POST http://127.0.0.1:8080/v1/transfers -d '{
  "dedup_id":"local-demo-1","from_account":"platform","to_account":"seller-1",
  "amount_minor":10000,"fee_minor":300,"currency":"USD",
  "category":"seller_payout","country":"US"
}'; echo

echo "==> Fast path (balance-reader) vs. the ledger's own read path..."
sleep 1
curl -s http://127.0.0.1:8280/v1/balances/seller-1; echo
curl -s http://127.0.0.1:8001/v1/accounts/seller-1; echo

echo "==> Trial balance / reconciliation report (reads what persistence-sync just wrote to Postgres)..."
sleep 1
/tmp/trialbalance -postgres-url "postgres://localhost:5432/ledgerline?sslmode=disable"

cat <<'EOF'

==> Local demo complete. Everything above ran as plain OS processes -- no
    Docker required. Press Ctrl+C or let this script exit to tear it all
    down (trap cleanup kills every process it started).
EOF
read -r -p "Press Enter to shut everything down... "
