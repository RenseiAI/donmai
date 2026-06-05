// Package codeintel provides a shell-out bridge to the TypeScript
// @donmai/cli (donmai-code).
//
// # Architectural choice: shell-out bridge (Phase D parity)
//
// The tree-sitter Go bindings (go-tree-sitter) were evaluated but rejected for
// this phase because:
//
//  1. CGo + native deps make CI slower and cross-compilation fragile.
//  2. The AC requires byte-identical index format with TS readers — easiest to
//     guarantee when TS owns the indexing entirely.
//  3. Phase D goal is parity, not re-implementation.
//
// This package shells out to `pnpm donmai-code` (resolving via PATH or
// DONMAI_CODE_BIN env var) and returns the parsed JSON output.
//
// A future issue (post-Wave 4) can replace the shell-out with native Go
// tree-sitter after parity is verified end-to-end.
//
// # Binary resolution (PATH portability)
//
// The binary is resolved in this order:
//  1. DONMAI_CODE_BIN env var (legacy: AGENTFACTORY_CODE_BIN) — explicit override for non-monorepo users
//  2. `donmai-code` on PATH (installed via `npm install -g @donmai/cli`)
//  3. `pnpm donmai-code` via pnpm run in the current working directory (monorepo dev)
//
// If none of those resolve, every command returns an ErrNotAvailable error with
// clear installation instructions. The caller surfaces this gracefully rather
// than crashing.
package codeintel

// SymbolKind enumerates the kinds of code symbols the extractors recognise.
// Values are the string literals used in the TS @donmai/code-intelligence
// package (types.ts — SymbolKindSchema) and serialised into index.json
// verbatim. Do NOT change these strings without also updating the TS side.
type SymbolKind string

// Symbol kind constants — string values must match the TS SymbolKindSchema in
// donmai-libraries/packages/code-intelligence/src/types.ts exactly.
const (
	KindFunction  SymbolKind = "function"
	KindClass     SymbolKind = "class"
	KindInterface SymbolKind = "interface"
	KindType      SymbolKind = "type"
	KindVariable  SymbolKind = "variable"
	KindMethod    SymbolKind = "method"
	KindProperty  SymbolKind = "property"
	KindImport    SymbolKind = "import"
	KindExport    SymbolKind = "export"
	KindEnum      SymbolKind = "enum"
	KindStruct    SymbolKind = "struct"
	KindTrait     SymbolKind = "trait"
	KindImpl      SymbolKind = "impl"
	KindMacro     SymbolKind = "macro"
	KindDecorator SymbolKind = "decorator"
	KindModule    SymbolKind = "module"
)

// CodeSymbol mirrors the TS CodeSymbol type from types.ts.
// JSON field names must match the TS serialisation exactly for index.json
// round-trip compatibility.
type CodeSymbol struct {
	Name          string     `json:"name"`
	Kind          SymbolKind `json:"kind"`
	FilePath      string     `json:"filePath"`
	Line          int        `json:"line"`
	EndLine       *int       `json:"endLine,omitempty"`
	Signature     string     `json:"signature,omitempty"`
	Documentation string     `json:"documentation,omitempty"`
	Exported      bool       `json:"exported"`
	ParentName    string     `json:"parentName,omitempty"`
	Language      string     `json:"language,omitempty"`
}

// FileAST holds the extraction result for a single file. It is an in-memory
// intermediate and is NOT persisted directly; see FileIndex for what goes to
// disk.
type FileAST struct {
	FilePath string
	Language string
	Symbols  []CodeSymbol
	Imports  []string
	Exports  []string
}

// FileIndex is the per-file node persisted to index.json.
// Schema matches the TS FileIndexSchema in types.ts.
type FileIndex struct {
	FilePath    string       `json:"filePath"`
	GitHash     string       `json:"gitHash"`
	Symbols     []CodeSymbol `json:"symbols"`
	LastIndexed int64        `json:"lastIndexed"` // Unix ms
}

// IndexMetadata is the top-level metadata block persisted to index.json.
// Schema matches the TS IndexMetadataSchema in types.ts.
type IndexMetadata struct {
	Version      int      `json:"version"`
	RootHash     string   `json:"rootHash"`
	TotalFiles   int      `json:"totalFiles"`
	TotalSymbols int      `json:"totalSymbols"`
	LastUpdated  int64    `json:"lastUpdated"` // Unix ms
	Languages    []string `json:"languages"`
}

// IndexFile is the top-level structure of .donmai/code-index/index.json.
// The TS IncrementalIndexer.save() writes:
//
//	{ "files": { "<filePath>": FileIndex, ... }, "rootHash": "<hash>" }
type IndexFile struct {
	Files    map[string]FileIndex `json:"files"`
	RootHash string               `json:"rootHash"`
}

// RepoMapSymbol is the trimmed symbol shape inside a RepoMapEntry.
type RepoMapSymbol struct {
	Name string     `json:"name"`
	Kind SymbolKind `json:"kind"`
	Line int        `json:"line"`
}

// RepoMapEntry mirrors the TS RepoMapEntrySchema.
type RepoMapEntry struct {
	FilePath string          `json:"filePath"`
	Rank     float64         `json:"rank"`
	Symbols  []RepoMapSymbol `json:"symbols"`
}
