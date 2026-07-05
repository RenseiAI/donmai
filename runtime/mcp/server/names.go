// Package server implements the stdio MCP (Model Context Protocol) server
// that exposes the donmai code-intelligence engine (afclient/codeintel) as
// six MCP tools over newline-delimited JSON-RPC 2.0.
//
// It is the server-side counterpart to the in-repo client in the parent
// runtime/mcp package: that client is the conformance oracle — it Dials this
// server over stdio, negotiates the handshake, lists the tools, and routes a
// model's mcp__* calls to them. The wire framing here mirrors what stdio.go +
// client.go expect (one JSON object per line, responses correlated by id),
// and what the claude/amp CLIs consume via --mcp-config
// (provider/harness/clijsonl/mcp.go) and the gemini direct harness bridges
// through (provider/harness/gemini/mcp.go).
//
// A single long-lived process holds ONE codeintel.NativeRunner (the Wave-1
// warm-cache design): the index is built once during process init (warm-up)
// and every tools/call reuses it without re-walking the tree. The engine is
// root-scoped via NewNativeRunner(root); the server never serves paths
// outside --root.
package server

// ServerName is the MCP serverInfo.name this server advertises in the
// initialize handshake. It is frozen: the fully-qualified tool prefix
// mcp__af-code-intelligence__af_code_* that autonomous agents call (and the
// agent/types.go MCPToolNames example) is derived from it, so it MUST NOT
// change without updating every consumer.
const ServerName = "af-code-intelligence"

// serverVersion is the serverInfo.version reported in initialize. The in-repo
// client ignores it; other consumers (claude/amp CLIs) surface it for
// diagnostics only.
const serverVersion = "0.1.0"

// The six code-intelligence tool names, frozen by the wire contract. Each is
// backed 1:1 by a codeintel.NativeRunner method and mirrors the corresponding
// `donmai code <subcommand>` handler in afcli/code.go.
const (
	ToolGetRepoMap        = "af_code_get_repo_map"
	ToolSearchSymbols     = "af_code_search_symbols"
	ToolSearchCode        = "af_code_search_code"
	ToolCheckDuplicate    = "af_code_check_duplicate"
	ToolFindTypeUsages    = "af_code_find_type_usages"
	ToolValidateCrossDeps = "af_code_validate_cross_deps"
)

// allToolNames returns the six tool names in canonical (contract) order. The
// order is authoritative for tools/list determinism.
func allToolNames() []string {
	return []string{
		ToolGetRepoMap,
		ToolSearchSymbols,
		ToolSearchCode,
		ToolCheckDuplicate,
		ToolFindTypeUsages,
		ToolValidateCrossDeps,
	}
}
