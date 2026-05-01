// Package mcp builds per-session MCP stdio configuration tmpfiles.
//
// The runner uses Builder.Build to materialize a JSON config the
// provider hands to its native MCP loader. The Claude provider passes
// the path via --mcp-config; the Codex provider reads the same shape
// via config/batchWrite over JSON-RPC. Keeping the on-disk shape stable
// across providers means tests + smoke harness can roundtrip a single
// fixture against either one.
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
