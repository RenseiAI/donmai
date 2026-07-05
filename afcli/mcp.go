package afcli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	mcpserver "github.com/RenseiAI/donmai/runtime/mcp/server"
)

// newMCPCmd constructs the hidden `donmai mcp` command group. It hosts the
// code-intelligence MCP server the runner spawns per session (via
// os.Executable() + ["mcp","code-intel","--root",<wpath>]). It is hidden
// because it is a machine-facing subprocess entry point, not an operator
// command.
func newMCPCmd(cfg Config) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "mcp",
		Short:  "MCP server entry points (machine-facing)",
		Hidden: true,
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
func newMCPCodeIntelCmd(cfg Config) *cobra.Command {
	bin := binaryName(cfg)
	var (
		root     string
		repoPath string
		tools    string
	)

	cmd := &cobra.Command{
		Use:    "code-intel",
		Short:  "Serve the af-code-intelligence MCP server over stdio",
		Hidden: true,
		Long: `Serve the code-intelligence engine as an MCP (Model Context Protocol) server
over stdio: newline-delimited JSON-RPC 2.0 on stdin/stdout, server name
"af-code-intelligence", exposing six tools (af_code_get_repo_map,
af_code_search_symbols, af_code_search_code, af_code_check_duplicate,
af_code_find_type_usages, af_code_validate_cross_deps).

The index is built once at startup (warm-up, logged to stderr) and reused
across tool calls; the session-scoped index is process-lived. stdout carries
ONLY JSON-RPC — never logs. Shuts down gracefully on stdin EOF.

This is a machine-facing entry point: ` + bin + ` spawns it per agent session
via --mcp-config. Operators rarely invoke it directly.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var toolSubset []string
			if s := strings.TrimSpace(tools); s != "" {
				for _, t := range strings.Split(s, ",") {
					if v := strings.TrimSpace(t); v != "" {
						toolSubset = append(toolSubset, v)
					}
				}
			}

			srv, err := mcpserver.New(mcpserver.Config{
				Root:     root,
				RepoPath: repoPath,
				Tools:    toolSubset,
				// Warm-up / lifecycle logging goes to stderr; stdout is the
				// protocol channel and must stay JSON-RPC-only.
				Logf: func(format string, args ...any) {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "[af-code-intelligence] "+format+"\n", args...)
				},
			})
			if err != nil {
				// Fail loud: a bad --root / --repo-path / --tools must not serve.
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "mcp code-intel: %v\n", err)
				return err
			}

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
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
	_ = cmd.MarkFlagRequired("root")

	// The runner authors the MCP entry with Command=os.Executable() and
	// Args=["mcp","code-intel","--root",<wpath>] (+ --repo-path/--tools when
	// set), so this flag set is the frozen contract that entry builds against.
	return cmd
}
