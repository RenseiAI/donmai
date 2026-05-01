# `runtime/mcp/`

Per-session MCP stdio configuration tmpfile builder.

## What it does

```go
b := mcp.NewBuilder()
path, cleanup, err := b.Build(spec.MCPServers)
defer cleanup()
// path is the absolute file path Claude CLI's --mcp-config / Codex
// app-server's config/batchWrite expects.
```

## Wire shape

```json
{
  "mcpServers": {
    "af-linear": {
      "type": "stdio",
      "command": "/usr/local/bin/af",
      "args": ["linear-mcp"],
      "env": { "LINEAR_API_KEY": "..." }
    },
    "af-code": {
      "type": "stdio",
      "command": "/usr/local/bin/af",
      "args": ["code-mcp"]
    }
  }
}
```

This shape is consistent with `provider/claude/mcp.go` (it produces the exact same JSON via its own writer; `runtime/mcp` is the cross-provider builder used by the runner). Codex's `provider/codex/spec_translation.go` reads the same array form via `mcpServersConfig`.

Per coordinator decision #10 (F.1.1 §10): per-session tmpfile, not host-shared. The cleanup closure removes the file when the session ends; the runner defers it.

## Tests

`builder_test.go` covers: empty input → empty path + noop cleanup, full roundtrip, cleanup actually deletes (idempotent), parallel sessions get unique paths, validation rejects empty Name/Command, defensive copy of Args/Env.
