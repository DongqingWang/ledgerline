-- Creates the least-privilege role ledgerline-ai-gateway connects as.
-- Table-level SELECT grants are deferred to scripts/demo.sh, run *after*
-- ledgerline-persistence-sync has created journal_entries/account_balances
-- -- this init script runs once, at first container startup, before any
-- application has had a chance to create those tables.
CREATE ROLE ledgerline_ai_readonly LOGIN;
GRANT CONNECT ON DATABASE ledgerline TO ledgerline_ai_readonly;
GRANT USAGE ON SCHEMA public TO ledgerline_ai_readonly;
