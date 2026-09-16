// Package codeintelcontract owns the canonical local code-intelligence tool
// descriptors shared by transports and inert extension generation.
package codeintelcontract

import "encoding/json"

// Canonical server and tool identities.
const (
	ServerName            = "af-code-intelligence"
	ToolGetRepoMap        = "af_code_get_repo_map"
	ToolSearchSymbols     = "af_code_search_symbols"
	ToolSearchCode        = "af_code_search_code"
	ToolCheckDuplicate    = "af_code_check_duplicate"
	ToolFindTypeUsages    = "af_code_find_type_usages"
	ToolValidateCrossDeps = "af_code_validate_cross_deps"
)

// Descriptor is one immutable tool contract in existing CLI/display order.
type Descriptor struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

var descriptors = []Descriptor{
	{ToolGetRepoMap, "Repo map ranked by import centrality: most important files + their symbols. Call FIRST to orient.", json.RawMessage(`{"type":"object","properties":{"maxFiles":{"type":"integer","description":"Max files (0 = default 50)"},"filePatterns":{"type":"array","items":{"type":"string"},"description":"Glob filters"}},"additionalProperties":false}`)},
	{ToolSearchSymbols, "Search symbols by name (functions, methods, types, ...); exact names return only the exact hits.", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"maxResults":{"type":"integer","description":"Max results (0 = default)"},"kinds":{"type":"array","items":{"type":"string"},"description":"Symbol kind filter"},"filePattern":{"type":"string","description":"Glob filter"},"includeDoc":{"type":"boolean","description":"Full docs (default one-line)"}},"required":["query"],"additionalProperties":false}`)},
	{ToolSearchCode, "Keyword search over code content with code-aware tokenization (camelCase/snake_case).", json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"maxResults":{"type":"integer","description":"Max results (0 = default)"},"language":{"type":"string","description":"Language filter (go, typescript, ...)"},"includeDoc":{"type":"boolean","description":"Full docs (default one-line)"}},"required":["query"],"additionalProperties":false}`)},
	{ToolCheckDuplicate, "Check whether code already exists (exact or near duplicate). Pass content OR contentFile.", json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","description":"Inline content"},"contentFile":{"type":"string","description":"Repo-relative path"},"maxResults":{"type":"integer","description":"Duplicate sites (0 = top match only)"}},"additionalProperties":false}`)},
	{ToolFindTypeUsages, "Find every usage site of a named type. Call BEFORE a cross-file rename/refactor to list all sites.", json.RawMessage(`{"type":"object","properties":{"typeName":{"type":"string"},"maxResults":{"type":"integer","description":"Max results (0 = default)"}},"required":["typeName"],"additionalProperties":false}`)},
	{ToolValidateCrossDeps, "Validate monorepo cross-package imports against package.json dependency declarations.", json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative path scope"}},"additionalProperties":false}`)},
}

// Descriptors returns an independent deep copy.
func Descriptors() []Descriptor {
	out := make([]Descriptor, len(descriptors))
	for i, descriptor := range descriptors {
		out[i] = descriptor
		out[i].InputSchema = append(json.RawMessage(nil), descriptor.InputSchema...)
	}
	return out
}

// Names returns identities in existing CLI/display order.
func Names() []string {
	out := make([]string, len(descriptors))
	for i, descriptor := range descriptors {
		out[i] = descriptor.Name
	}
	return out
}
