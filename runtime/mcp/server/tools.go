package server

import (
	"encoding/json"
	"errors"

	"github.com/RenseiAI/donmai/afclient/codeintel"
)

// buildTools constructs the full six-tool set bound to this server's warm
// NativeRunner. Each tool mirrors 1:1 the corresponding `donmai code`
// subcommand handler in afcli/code.go — same options, same JSON output — so an
// agent that sees both surfaces gets consistent results. The enabled subset is
// selected from this set by registerTools.
//
// Tool results carry the SAME indented JSON the CLI prints (json.MarshalIndent
// with two-space indent, matching afcli's printJSON), wrapped in a single MCP
// text content item.
func (s *Server) buildTools() []*toolDef {
	r := s.runner
	root := s.root

	return []*toolDef{
		{
			name:        ToolGetRepoMap,
			description: "Repo map ranked by import centrality: most important files + their symbols. Call FIRST to orient.",
			inputSchema: schemaGetRepoMap,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					MaxFiles     int      `json:"maxFiles"`
					FilePatterns []string `json:"filePatterns"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.GetRepoMapNative(codeintel.GetRepoMapOptions{
					MaxFiles:     in.MaxFiles,
					FilePatterns: in.FilePatterns,
				})
			},
		},
		{
			name:        ToolSearchSymbols,
			description: "Search symbols by name (functions, methods, types, ...); exact names return only the exact hits.",
			inputSchema: schemaSearchSymbols,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Query       string   `json:"query"`
					MaxResults  int      `json:"maxResults"`
					Kinds       []string `json:"kinds"`
					FilePattern string   `json:"filePattern"`
					IncludeDoc  bool     `json:"includeDoc"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.SearchSymbolsNative(codeintel.SearchSymbolsOptions{
					Query:       in.Query,
					MaxResults:  in.MaxResults,
					Kinds:       in.Kinds,
					FilePattern: in.FilePattern,
					IncludeDoc:  in.IncludeDoc,
				})
			},
		},
		{
			name:        ToolSearchCode,
			description: "Keyword search over code content with code-aware tokenization (camelCase/snake_case).",
			inputSchema: schemaSearchCode,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Query      string `json:"query"`
					MaxResults int    `json:"maxResults"`
					Language   string `json:"language"`
					IncludeDoc bool   `json:"includeDoc"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.SearchCodeNative(codeintel.SearchCodeOptions{
					Query:      in.Query,
					MaxResults: in.MaxResults,
					Language:   in.Language,
					IncludeDoc: in.IncludeDoc,
				})
			},
		},
		{
			name:        ToolCheckDuplicate,
			description: "Check whether code already exists (exact or near duplicate). Pass content OR contentFile.",
			inputSchema: schemaCheckDuplicate,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Content     string `json:"content"`
					ContentFile string `json:"contentFile"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				if (in.Content == "") == (in.ContentFile == "") {
					return nil, errors.New("exactly one of content or contentFile is required")
				}
				opts := codeintel.CheckDuplicateOptions{Content: in.Content}
				if in.ContentFile != "" {
					// Confine contentFile to the served root — reject absolute
					// paths and ../ escapes so a tool call cannot read outside
					// --root (the CLI allows arbitrary paths; the agent-facing
					// MCP surface does not).
					scoped, err := resolveScopedFile(root, in.ContentFile)
					if err != nil {
						return nil, err
					}
					opts.ContentFile = scoped
				}
				return r.CheckDuplicateNative(opts)
			},
		},
		{
			name:        ToolFindTypeUsages,
			description: "Find every usage site of a named type. Call BEFORE a cross-file rename/refactor to list all sites.",
			inputSchema: schemaFindTypeUsages,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					TypeName   string `json:"typeName"`
					MaxResults int    `json:"maxResults"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.FindTypeUsagesNative(codeintel.FindTypeUsagesOptions{
					TypeName:   in.TypeName,
					MaxResults: in.MaxResults,
				})
			},
		},
		{
			name:        ToolValidateCrossDeps,
			description: "Validate monorepo cross-package imports against package.json dependency declarations.",
			inputSchema: schemaValidateCrossDeps,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Path string `json:"path"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.ValidateCrossDepsNative(codeintel.ValidateCrossDepsOptions{Path: in.Path})
			},
		},
	}
}

// ── inputSchema definitions (JSON Schema, mirroring the CLI flags) ────────────
//
// WS11: these schemas + the tool descriptions above are injected into the
// model context on every tool load, so they are a fixed per-session token tax.
// Keep every property description one line, drop schema boilerplate (e.g.
// "minimum": 0 — the engine already treats <= 0 as "use the default"), and
// never remove or rename a property: the plumbing executor, the CLI, and the
// conformance tests pass these names. TestToolSurfaceWeight_Budget enforces
// the character budget.

var schemaGetRepoMap = json.RawMessage(`{
  "type": "object",
  "properties": {
    "maxFiles": {"type": "integer", "description": "Max files (0 = default 50)"},
    "filePatterns": {"type": "array", "items": {"type": "string"}, "description": "Glob filters"}
  },
  "additionalProperties": false
}`)

var schemaSearchSymbols = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"},
    "kinds": {"type": "array", "items": {"type": "string"}, "description": "Symbol kind filter"},
    "filePattern": {"type": "string", "description": "Glob filter"},
    "includeDoc": {"type": "boolean", "description": "Return full docs (default one-line)"}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

var schemaSearchCode = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"},
    "language": {"type": "string", "description": "Language filter (go, typescript, ...)"},
    "includeDoc": {"type": "boolean", "description": "Return full docs (default one-line)"}
  },
  "required": ["query"],
  "additionalProperties": false
}`)

var schemaCheckDuplicate = json.RawMessage(`{
  "type": "object",
  "properties": {
    "content": {"type": "string", "description": "Inline content to check"},
    "contentFile": {"type": "string", "description": "Path relative to the repo root"}
  },
  "additionalProperties": false
}`)

var schemaFindTypeUsages = json.RawMessage(`{
  "type": "object",
  "properties": {
    "typeName": {"type": "string"},
    "maxResults": {"type": "integer", "description": "Max results (0 = default)"}
  },
  "required": ["typeName"],
  "additionalProperties": false
}`)

var schemaValidateCrossDeps = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Relative path scope"}
  },
  "additionalProperties": false
}`)
