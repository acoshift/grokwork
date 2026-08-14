package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/acoshift/grokwork/internal/agentmcp"
	"github.com/acoshift/grokwork/internal/grokrun"
)

// runAgentMCPStdio is the MCP server process the coding CLI spawns for grokwork tools.
// It proxies tools/call over the host Unix socket using the per-run token.
// Missing env is not a process failure: user-scope grok attach starts this
// binary outside a grokwork run; serve the catalog and error on call.
func runAgentMCPStdio() error {
	token := os.Getenv(grokrun.AgentTokenEnv)
	sock := os.Getenv(grokrun.AgentSockEnv)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	call := func(ctx context.Context, tok, name string, args map[string]any) (any, error) {
		if token == "" || sock == "" {
			return nil, fmt.Errorf("not a grokwork-attached run")
		}
		return agentmcp.ClientCall(ctx, sock, tok, name, args)
	}
	list := func(ctx context.Context, tok string) []agentmcp.ToolDef {
		if token == "" || sock == "" {
			// User-scope grok attach outside a run: show the full catalog.
			return agentmcp.ToolDefs()
		}
		defs, err := agentmcp.ClientListTools(ctx, sock, tok)
		if err != nil {
			return []agentmcp.ToolDef{}
		}
		return defs
	}
	return agentmcp.RunStdioDefaultList(ctx, call, list, token)
}
