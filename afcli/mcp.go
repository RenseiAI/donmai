package afcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// newMCPCmd constructs the `donmai mcp` command group. It hosts the
// code-intelligence MCP server the runner spawns per session (via
// os.Executable() + ["mcp","code-intel","--root",<wpath>]).
func newMCPCmd(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run Donmai MCP servers",
		Long: `Run Donmai Model Context Protocol (MCP) servers.

The local code-intelligence server is free, runs over stdio, and indexes a
repository or worktree rooted at an explicit absolute path. See
/docs/CODE-INTELLIGENCE-MCP-CONTRACT.md for the frozen six-tool contract and
client configuration guidance.`,
	}
	cmd.AddCommand(newMCPCodeIntelCmd(cfg))
	return cmd
}

// newMCPCodeIntelCmd constructs `donmai mcp code-intel`: a stdio MCP server
// (server name "af-code-intelligence") exposing the six af_code_* code
// intelligence tools over newline-delimited JSON-RPC on stdin/stdout, backed
// by one long-lived warm codeintel engine.
//
// Flags follow the frozen wire contract:
//   - --root      ABSOLUTE path to the session repo/worktree root. REQUIRED —
//     the runner always passes it explicitly because the process cwd is
//     unreliable across sandbox targets.
//   - --repo-path OPTIONAL relative subtree under root (same validation
//     semantics as the `code` group: no absolute paths, no ../ escapes).
//   - --tools     OPTIONAL comma-separated subset of the six tool names to
//     expose; empty exposes all six. Unknown names fail loud at startup.
func newMCPCodeIntelCmd(_ Config) *cobra.Command {
	var (
		root     string
		repoPath string
		tools    string
		verify   bool
	)

	cmd := &cobra.Command{
		Use:   "code-intel",
		Short: "Serve the af-code-intelligence MCP server over stdio",
		Long: `Serve the code-intelligence engine as an MCP (Model Context Protocol) server
over stdio: newline-delimited JSON-RPC 2.0 on stdin/stdout, server name
"af-code-intelligence", exposing six tools (af_code_get_repo_map,
af_code_search_symbols, af_code_search_code, af_code_check_duplicate,
af_code_find_type_usages, af_code_validate_cross_deps).

The index is built once at startup (warm-up, logged to stderr) and reused
across tool calls; the session-scoped index is process-lived. stdout carries
ONLY JSON-RPC — never logs. Shuts down gracefully on stdin EOF.

This is a stdio server entry point. Configure an MCP client to run it with an
absolute repository or worktree root. Use --verify to perform a local
initialize + tools/list + six-tool-call readiness check without entering server mode. See
/docs/CODE-INTELLIGENCE-MCP-CONTRACT.md for the frozen contract.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			toolSubset := parseMCPToolSubset(tools)
			srv, err := newMCPCodeIntelServer(cmd, root, repoPath, toolSubset)
			if err != nil {
				return err
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if verify {
				return verifyMCPCodeIntel(ctx, cmd.OutOrStdout(), srv)
			}
			return srv.Serve(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&root, "root", "",
		"ABSOLUTE path to the session repo/worktree root to index (required)")
	cmd.Flags().StringVar(&repoPath, "repo-path", "",
		"Optional RELATIVE subtree under --root to scope indexing to (no absolute paths, no ../ escapes)")
	cmd.Flags().StringVar(&tools, "tools", "",
		"Optional comma-separated subset of the six af_code_* tools to expose (empty = all)")
	cmd.Flags().BoolVar(&verify, "verify", false,
		"Initialize the local server and verify its complete six-tool profile")
	_ = cmd.MarkFlagRequired("root")

	// The runner authors the MCP entry with Command=os.Executable() and
	// Args=["mcp","code-intel","--root",<wpath>] (+ --repo-path/--tools when
	// set), so this flag set is the frozen contract that entry builds against.
	return cmd
}

func parseMCPToolSubset(tools string) []string {
	var toolSubset []string
	if s := strings.TrimSpace(tools); s != "" {
		for _, t := range strings.Split(s, ",") {
			if v := strings.TrimSpace(t); v != "" {
				toolSubset = append(toolSubset, v)
			}
		}
	}
	return toolSubset
}

func newMCPCodeIntelServer(cmd *cobra.Command, root, repoPath string, tools []string) (*mcpserver.Server, error) {
	srv, err := mcpserver.New(mcpserver.Config{
		Root:     root,
		RepoPath: repoPath,
		Tools:    tools,
		// Warm-up / lifecycle logging goes to stderr; stdout is the protocol
		// channel and must stay JSON-RPC-only.
		Logf: func(format string, args ...any) {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[af-code-intelligence] "+format+"\n", args...)
		},
	})
	if err != nil {
		// Fail loud: a bad --root / --repo-path / --tools must not serve.
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "mcp code-intel: %v\n", err)
		return nil, err
	}
	return srv, nil
}

type mcpCodeIntelServer interface {
	Serve(context.Context, io.Reader, io.Writer) error
}

func verifyMCPCodeIntel(parent context.Context, out io.Writer, srv mcpCodeIntelServer) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	requests, requestWriter := io.Pipe()
	responses, responseWriter := io.Pipe()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ctx, requests, responseWriter)
		_ = responseWriter.Close()
	}()
	defer func() { _ = requestWriter.Close() }()

	enc := json.NewEncoder(requestWriter)
	dec := json.NewDecoder(responses)
	request := func(id int, method string, params any, result any) error {
		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		}); err != nil {
			return fmt.Errorf("write %s request: %w", method, err)
		}
		var response struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err := dec.Decode(&response); err != nil {
			return fmt.Errorf("read %s response: %w", method, err)
		}
		if response.JSONRPC != "2.0" || response.ID != id {
			return fmt.Errorf("invalid %s response envelope", method)
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("decode %s result: %w", method, err)
		}
		return nil
	}

	var initialize struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := request(1, "initialize", map[string]any{"protocolVersion": "2025-03-26"}, &initialize); err != nil {
		return err
	}
	if initialize.ServerInfo.Name != mcpserver.ServerName {
		return fmt.Errorf("initialize reported server %q, want %q", initialize.ServerInfo.Name, mcpserver.ServerName)
	}

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := request(2, "tools/list", map[string]any{}, &listed); err != nil {
		return err
	}
	want := []string{
		mcpserver.ToolGetRepoMap,
		mcpserver.ToolSearchSymbols,
		mcpserver.ToolSearchCode,
		mcpserver.ToolCheckDuplicate,
		mcpserver.ToolFindTypeUsages,
		mcpserver.ToolValidateCrossDeps,
	}
	if len(listed.Tools) != len(want) {
		return fmt.Errorf("tools/list returned %d tools, want %d", len(listed.Tools), len(want))
	}
	for i, tool := range listed.Tools {
		if tool.Name != want[i] {
			return fmt.Errorf("tools/list tool %d = %q, want %q", i, tool.Name, want[i])
		}
	}

	toolCalls := []struct {
		name      string
		arguments map[string]any
	}{
		{name: mcpserver.ToolGetRepoMap, arguments: map[string]any{}},
		{name: mcpserver.ToolSearchSymbols, arguments: map[string]any{"query": "MCPVerifyFixture"}},
		{name: mcpserver.ToolSearchCode, arguments: map[string]any{"query": "MCPVerifyFixture"}},
		{name: mcpserver.ToolCheckDuplicate, arguments: map[string]any{"content": "func MCPVerifyFixture() {}"}},
		{name: mcpserver.ToolFindTypeUsages, arguments: map[string]any{"typeName": "MCPVerifyFixture"}},
		{name: mcpserver.ToolValidateCrossDeps, arguments: map[string]any{}},
	}
	for i, tool := range toolCalls {
		var result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		}
		if err := request(i+3, "tools/call", map[string]any{
			"name":      tool.name,
			"arguments": tool.arguments,
		}, &result); err != nil {
			return fmt.Errorf("verify tool %s: %w", tool.name, err)
		}
		if result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" || result.Content[0].Text == "" {
			return fmt.Errorf("verify tool %s: expected one non-error text result", tool.name)
		}
	}

	if err := requestWriter.Close(); err != nil {
		return fmt.Errorf("close verify requests: %w", err)
	}
	if err := <-serveDone; err != nil {
		return fmt.Errorf("serve verify session: %w", err)
	}
	_, err := fmt.Fprintf(out, "Verified af-code-intelligence: initialize succeeded and listed and called %d tools.\n", len(want))
	return err
}
