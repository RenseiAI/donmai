# Code intelligence MCP contract

## Status

This document freezes the public, local stdio contract served by:

```text
donmai mcp code-intel --root <absolute-repository-root>
```

The command is a documented first-class `donmai mcp` entry point. The server is
free to use without an account and is self-contained in the `donmai` binary.

**Contract version:** `0.1.0`.

## Install and client configuration

Install `donmai` by the [documented Homebrew, Go, release-download, or
source-build route](../README.md#install). The configured command must resolve
to that installed binary; do not point a client at a build artifact that is not
on its machine.

The server needs an absolute checkout root. From the repository or worktree to
serve, verify the local journey before configuring a client:

```bash
donmai mcp code-intel --root "$(pwd -P)" --verify
```

`--verify` starts the real server in a local stdio session, sends `initialize`,
checks that `tools/list` returns the frozen six-tool profile, and calls every
tool with minimal valid arguments. It prints its human-readable summary only
after that check exits successfully. Normal server mode is different: stdout is
JSON-RPC only, while all warm-up and diagnostic logging stays on stderr.

The following configuration blocks use the same absolute root. A client should
start one server per repository or worktree; it must not rely on its own
working directory to select the index root.

### Claude Code

Add this block to a project `.mcp.json` (or an equivalent Claude Code MCP
scope):

```json
{
  "mcpServers": {
    "donmai-code-intelligence": {
      "type": "stdio",
      "command": "/absolute/path/to/donmai",
      "args": ["mcp", "code-intel", "--root", "/absolute/path/to/repository"]
    }
  }
}
```

### OpenCode

Add this block to `opencode.json`:

```json
{
  "mcp": {
    "servers": {
      "donmai-code-intelligence": {
        "type": "local",
        "command": ["/absolute/path/to/donmai", "mcp", "code-intel", "--root", "/absolute/path/to/repository"]
      }
    }
  }
}
```

### Pi

When using the Pi MCP adapter, add this block to `.pi/mcp.json` (or its global
`~/.pi/agent/mcp.json`):

```json
{
  "mcpServers": {
    "donmai-code-intelligence": {
      "command": "/absolute/path/to/donmai",
      "args": ["mcp", "code-intel", "--root", "/absolute/path/to/repository"]
    }
  }
}
```

## Transport and lifecycle

The server uses newline-delimited JSON-RPC 2.0 on stdin and stdout. Each
non-empty input line is one JSON object and each response is one JSON object
followed by a newline. Stdout contains protocol messages only; diagnostics and
index warm-up logs go to stderr. EOF on stdin is a graceful shutdown.

The supported request methods are:

- `initialize`
- `notifications/initialized` (notification; no response)
- `ping`
- `tools/list`
- `tools/call`

`initialize` advertises the requested `protocolVersion` when one is supplied,
or `2025-03-26` otherwise. Its capability-discovery result is:

```json
{
  "protocolVersion": "2025-03-26",
  "capabilities": {"tools": {}},
  "serverInfo": {"name": "af-code-intelligence", "version": "0.1.0"}
}
```

`capabilities.tools` is present to advertise tools, but does not advertise
`listChanged`. `tools/list` returns the complete enabled list in canonical
order, with no `nextCursor`. The normal invocation exposes exactly the six
tools below. The optional `--tools` flag exposes only a validated subset and is
an execution-scoping flag, not a different public profile.

## Root and repository scope

`--root` is required, absolute, existing, and a directory. The server indexes
that root rather than inheriting its working directory. This is required
because a caller's process working directory is not a reliable repository
boundary.

`--repo-path`, when present, must be an existing relative directory under
`--root`; absolute paths and traversal outside the root are rejected. It
becomes the effective index root.

A Git worktree is an independent served root. Configure the absolute worktree
path when the client edits that worktree rather than the main checkout; its
index is built and persisted for that selected root. Start separate processes
for distinct worktrees. The initial index warms at server startup and stays
warm only for the lifetime of that server process; subsequent tool calls reuse
that process-local index.

The `contentFile` argument accepted by `af_code_check_duplicate` is also
relative to the effective root. Absolute paths, traversal outside the root, and
symlinks that resolve outside the root are rejected. Indexing and type/dependency
scans do not follow symlinks outside their served root.

## Frozen tool surface

Tool names, descriptions, input property names, required fields, and the
output shapes below are the `0.1.0` contract. Every successful `tools/call`
result has one text content item. Its `text` value is an indented JSON encoding
of the output shape documented below.

### `af_code_get_repo_map`

**Description:** Repo map ranked by import centrality: most important files +
their symbols. Call FIRST to orient.

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "maxFiles": {"type": "integer", "description": "Max files (0 = default 50)"},
    "filePatterns": {"type": "array", "items": {"type": "string"}, "description": "Glob filters"}
  },
  "additionalProperties": false
}
```

`maxFiles <= 0` defaults to 50. `filePatterns` filters the returned files, not
the repository-wide graph used to calculate rank.

**Output schema:**

```text
{
  entries: Array<{
    filePath: string,
    rank: number,
    symbols: Array<{name: string, kind: string, line: integer}>
  }>,
  rootHash: string,
  files: integer
}
```

`entries` is sorted by descending rank, then ascending file path. Empty results
are `[]`, never `null`.

### `af_code_search_symbols`

**Description:** Search symbols by name (functions, methods, types, ...);
exact names return only the exact hits.

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"},
    "kinds": {"type": "array", "items": {"type": "string"}, "description": "Symbol kind filter"},
    "filePattern": {"type": "string", "description": "Glob filter"},
    "includeDoc": {"type": "boolean", "description": "Full docs (default one-line)"}
  },
  "required": ["query"],
  "additionalProperties": false
}
```

**Output schema:**

```text
Array<{
  symbol: CompactCodeSymbol,
  score: number,
  matchType: "exact" | "bm25" | "fuzzy"
} | {
  truncatedExactMatches: integer,
  hint: string
}>

CompactCodeSymbol = {
  name: string,
  kind: string,
  filePath: string,
  line: integer,
  signature?: string,
  documentation?: string,
  parentName?: string
}
```

With `includeDoc: true`, `symbol` additionally uses the full symbol projection:
`endLine?: integer`, `signature?: string`, `documentation?: string`,
`exported: boolean`, `parentName?: string`, and `language?: string`. A trailing
truncation object appears only when an exact-name result exceeds its cap.

### `af_code_search_code`

**Description:** Keyword search over code content with code-aware tokenization
(camelCase/snake_case).

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"},
    "language": {"type": "string", "description": "Language filter (go, typescript, ...)"},
    "includeDoc": {"type": "boolean", "description": "Full docs (default one-line)"}
  },
  "required": ["query"],
  "additionalProperties": false
}
```

**Output schema:**

```text
Array<{
  symbol: CompactCodeSymbol,
  score: number,
  matchType: "exact" | "bm25" | "fuzzy"
}>
```

`CompactCodeSymbol` and the `includeDoc` expansion have the same meaning as
`af_code_search_symbols`.

### `af_code_check_duplicate`

**Description:** Check whether code already exists (exact or near duplicate).
Pass content OR contentFile.

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "content": {"type": "string", "description": "Inline content"},
    "contentFile": {"type": "string", "description": "Repo-relative path"},
    "maxResults": {"type": "integer", "description": "Duplicate sites (0 = top match only)"}
  },
  "additionalProperties": false
}
```

Exactly one of `content` and `contentFile` is required semantically.
`maxResults = 0` returns the top match only; a value greater than one requests
ranked matches.

**Output schema:**

```text
{
  isDuplicate: boolean,
  matchType: "exact" | "near" | "none",
  existingId: string,
  hammingDistance: integer,
  filePath?: string,
  symbolName?: string,
  line?: integer,
  matches?: Array<{
    filePath: string,
    symbolName?: string,
    line?: integer,
    matchType: "exact" | "near",
    hammingDistance?: integer
  }>
}
```

`filePath`, `symbolName`, and `line` identify a matching file or symbol when a
match exists. `matches` is present only for a matching result requested with
`maxResults > 1`.

### `af_code_find_type_usages`

**Description:** Find every usage site of a named type. Call BEFORE a
cross-file rename/refactor to list all sites.

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "typeName": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"}
  },
  "required": ["typeName"],
  "additionalProperties": false
}
```

**Output schema:**

```text
{
  typeName: string,
  totalUsages: integer,
  usages: Array<{
    filePath: string,
    line: integer,
    context: string,
    kind: "import" | "switch_case" | "mapping_object" |
          "type_reference" | "exhaustive_check"
  }>,
  switchStatements: integer,
  mappingObjects: integer
}
```

`totalUsages` is the count before the optional `maxResults` output cap.

### `af_code_validate_cross_deps`

**Description:** Validate monorepo cross-package imports against package.json
dependency declarations.

**Input schema:**

```json
{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Relative path scope"}
  },
  "additionalProperties": false
}
```

**Output schema:**

```text
{
  valid: boolean,
  missingDeps: Array<{
    importingFile: string,
    importedPackage: string,
    packageJsonPath: string,
    line: integer
  }>,
  packagesChecked: integer,
  filesChecked: integer
}
```

## Error shape

Invalid startup flags fail before the server starts. In particular, missing or
non-absolute roots, missing/non-directory repository paths, escaping paths, and
unknown `--tools` names are process errors written to stderr.

For a JSON-RPC request with an id, protocol errors use this shape:

```json
{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"..."}}
```

The defined codes are:

| Condition | Code | Message form |
| --- | ---: | --- |
| unknown method | `-32601` | `method not found: <method>` |
| malformed `tools/call` parameters, missing tool name, unknown or disabled tool | `-32602` | request-specific text |
| cancellation before index warm-up completes | `-32000` | `server shutting down before warm-up completed` |

A recognized tool that runs but cannot satisfy its arguments or its underlying
operation returns a normal JSON-RPC result, not a protocol error:

```json
{
  "content": [{"type": "text", "text": "<operation error text>"}],
  "isError": true
}
```

There is no structured error payload in `0.1.0`. Malformed input lines are
dropped without a response, and notifications receive no response.

## Compatibility policy

The six names are the complete `0.1.0` profile. A client discovers the profile
by calling `initialize`, checking `serverInfo.name` is `af-code-intelligence`,
reading semantic `serverInfo.version`, and then calling `tools/list`.

Compatible evolution is additive only:

- A new optional input property or optional output field on an existing tool may
  ship in a later minor version. Existing fields retain their names, types, and
  meanings.
- Tool names, the server name, required input fields, and existing output fields
  are frozen. The six-tool list cannot grow within this profile.
- Renaming or removing a tool or field, changing an existing field's meaning or
  type, making an optional input required, or changing the list of six is
  breaking. It requires a semantic-version major bump in
  `initialize.result.serverInfo.version`; a new tool profile must be introduced
  under a distinct server identity rather than silently altering this list.

Consumers that depend on the frozen profile must reject an unexpected major
version or tool list before issuing calls. The `serverInfo.version` field is the
wire version signal; command/package release versions are not a substitute.

## Verification

`runtime/mcp/server/conformance_test.go` contains the contract gate. It starts
a clean subprocess through `afcli.RegisterCommands`, initializes it over real
stdio, verifies the exact ordered six-tool list, pins each description and input
schema's required fields, calls every tool, and checks the required top-level
output fields on a fixture repository.
