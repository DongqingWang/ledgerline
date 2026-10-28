// Command ledgerline-ai-gateway is an MCP (Model Context Protocol) server
// that lets an AI agent query ledger data (account balances and journal
// entries) over stdio, with field-level redaction, a hard result-size
// guardrail, structured audit logging, and a dedicated least-privilege
// Postgres role.
//
// See README.md for the full design rationale, setup, and manual
// verification steps.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/ledgerline/ledgerline-ai-gateway/internal/audit"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/db"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/redact"
	"github.com/ledgerline/ledgerline-ai-gateway/internal/tools"
)

// defaultPostgresURL points at the dedicated, read-only
// ledgerline_ai_readonly Postgres role (see README.md for the exact
// CREATE ROLE/GRANT statements). It intentionally does NOT use the
// admin/owner role that sibling services use for writes.
const defaultPostgresURL = "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable"

func main() {
	postgresURL := flag.String("postgres-url", defaultPostgresURL,
		"Postgres connection string. Must point at a least-privilege, read-only role (see README.md) -- never an admin/owner role.")
	policyPath := flag.String("redaction-policy", "redaction.yaml",
		"Path to the field-level redaction policy YAML file.")
	auditLogPath := flag.String("audit-log", "",
		"Path to append structured JSON audit log lines to. If empty, audit lines go to stderr "+
			"(stdout is reserved for the MCP JSON-RPC protocol stream when running over stdio).")
	maxSearchLimit := flag.Int("max-search-limit", tools.DefaultMaxSearchLimit,
		"Hard server-side cap on search_transactions' limit argument, regardless of what the caller requests.")
	flag.Parse()

	policy, err := redact.LoadPolicy(*policyPath)
	if err != nil {
		log.Printf("warning: could not load redaction policy from %q (%v); falling back to built-in default policy", *policyPath, err)
		policy = redact.DefaultPolicy()
	}

	store, err := db.Open(*postgresURL)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	if err := store.Ping(context.Background()); err != nil {
		log.Fatalf("failed to connect to database at startup (is postgres running, and does the role in -postgres-url exist? see README.md): %v", err)
	}

	var auditWriter io.Writer = os.Stderr
	if *auditLogPath != "" {
		f, err := os.OpenFile(*auditLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("failed to open audit log file %q: %v", *auditLogPath, err)
		}
		defer f.Close()
		auditWriter = f
	}
	auditLogger := audit.NewLogger(auditWriter)

	svc := tools.NewService(store, policy, auditLogger, *maxSearchLimit)

	s := server.NewMCPServer(
		"ledgerline-ai-gateway",
		"0.1.0",
		server.WithToolCapabilities(false),
		server.WithRecovery(),
	)

	s.AddTool(
		mcp.NewTool("get_account_balance",
			mcp.WithDescription("Look up the current balance for a single ledger account by account_id. "+
				"This is a narrow, scoped lookup -- the caller already named the exact account it wants, "+
				"so no field redaction is applied to the response."),
			mcp.WithString("account_id",
				mcp.Required(),
				mcp.Description(`The account id to look up, e.g. "alice".`),
			),
		),
		handleGetAccountBalance(svc),
	)

	s.AddTool(
		mcp.NewTool("get_journal_entry",
			mcp.WithDescription("Look up both legs of a double-entry transfer by its transfer_dedup_id. "+
				"The cost_center field is redacted (omitted) from every returned row per the gateway's "+
				"redaction policy (see redaction.yaml)."),
			mcp.WithString("transfer_dedup_id",
				mcp.Required(),
				mcp.Description("The transfer dedup id shared by both legs of the transfer."),
			),
		),
		handleGetJournalEntry(svc),
	)

	s.AddTool(
		mcp.NewTool("search_transactions",
			mcp.WithDescription(fmt.Sprintf(
				"Search the most recent journal entries for a single account_id, most recent first. "+
					"cost_center is redacted and transfer_dedup_id is masked (prefix...suffix) in every row, "+
					"since this is a broader, less-scoped query than get_journal_entry. "+
					"limit is hard-capped at %d server-side regardless of what is requested.",
				*maxSearchLimit,
			)),
			mcp.WithString("account_id",
				mcp.Required(),
				mcp.Description("The account id to search transactions for."),
			),
			mcp.WithNumber("limit",
				mcp.Description(fmt.Sprintf("Maximum number of rows to return. Hard-capped at %d server-side.", *maxSearchLimit)),
			),
		),
		handleSearchTransactions(svc),
	)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleGetAccountBalance(svc *tools.Service) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		accountID, err := req.RequireString("account_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		result, err := svc.GetAccountBalance(ctx, tools.GetAccountBalanceArgs{AccountID: accountID})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		res, err := mcp.NewToolResultJSON(result)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return res, nil
	}
}

func handleGetJournalEntry(svc *tools.Service) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dedupID, err := req.RequireString("transfer_dedup_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		result, err := svc.GetJournalEntry(ctx, tools.GetJournalEntryArgs{TransferDedupID: dedupID})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		res, err := mcp.NewToolResultJSON(result)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return res, nil
	}
}

func handleSearchTransactions(svc *tools.Service) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		accountID, err := req.RequireString("account_id")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		limit := req.GetInt("limit", 0)

		result, err := svc.SearchTransactions(ctx, tools.SearchTransactionsArgs{AccountID: accountID, Limit: limit})
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}

		res, err := mcp.NewToolResultJSON(result)
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		return res, nil
	}
}
