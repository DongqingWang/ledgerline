// Command verify-mcp is a manual verification harness, not part of the
// shipped gateway. It spawns the compiled ledgerline-ai-gateway binary as a
// subprocess, speaks real MCP protocol to it over stdio using mcp-go's own
// client package (so this is a genuine protocol-level round trip, not a
// call into the Go functions directly), and prints each request/response
// pair as JSON so it can be pasted into a verification report.
//
// Usage:
//
//	go run ./cmd/verify-mcp \
//	    -binary ./bin/ledgerline-ai-gateway \
//	    -postgres-url "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable" \
//	    -account-id alice \
//	    -dedup-id txn-2
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func printJSON(label string, v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Printf("%s: <failed to marshal: %v>\n", label, err)
		return
	}
	fmt.Printf("--- %s ---\n%s\n\n", label, b)
}

func main() {
	binary := flag.String("binary", "./bin/ledgerline-ai-gateway", "path to the compiled server binary")
	postgresURL := flag.String("postgres-url", "postgres://ledgerline_ai_readonly@localhost:5432/ledgerline?sslmode=disable", "postgres connection string to pass to the server")
	redactionPolicy := flag.String("redaction-policy", "redaction.yaml", "path to redaction.yaml to pass to the server")
	auditLog := flag.String("audit-log", "", "path to pass as -audit-log to the server (empty = server default, stderr)")
	accountID := flag.String("account-id", "alice", "a real account_id present in account_balances/journal_entries")
	dedupID := flag.String("dedup-id", "txn-2", "a real transfer_dedup_id present in journal_entries")
	maxSearchLimit := flag.Int("max-search-limit", 0, "if > 0, passed through as -max-search-limit to the server (useful to demonstrate the hard cap against a small real dataset)")
	flag.Parse()

	serverArgs := []string{
		"-postgres-url", *postgresURL,
		"-redaction-policy", *redactionPolicy,
	}
	if *auditLog != "" {
		serverArgs = append(serverArgs, "-audit-log", *auditLog)
	}
	if *maxSearchLimit > 0 {
		serverArgs = append(serverArgs, "-max-search-limit", fmt.Sprintf("%d", *maxSearchLimit))
	}

	stdio := transport.NewStdio(*binary, nil, serverArgs...)
	mcpClient := client.NewClient(stdio)

	ctx := context.Background()
	if err := mcpClient.Start(ctx); err != nil {
		log.Fatalf("failed to start server subprocess: %v", err)
	}
	defer mcpClient.Close()

	initResult, err := mcpClient.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "verify-mcp",
				Version: "1.0.0",
			},
		},
	})
	if err != nil {
		log.Fatalf("initialize failed: %v", err)
	}
	printJSON("initialize response", initResult)

	toolsResult, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		log.Fatalf("tools/list failed: %v", err)
	}
	printJSON("tools/list response", toolsResult)

	// 1. get_account_balance
	callAndPrint(ctx, mcpClient, "get_account_balance", map[string]any{
		"account_id": *accountID,
	})

	// 2. get_journal_entry
	callAndPrint(ctx, mcpClient, "get_journal_entry", map[string]any{
		"transfer_dedup_id": *dedupID,
	})

	// 3. search_transactions, requesting far more than the hard cap to
	// prove the server-side limit actually bites.
	callAndPrint(ctx, mcpClient, "search_transactions", map[string]any{
		"account_id": *accountID,
		"limit":      1000,
	})

	// 3b. search_transactions with a request under the cap, for contrast.
	callAndPrint(ctx, mcpClient, "search_transactions", map[string]any{
		"account_id": *accountID,
		"limit":      2,
	})

	// 4. error path: unknown account, to show failures are still well-formed.
	callAndPrint(ctx, mcpClient, "get_account_balance", map[string]any{
		"account_id": "this-account-does-not-exist",
	})

	fmt.Fprintln(os.Stderr, "verify-mcp: done")
}

func callAndPrint(ctx context.Context, c *client.Client, tool string, args map[string]any) {
	req := mcp.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args

	printJSON(fmt.Sprintf("tools/call request: %s", tool), req.Params)

	result, err := c.CallTool(ctx, req)
	if err != nil {
		fmt.Printf("--- tools/call transport error: %s ---\n%v\n\n", tool, err)
		return
	}
	printJSON(fmt.Sprintf("tools/call response: %s (isError=%v)", tool, result.IsError), result)
}
