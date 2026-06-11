// Package mcp builds per-session MCP configuration tmpfiles and ships a
// minimal in-box MCP client.
//
// The runner uses Builder.Build to materialize a JSON config the
// provider hands to its native MCP loader. The Claude provider passes
// the path via --mcp-config; the Codex provider reads the same shape
// via config/batchWrite over JSON-RPC. Keeping the on-disk shape stable
// across providers means tests + smoke harness can roundtrip a single
// fixture against either one.
//
// Providers WITHOUT a native MCP loader (e.g. the Gemini direct harness,
// which drives generateContent over plain HTTP) instead Dial the declared
// servers themselves: Dial speaks the MCP wire protocol over the stdio
// (newline-delimited JSON-RPC subprocess) or Streamable HTTP transport,
// ListTools discovers the per-server tool surface at session start, and
// CallTool routes the model's mcp__* function calls to the live server.
//
// Per coordinator decision #10 in F.1.1 §10, configuration files are
// per-session — written under os.TempDir() with a unique prefix and
// removed by the cleanup closure when the session ends.
//
// The wire shape is the legacy TS Claude SDK Record<string,
// McpStdioServerConfig> form:
//
//	{
//	  "mcpServers": {
//	    "<name>": {
//	      "type": "stdio",
//	      "command": "...",
//	      "args": [...],
//	      "env": { ... }
//	    }
//	  }
//	}
//
// Both provider/claude/mcp.go and provider/codex/spec_translation.go
// are kept consistent with this builder.
package mcp
